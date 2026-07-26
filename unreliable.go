package drpc

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file holds the client-side unreliable-mode machinery: control-frame
// retransmission, peer liveness, stream probes, client tombstones, and the
// coarse sweep loop that drives them (PROTOCOL.md §9-§10, Appendix C).

func nowNano() int64 { return time.Now().UnixNano() }

// clientTomb remembers a finished call for TTL_tomb: stragglers for it are
// dropped, and a pending abort keeps retransmitting under its obligation
// until a matching T, a RESET, or expiry (PROTOCOL.md §9.2, §10.3).
type clientTomb struct {
	expire time.Time
	abort  *Frame // nil once the obligation is cleared
	retxAt time.Time
	ival   time.Duration
}

// sweeper drives periodic work while there is any; it stops itself when idle
// and is kicked back to life by state mutations (PROTOCOL.md Appendix C).
// A closed quit channel terminates the running loop at once — Conn.Close /
// Server.Stop use it to reclaim the goroutine immediately instead of waiting
// for the last tombstone to expire.
type sweeper struct {
	mu       sync.Mutex
	on       bool
	stopped  bool
	quit     chan struct{}
	quitOnce sync.Once
}

func newSweeper() sweeper { return sweeper{quit: make(chan struct{})} }

// kick starts run in a goroutine unless one is already running or the sweeper
// has been stopped.
func (w *sweeper) kick(run func()) {
	w.mu.Lock()
	if !w.on && !w.stopped {
		w.on = true
		go run()
	}
	w.mu.Unlock()
}

// idle marks the sweeper stopped if hasWork still reports none; it returns
// true when the loop should exit. The hasWork re-check under w.mu closes the
// race against a concurrent kick.
func (w *sweeper) idle(hasWork func() bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if hasWork() {
		return false
	}
	w.on = false
	return true
}

// stop terminates the sweeper loop and prevents future kicks.
func (w *sweeper) stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	w.quitOnce.Do(func() { close(w.quit) })
}

// ---------------------------------------------------------------------------
// clientStream hooks
// ---------------------------------------------------------------------------

// noteValidatedRx runs for every validated server frame of this stream:
// accepted or dedup-dropped (PROTOCOL.md §9.1). It refreshes the idle clocks
// and clears the OPEN retransmission obligation.
//
// Which frames end that obligation differs by call type (§10.3). For a
// streaming call any server frame does: the creation ack proves the call
// exists, and the stream has its own recovery afterwards. A unary call has no
// other recovery — its OPEN retransmission is what recovers a lost terminal
// through the server's tombstone — so only the terminal stops it. Otherwise a
// handler that flushes a header would silently halve every client's loss
// tolerance.
func (s *clientStream) noteValidatedRx(f *Frame) {
	n := nowNano()
	s.lastRx.Store(n)
	s.conn.lastRx.Store(n)

	if !s.clientStreams && !s.serverStreams && !f.isTerminal() {
		return
	}
	s.txMu.Lock()
	s.retxOpen = nil
	s.txMu.Unlock()
}

// transmit sends a non-probe frame, feeding the tx idle clocks.
func (s *clientStream) transmit(ctx context.Context, f *Frame) error {
	n := nowNano()
	s.lastTx.Store(n)
	s.conn.lastTx.Store(n)
	return s.conn.tx.Handle(ctx, f)
}

// scheduleRetxLocked (re)arms the stream's retransmission timer; txMu held.
// Each control event starts a fresh RTI schedule (PROTOCOL.md §10.3) — a
// half-close must not inherit the OPEN's backed-off cadence.
func (s *clientStream) scheduleRetxLocked() {
	if s.conn.mode.reliable {
		return
	}
	s.retxIval = s.conn.mode.timing.Retransmit
	s.retxAt = time.Now().Add(s.retxIval)
}

// sweepRetx returns the control frames due for retransmission and advances
// the backoff (×2, capped at T_probe). PROTOCOL.md §10.3.
func (s *clientStream) sweepRetx(now time.Time, cap time.Duration) []*Frame {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	if s.retxAt.IsZero() || now.Before(s.retxAt) {
		return nil
	}
	var out []*Frame
	if s.retxOpen != nil {
		out = append(out, s.retxOpen)
	}
	if s.retxClose != nil {
		out = append(out, s.retxClose)
	}
	if len(out) == 0 {
		s.retxAt = time.Time{}
		return nil
	}
	s.retxIval = min(s.retxIval*2, cap)
	s.retxAt = now.Add(s.retxIval)
	return out
}

// probeDue emits a stream probe when both idle clocks passed T_probe
// (PROTOCOL.md §10.5). Probes reset neither idle clock.
func (s *clientStream) probeDue(now time.Time, probe time.Duration) *Frame {
	n := now.UnixNano()
	p := int64(probe)
	if n-s.lastRx.Load() < p || n-s.lastTx.Load() < p || n-s.lastProbe.Load() < p {
		return nil
	}
	s.lastProbe.Store(n)
	f := &Frame{}
	f.SetEpoch(s.conn.epoch)
	f.SetSid(s.sid)
	f.SetFlags(FlagPing)
	return f
}

// ---------------------------------------------------------------------------
// Conn machinery
// ---------------------------------------------------------------------------

// retire removes a finished stream from the live map and installs its
// tombstone; a pending abort keeps retransmitting under it (PROTOCOL.md §9.2).
func (c *Conn) retire(s *clientStream) {
	c.mu.Lock()
	delete(c.ss, s.sid)
	if !c.mode.reliable {
		s.txMu.Lock()
		abort := s.abortFrame
		s.retxOpen, s.retxClose = nil, nil
		s.retxAt = time.Time{}
		s.txMu.Unlock()

		now := time.Now()
		ttl := c.mode.timing.Tombstone
		if dl, ok := s.ctx.Deadline(); ok {
			// TTL floor: the call's propagated timeout remainder (§9.2).
			ttl = max(ttl, time.Until(dl))
		}
		tb := &clientTomb{expire: now.Add(ttl), abort: abort}
		if abort != nil {
			tb.ival = c.mode.timing.Retransmit
			tb.retxAt = now.Add(tb.ival)
		}
		c.tombs[s.sid] = tb
	}
	c.mu.Unlock()
	c.kickSweep()
}

// clearTombAbort clears a tombstone's pending abort obligation: a matching
// terminal or a RESET arrived (PROTOCOL.md §10.3).
func (c *Conn) clearTombAbort(sid uint32) {
	c.mu.Lock()
	if tb := c.tombs[sid]; tb != nil {
		tb.abort = nil
		tb.retxAt = time.Time{}
	}
	c.mu.Unlock()
}

// sendReset answers a frame for an unknown call, rate-limited per sid
// (PROTOCOL.md §9.3; clients RESET immediately — no OPEN can arrive here).
func (c *Conn) sendReset(ctx context.Context, f *Frame) error {
	if !c.mode.reliable {
		sid := f.GetSid()
		n := nowNano()
		c.mu.Lock()
		if last, ok := c.resetAt[sid]; ok {
			if n-last < int64(c.mode.timing.Retransmit) {
				c.mu.Unlock()
				return nil
			}
		} else if len(c.resetAt) >= c.limits.MaxPendingResets {
			// Bounded: drop rather than grow (anti-amplification, §15).
			c.mu.Unlock()
			return nil
		}
		c.resetAt[sid] = n
		c.mu.Unlock()
		c.kickSweep()
	}
	c.protoEvent(ProtocolEvent{Kind: EventResetSent, Sid: f.GetSid()})
	return c.tx.Handle(ctx, resetFor(f))
}

// failAll ends every live call with err and drops all retransmission
// obligations (used by liveness expiry and adapter teardown).
func (c *Conn) failAll(err error) {
	c.mu.Lock()
	ss := make([]*clientStream, 0, len(c.ss))
	for _, s := range c.ss {
		ss = append(ss, s)
	}
	for _, tb := range c.tombs {
		tb.abort = nil
		tb.retxAt = time.Time{}
	}
	c.mu.Unlock()

	for _, s := range ss {
		s.finishLocal(err)
	}
}

func (c *Conn) kickSweep() {
	if c.mode.reliable {
		return
	}
	c.sw.kick(c.sweepLoop)
}

func (c *Conn) hasWork() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ss) > 0 || len(c.tombs) > 0 || len(c.resetAt) > 0
}

func (c *Conn) sweepLoop() {
	tick := time.NewTicker(c.mode.timing.tick())
	defer tick.Stop()
	for {
		select {
		case <-c.sw.quit:
			return
		case now := <-tick.C:
			c.sweep(now)
			if c.sw.idle(c.hasWork) {
				return
			}
		}
	}
}

func (c *Conn) sweep(now time.Time) {
	t := c.mode.timing
	n := now.UnixNano()

	c.mu.Lock()
	streams := make([]*clientStream, 0, len(c.ss))
	for _, s := range c.ss {
		streams = append(streams, s)
	}
	var tombRetx []*Frame
	for sid, tb := range c.tombs {
		if now.After(tb.expire) {
			delete(c.tombs, sid)
			continue
		}
		if tb.abort != nil && !tb.retxAt.IsZero() && now.After(tb.retxAt) {
			tombRetx = append(tombRetx, tb.abort)
			tb.ival = min(tb.ival*2, t.probe())
			tb.retxAt = now.Add(tb.ival)
		}
	}
	for sid, at := range c.resetAt {
		if n-at > int64(t.Tombstone) {
			delete(c.resetAt, sid)
		}
	}
	live := len(c.ss) > 0
	c.mu.Unlock()

	ctx := context.Background()

	// Peer liveness (PROTOCOL.md §10.4): one peer per Conn.
	if live {
		if n-c.lastRx.Load() >= int64(t.Liveness) {
			c.protoEvent(ProtocolEvent{Kind: EventLivenessExpired})
			c.failAll(status.Error(codes.Unavailable, "peer lost"))
			return
		}
		if n-c.lastTx.Load() >= int64(t.probe()) && n-c.lastPing.Load() >= int64(t.probe()) {
			c.lastPing.Store(n)
			c.lastTx.Store(n)
			ping := &Frame{}
			ping.SetEpoch(c.epoch)
			ping.SetFlags(FlagPing)
			c.protoEvent(ProtocolEvent{Kind: EventKeepaliveSent})
			c.tx.Handle(ctx, ping)
		}
	}

	// Per-stream retransmissions and probes.
	for _, s := range streams {
		for _, f := range s.sweepRetx(now, t.probe()) {
			s.protoEvent(ProtocolEvent{Kind: EventRetransmit})
			s.transmit(ctx, f)
		}
		if f := s.probeDue(now, t.probe()); f != nil {
			s.protoEvent(ProtocolEvent{Kind: EventProbeSent})
			// Probes feed the peer-keepalive cadence but not the stream's
			// own idle clocks (PROTOCOL.md §10.5).
			c.lastTx.Store(n)
			c.tx.Handle(ctx, f)
		}
	}
	// Tombstoned aborts.
	for _, f := range tombRetx {
		c.protoEvent(ProtocolEvent{Kind: EventRetransmit, Sid: f.GetSid()})
		c.lastTx.Store(n)
		c.tx.Handle(ctx, f)
	}
}
