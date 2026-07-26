package drpc

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Per-stream flow control (PROTOCOL.md §4.2, reliable mode only).
//
// Without it, a receiver whose per-stream buffer is full can only block its
// Handle call, and since a reliable adapter delivers synchronously from one
// read loop, one slow consumer stalls every call on the channel — the
// head-of-line blocking gRPC avoids with per-stream HTTP/2 windows.
//
// The mechanism mirrors HTTP/2, counting messages rather than bytes:
//
//   - a receiver advertises its buffer as an initial window — on the OPEN
//     (client) and on the creation-ack H (server);
//   - until that advertisement arrives, a sender assumes wInit, the protocol's
//     initial window, exactly as HTTP/2 senders assume 65535 bytes before a
//     SETTINGS frame lands. Without this a client-streaming burst could empty
//     itself onto the wire before the ack it would have been paced by;
//   - the advertisement is authoritative: it replaces the assumption, counted
//     against what the sender has already sent (so a window smaller than the
//     assumption simply parks the sender until the receiver drains);
//   - WINDOW frames (§7) then add credit as the receiving application consumes
//     messages, batched at half the window;
//   - an advertisement of 0 means the peer does no flow control: unlimited,
//     which is the pre-v1.1 behavior and what unreliable mode always uses
//     (there a full buffer drops by policy, §4.2, and blocking never arises).
//
// The receiver's blocking enqueue stays as the safety net for the window in
// which a sender is still running on the assumption.

// wInit is the initial per-stream window a sender assumes before the peer's
// advertisement arrives (PROTOCOL.md §4.2, Appendix B) — the same value as the
// default rx buffer, so the assumption is exact for a default receiver.
const wInit uint32 = defaultRxBuffer

// flowSender is the sending half: how much the peer has allowed, how much has
// been sent, and a parking spot for the difference.
type flowSender struct {
	mu       sync.Mutex
	on       bool
	observed bool
	granted  int64
	sent     int64
	waiters  chan struct{} // closed on every grant; replaced under mu
}

// assume starts flow control on the protocol's initial window, before the
// peer has said anything.
func (f *flowSender) assume(window uint32) {
	if window == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observed || f.on {
		return
	}
	f.on = true
	f.granted = int64(window)
}

// observe adopts the peer's advertised window. It is authoritative and
// replaces any assumption; 0 means the peer does no flow control.
func (f *flowSender) observe(window uint32) {
	f.mu.Lock()
	if f.observed {
		f.mu.Unlock()
		return
	}
	f.observed = true
	if window == 0 {
		f.on = false
	} else {
		f.on = true
		f.granted = int64(window)
	}
	w := f.waiters
	f.waiters = nil
	f.mu.Unlock()
	if w != nil {
		close(w)
	}
}

// grant adds credit and wakes anyone parked. A grant never turns flow control
// ON by itself: only an advertisement does (assume/observe). Otherwise a
// stray, duplicated or injected WINDOW frame could park a sender that was
// never flow-controlled — free of charge on a datagram channel (§4.2, §15).
func (f *flowSender) grant(n uint32) {
	if n == 0 {
		return
	}
	f.mu.Lock()
	if !f.on {
		f.mu.Unlock()
		return
	}
	f.granted = saturateAdd(f.granted, int64(n))
	w := f.waiters
	f.waiters = nil
	f.mu.Unlock()
	if w != nil {
		close(w)
	}
}

// undo returns one message of credit: the frame it was taken for never
// reached the wire (a synchronous adapter refusal, §4.4). Without this a
// handler that ignores such errors leaks its whole window and parks forever.
func (f *flowSender) undo() {
	f.mu.Lock()
	if f.sent > 0 {
		f.sent--
	}
	w := f.waiters
	f.waiters = nil
	f.mu.Unlock()
	if w != nil {
		close(w)
	}
}

// saturateAdd keeps the credit accumulator from wrapping on a hostile or
// buggy peer's grants.
func saturateAdd(a, b int64) int64 {
	if c := a + b; c >= a {
		return c
	}
	return int64(^uint64(0) >> 1)
}

// acquire consumes one message of credit, parking until there is some.
// onStall is called once, at the moment it first parks — observable while the
// sender is still blocked, which is what a stall counter must report. It
// returns the reason it gave up: the call's own ctx (deadline, cancellation,
// teardown) or the call ending. stalled says whether it parked at all.
func (f *flowSender) acquire(ctx context.Context, done <-chan struct{}, stall time.Duration, onStall func()) (stalled bool, err error) {
	var timer *time.Timer
	var expired <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		f.mu.Lock()
		if !f.on || f.sent < f.granted {
			f.sent++
			f.mu.Unlock()
			return stalled, nil
		}
		if f.waiters == nil {
			f.waiters = make(chan struct{})
		}
		w := f.waiters
		f.mu.Unlock()

		if !stalled {
			stalled = true
			if stall > 0 {
				// A parked sender needs its own bound: reliable mode runs no
				// protocol timers, and the park happens before the adapter's
				// write path, so nothing else would ever break it (§4.2).
				timer = time.NewTimer(stall)
				expired = timer.C
			}
			if onStall != nil {
				onStall()
			}
		}
		select {
		case <-w:
		case <-done:
			return stalled, errCallEnded
		case <-ctx.Done():
			return stalled, ctxErr(ctx)
		case <-expired:
			return stalled, status.Errorf(codes.Unavailable,
				"drpc: flow-control stall: the peer granted no credit for %v", stall)
		}
	}
}

// release wakes every parked sender; the call is over.
func (f *flowSender) release() {
	f.mu.Lock()
	f.on = false
	w := f.waiters
	f.waiters = nil
	f.mu.Unlock()
	if w != nil {
		close(w)
	}
}

// flowReceiver is the receiving half: it counts messages the application has
// consumed and says when to send a grant. Grants are batched at half the
// window, as HTTP/2 stacks do, so a steady stream costs one small frame per
// window/2 messages.
type flowReceiver struct {
	mu      sync.Mutex
	on      bool
	window  uint32
	pending uint32
}

func (f *flowReceiver) enable(window uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on = window > 0
	f.window = window
}

// active reports whether this side is granting credit, i.e. whether the peer
// is expected to respect a window.
func (f *flowReceiver) active() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on
}

// consumed reports that n messages left the buffer and returns the credit to
// grant now (0 = nothing to send yet).
func (f *flowReceiver) consumed(n uint32) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.on {
		return 0
	}
	f.pending += n
	if f.pending == 0 {
		return 0
	}
	if f.pending*2 < f.window {
		return 0
	}
	grant := f.pending
	f.pending = 0
	return grant
}
