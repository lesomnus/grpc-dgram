package drpc

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Flow control (PROTOCOL.md §4.2, reliable mode only): a per-stream window
// and, beside it, a per-peer connection window.
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
//
// The connection window (§4.2.1, §15) bounds what one peer can pin across all
// of its calls, as RFC 9113 §6.9.1's connection window does: a data frame
// needs one credit from its stream window AND one from the peer's connection
// window. It is never advertised — every sender assumes wConn per peer and
// the peer's first per-stream advertisement settles it (confirm): > 0 keeps
// the assumption, 0 turns it off. WINDOW sid=0 adds credit; a receiver
// returns one credit for every data frame it received once that frame stops
// occupying a buffer (peerFlowRx).

// wInit is the initial per-stream window a sender assumes before the peer's
// advertisement arrives (PROTOCOL.md §4.2, Appendix B) — the same value as the
// default rx buffer, so the assumption is exact for a default receiver.
const wInit uint32 = defaultRxBuffer

// wConn is the connection window a sender assumes per peer before any sid-0
// grant (PROTOCOL.md §4.2.1, §10.1, Appendix B), and the floor of
// Limits.MaxPeerWindow for the same reason wInit floors the rx buffer: a
// receiver holding less than a sender assumes is overrun by a conforming
// sender. A fixed protocol constant, 32 × wInit.
const wConn uint32 = 1024

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

// confirm settles a connection window by the peer's first per-stream
// advertisement (§4.2.1): 0 means the peer does no flow control and turns it
// OFF; anything else confirms the assumption as it stands. Unlike observe it
// keeps the credit already granted — a sid-0 grant that raced ahead of the
// settle (the peer's raise rides right behind its advertisement) must not be
// clobbered by a replace. Once only, like observe. A sender that was never
// assumed stays off: an advertisement is not a grant, and nothing was
// assumed that could be confirmed.
func (f *flowSender) confirm(window uint32) {
	f.mu.Lock()
	if f.observed {
		f.mu.Unlock()
		return
	}
	f.observed = true
	if window == 0 {
		f.on = false
	}
	w := f.waiters
	f.waiters = nil
	f.mu.Unlock()
	if w != nil {
		close(w)
	}
}

// reassume restarts a connection window from scratch for a new peer
// incarnation (§4.2.1, §10.6): assumed at window, unsettled, nothing sent.
// A server that restarted on a surviving channel counts from zero, so the
// cumulative sent count and any credit of the dead incarnation would never
// line up with its grants again — a forever-park. Anyone parked is woken to
// re-race on the fresh credit.
func (f *flowSender) reassume(window uint32) {
	f.mu.Lock()
	f.on = window > 0
	f.observed = false
	f.granted = int64(window)
	f.sent = 0
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
// reached the wire (a synchronous adapter refusal, §4.4, or a call that ended
// between taking the credit and transmitting). Without this a handler that
// ignores such errors leaks its whole window and parks forever — and on the
// connection window, shared by every call to the peer and cumulative for the
// incarnation's life, each such leak is a permanent shrink (§4.2.1).
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
		w, ok := f.tryAcquire()
		if ok {
			return stalled, nil
		}

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

// tryAcquire consumes one message of credit if there is some, and otherwise
// hands back the channel a grant will close.
func (f *flowSender) tryAcquire() (wait <-chan struct{}, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.on || f.sent < f.granted {
		f.sent++
		return nil, true
	}
	if f.waiters == nil {
		f.waiters = make(chan struct{})
	}
	return f.waiters, false
}

// empty reports, taking nothing, whether a send would park here right now:
// flow control is on and the credit is spent. acquire2 asks the connection
// window this when the stream window is short, so that a park short on both
// is reported as the connection one (§4.2.1, §14).
func (f *flowSender) empty() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on && f.sent >= f.granted
}

// acquire2 consumes one message of credit from the stream window AND one
// from the peer's connection window (§4.2.1), parking until both are there.
// A nil conn means no connection window (unreliable mode).
//
// Stream credit is taken first; if the connection is then short the stream
// credit is refunded (undo) before parking. The order is load-bearing:
// connection-first would let streams parked on their own window hoard the
// shared budget until every stream parks — a mutual T_stall. Never holding
// one credit while parked on the other is what keeps a stuck stream from
// starving the healthy ones, and the two mutexes are never nested.
//
// A call that has already ended spends nothing: once both credits are taken
// the fast path re-checks done and refunds both, so a dead call cannot spend
// the connection window — shared by every call to the peer and cumulative
// for the incarnation's life — on a frame that will never go out. The
// callers refund at their own late exits for the same reason.
//
// One T_stall timer, armed at the first park, bounds the whole wait across
// both windows: §10.1 makes T_stall the longest a send may wait for credit,
// and two budgets would silently make it 2 × T_stall. onStall is called once,
// at the first park, with peer = true when the connection window is the one
// that is empty — and when both are, since a sender short on both waits on
// the peer's whole budget, not on one consumer (§14) — the caller records
// that for its resume event. On expiry the error names the window by the
// same rule.
func acquire2(stream, conn *flowSender, ctx context.Context, done <-chan struct{}, stall time.Duration, onStall func(peer bool)) error {
	var timer *time.Timer
	var expired <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	stalled := false
	for {
		w, ok := stream.tryAcquire()
		peer := false
		if ok && conn != nil {
			if cw, cok := conn.tryAcquire(); !cok {
				stream.undo()
				w, ok, peer = cw, false, true
			}
		}
		if ok {
			select {
			case <-done:
				// Ended between the caller's own check and here: nothing
				// will be sent, so nothing may stay spent.
				stream.undo()
				if conn != nil {
					conn.undo()
				}
				return errCallEnded
			default:
			}
			return nil
		}
		if !peer && conn != nil && conn.empty() {
			peer = true // short on both: the connection window is the one to name
		}

		if !stalled {
			stalled = true
			if stall > 0 {
				timer = time.NewTimer(stall)
				expired = timer.C
			}
			if onStall != nil {
				onStall(peer)
			}
		}
		select {
		case <-w:
		case <-done:
			return errCallEnded
		case <-ctx.Done():
			return ctxErr(ctx)
		case <-expired:
			if peer {
				return status.Errorf(codes.Unavailable,
					"drpc: flow-control stall: the peer granted no connection credit for %v", stall)
			}
			return status.Errorf(codes.Unavailable,
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

// senderState is a connection sender's position — on, settled, granted and
// sent — carried across its container's eviction (peerFlowRx.stash, §9.4,
// §15) so a recreated container continues where it left off.
type senderState struct {
	on, observed  bool
	granted, sent int64
}

func (f *flowSender) state() senderState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return senderState{on: f.on, observed: f.observed, granted: f.granted, sent: f.sent}
}

// restore continues a fresh sender from a saved position; nothing is parked
// on a fresh sender, so there is no one to wake.
func (f *flowSender) restore(st senderState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on, f.observed, f.granted, f.sent = st.on, st.observed, st.granted, st.sent
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

// peerFlowRx is the receiving half of the connection window (§4.2.1, §15):
// one per transport peer on the server, one per Conn on the client. It is a
// physical ledger — outstanding counts the peer's data frames sitting in this
// endpoint's buffers, pending the credit retired and not yet granted — so junk
// cannot desync it: only a frame this ledger admitted can raise outstanding,
// and the bound is enforced on outstanding, never on what was granted.
//
// Every reliable-mode data frame received returns exactly one credit once it
// stops occupying a buffer: consumed, discarded at its call's end, or never
// buffered at all (off-shape, overrun-refused, RESET-drawn, tombstone drop).
// Grants are batched at half the window like flowReceiver's, plus the
// starvation clause the §4.2.1 MUST requires here: with stuck consumers
// pinning most of the window, pending may never reach half of it while the
// sender is out of credit, so a grant also fires whenever outstanding +
// pending reaches the window — whatever is pending is everything the sender
// could still be waiting for.
//
// outstanding is one number: it is the bound, and the memory it bounds is
// pinned by the transport peer whatever incarnation sent it. pending is per
// incarnation, because a grant is addressed to one (peer_epoch, §6.1): two
// incarnations can coexist on one key — a client restarted at the same
// address on a datagram channel forced reliable, where no DisconnectPeer
// fires — and credit the live one's frames returned, batched into a grant
// addressed to the dead one because its finished call tipped the batch, would
// be dropped by the client and lost for good: a permanent shrink of the live
// sender. So each incarnation is granted exactly what its own frames returned.
// The starvation clause reads the shared outstanding against one incarnation's
// pending, which can only fire early, never late.
type peerFlowRx struct {
	mu          sync.Mutex
	window      uint32            // MaxPeerWindow; 0 = off (unreliable mode)
	outstanding uint32            // admitted − retired: the peer's frames in our buffers
	pending     map[uint32]uint32 // retired − granted, per incarnation (epoch)
	raised      bool              // the once-per-incarnation raise above wConn was sent

	// The sending half of the containers the MaxDeadPeers cap evicted (§9.4,
	// §15), by client epoch, in eviction order, at most evictCap of them.
	// An evicted incarnation may be idle rather than dead — a client holding
	// several Conns on one socket — and a recreated container that started
	// over at wConn would be under-credited against a client ledger that
	// already raised it, with no grant cadence able to reach it (§4.2.1
	// Raise: a forever-park), while repeating the raise would over-credit
	// the client. So the position is kept, bounded like the containers are,
	// and a grant addressed to an evicted incarnation still lands on it, as
	// does the credit of a RESET-drawn data frame it sent. Past the cap the
	// oldest is dropped, credit held back for it included, and that
	// incarnation starts over at wConn as a new one would — raised again,
	// which over-credits the client (§16).
	evicted      map[uint32]*evictedSender
	evictedOrder []uint32
	evictCap     int
}

// evictedSender is one held sender position: its credit and whether this
// side's raise already went to it.
type evictedSender struct {
	sender senderState
	raised bool
}

// enable sizes the ledger. evictCap is how many evicted senders it keeps
// (server: MaxDeadPeers; the client evicts nothing).
func (p *peerFlowRx) enable(window uint32, evictCap int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.window = window
	p.evictCap = evictCap
}

// active reports whether this side bounds the peer, i.e. whether a data
// frame must pass admit before it is buffered.
func (p *peerFlowRx) active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.window > 0
}

// admit charges one frame about to be buffered. It refuses — false, nothing
// charged — when the frame would take outstanding past the window: that is
// the overrun the receiver fails the offending call INTERNAL for (§4.2, §15).
// Off, everything is admitted and nothing counted.
func (p *peerFlowRx) admit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window == 0 {
		return true
	}
	if p.outstanding >= p.window {
		return false
	}
	p.outstanding++
	return true
}

// retire reports that n admitted frames of incarnation epoch stopped
// occupying a buffer and returns the credit to grant it now on sid 0 (0 =
// nothing to send yet). credit = false retires without returning anything:
// the frames came from a server incarnation the client has moved past, whose
// calls are RESET-failed anyway (§10.6), so their credit has no one to go to.
func (p *peerFlowRx) retire(epoch, n uint32, credit bool) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window == 0 {
		return 0
	}
	p.outstanding -= min(n, p.outstanding)
	if !credit {
		return 0
	}
	p.holdLocked(epoch, n)
	return p.dueLocked(epoch)
}

// unadmitted returns the credit of n frames incarnation epoch sent that were
// never admitted — a data frame for an unknown, finished or tombstoned sid,
// which draws a RESET (§9.3, §10.6) — without touching outstanding: they
// never occupied a buffer, but the sender spent credit on them and a window
// that never gets it back is a permanent shrink. Same batching as retire.
func (p *peerFlowRx) unadmitted(epoch, n uint32) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window == 0 {
		return 0
	}
	p.holdLocked(epoch, n)
	return p.dueLocked(epoch)
}

// unadmittedEvicted is unadmitted for an incarnation whose container the
// MaxDeadPeers cap evicted and whose sender position this ledger holds
// (§9.4): a data frame it still had in flight for a call this side has
// finished draws its RESET like any other, and its credit goes back to that
// incarnation — the stash keeps the server→client direction exact, and this
// keeps the other one. Held nowhere, nothing is held back: junk creates no
// state, and a dropped position takes its held-back credit with it.
func (p *peerFlowRx) unadmittedEvicted(epoch, n uint32) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window == 0 || p.evicted[epoch] == nil {
		return 0
	}
	p.holdLocked(epoch, n)
	return p.dueLocked(epoch)
}

// holdLocked holds back n retired credits for incarnation epoch. Caller
// holds mu.
func (p *peerFlowRx) holdLocked(epoch, n uint32) {
	if p.pending == nil {
		p.pending = map[uint32]uint32{}
	}
	p.pending[epoch] = uint32(min(uint64(p.pending[epoch])+uint64(n), uint64(^uint32(0))))
}

// dueLocked applies the grant rule to one incarnation's held-back credit:
// half the window, or the starvation clause against the shared outstanding.
// Caller holds mu.
func (p *peerFlowRx) dueLocked(epoch uint32) uint32 {
	pending := p.pending[epoch]
	if pending == 0 {
		return 0
	}
	if uint64(pending)*2 < uint64(p.window) && uint64(p.outstanding)+uint64(pending) < uint64(p.window) {
		return 0
	}
	delete(p.pending, epoch)
	return pending
}

// raise returns the once-per-peer-incarnation sid-0 grant that lifts the
// sender's assumed wConn to this receiver's window (§4.2.1): window − wConn,
// exactly once; 0 when there is nothing to raise by, and 0 ever after. It is
// a MUST, not an optimisation: this side's grant cadence is computed against
// its own window, so a sender left at wConn against a larger receiver would
// park before any batched grant fired.
func (p *peerFlowRx) raise() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.raised {
		return 0
	}
	p.raised = true
	return p.excessLocked()
}

// excess is what a raise lifts the sender by — window − wConn when positive
// — without the once-only latch: on the server the ledger is per transport
// peer while the raise is per client incarnation (§4.2.1, §15), so the
// container keeps the latch and asks the ledger only for the amount.
func (p *peerFlowRx) excess() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.excessLocked()
}

func (p *peerFlowRx) excessLocked() uint32 {
	if p.window <= wConn {
		return 0
	}
	return p.window - wConn
}

// renew makes the raise due again: the peer is a new incarnation, whose
// sender starts over at wConn and has never seen this receiver's window
// (§4.2.1). The ledger itself carries over — outstanding still counts the
// dead incarnation's frames until they drain, uncredited — but whatever was
// held back for the old incarnation has no one left to go to. Client only:
// a Conn faces one server incarnation at a time.
func (p *peerFlowRx) renew() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.raised = false
	p.pending = nil
}

// stash keeps the sender position of a container the MaxDeadPeers cap is
// evicting (see the field comment). Whatever the ledger holds back for that
// incarnation stays with it.
func (p *peerFlowRx) stash(epoch uint32, st senderState, raised bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.evictCap <= 0 {
		delete(p.pending, epoch)
		return
	}
	if p.evicted == nil {
		p.evicted = map[uint32]*evictedSender{}
	}
	if _, held := p.evicted[epoch]; !held {
		p.evictedOrder = append(p.evictedOrder, epoch)
	}
	p.evicted[epoch] = &evictedSender{sender: st, raised: raised}
	for len(p.evictedOrder) > p.evictCap {
		oldest := p.evictedOrder[0]
		p.evictedOrder = p.evictedOrder[1:]
		delete(p.evicted, oldest)
		delete(p.pending, oldest)
	}
}

// unstash hands back the held position of an incarnation whose container is
// being recreated, if it is still held.
func (p *peerFlowRx) unstash(epoch uint32) (st senderState, raised, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.evicted[epoch]
	if e == nil {
		return senderState{}, false, false
	}
	delete(p.evicted, epoch)
	for i, k := range p.evictedOrder {
		if k == epoch {
			p.evictedOrder = append(p.evictedOrder[:i], p.evictedOrder[i+1:]...)
			break
		}
	}
	return e.sender, e.raised, true
}

// creditEvicted applies a sid-0 grant addressed to an evicted incarnation to
// its held position: the client is returning credit for frames that were in
// flight or buffered at the eviction. Same rule as flowSender.grant — it
// never enables — and a grant for an incarnation held nowhere is dropped.
func (p *peerFlowRx) creditEvicted(epoch, n uint32) {
	if n == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.evicted[epoch]; e != nil && e.sender.on {
		e.sender.granted = saturateAdd(e.sender.granted, int64(n))
	}
}
