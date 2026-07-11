package drpc

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file holds the server-side unreliable-mode machinery: per-incarnation
// peer containers (tombstones, aged watermark, liveness), delayed RESETs,
// stream probes, and the sweep loop (PROTOCOL.md §9-§10, Appendix C).

// srvTomb remembers a finished call: its stored terminal frame is replayed
// (rate-limited) when stragglers or probes hit it (PROTOCOL.md §9.2).
type srvTomb struct {
	sid        uint32
	term       *Frame // nil = key-only (dedup preserved, replay lost)
	size       int
	expire     time.Time
	lastReplay time.Time
}

type hwmCP struct {
	at  time.Time
	hwm uint32
}

// peerState is the container for one client incarnation seen from one peer:
// (peer, client-epoch). Fields are guarded by Server.mu except the atomic
// clocks, which hot paths feed without the lock.
type peerState struct {
	peer  any
	epoch uint32
	txCtx context.Context // Server root + peer, for timer-driven sends (§6.4)

	// reliable is the mode of this peer's channel, captured at container
	// creation from the frame annotation (PROTOCOL.md §4.3). A reliable
	// container runs no timers: the sweep skips it entirely — no liveness,
	// no PING, no checkpoints, no GC (it lives until teardown, §10.6).
	reliable bool

	hwm  uint32
	cps  []hwmCP // watermark checkpoints, appended by the sweep (§9.4)
	dead bool    // liveness expired; cleared state

	tombs     map[uint32]*srvTomb
	tombOrder []uint32 // insertion order for eviction
	tombBytes int

	liveCalls int
	created   time.Time

	maxTombs     int // §15 caps, copied from the Server at creation
	maxTombBytes int
	maxLive      int

	lastRx   atomic.Int64 // validated frames only (§9.1)
	lastTx   atomic.Int64
	lastPing atomic.Int64
}

// hwmAgedLocked is the high-water mark as of TTL_tomb ago; sids at or below
// it are necessarily stale (PROTOCOL.md §9.4). Reliable mode degenerates to
// the plain current hwm (no aging: nothing is ever late).
func (ps *peerState) hwmAgedLocked(now time.Time, ttl time.Duration, reliable bool) uint32 {
	if reliable {
		return ps.hwm
	}
	aged := uint32(0)
	for _, cp := range ps.cps {
		if now.Sub(cp.at) >= ttl {
			aged = cp.hwm
		}
	}
	return aged
}

func (ps *peerState) addTombLocked(sid uint32, term *Frame, expire time.Time) {
	size := 0
	if term != nil {
		size = len(term.GetPayload())
	}
	if old := ps.tombs[sid]; old != nil {
		// Replace in place: keep the order entry, fix the byte accounting.
		ps.tombBytes += size - old.size
		old.term, old.size = term, size
		if expire.After(old.expire) {
			old.expire = expire
		}
		return
	}
	ps.tombs[sid] = &srvTomb{sid: sid, term: term, size: size, expire: expire}
	ps.tombOrder = append(ps.tombOrder, sid)
	ps.tombBytes += size

	// Byte cap: degrade oldest stored terminals to key-only (§9.2, §15).
	for i := 0; ps.tombBytes > ps.maxTombBytes && i < len(ps.tombOrder); i++ {
		if tb := ps.tombs[ps.tombOrder[i]]; tb != nil && tb.term != nil {
			ps.tombBytes -= tb.size
			tb.term, tb.size = nil, 0
		}
	}
	// Entry cap: drop oldest entries outright (a §14 residual).
	for len(ps.tombs) > ps.maxTombs && len(ps.tombOrder) > 0 {
		sid := ps.tombOrder[0]
		ps.tombOrder = ps.tombOrder[1:]
		if tb := ps.tombs[sid]; tb != nil {
			ps.tombBytes -= tb.size
			delete(ps.tombs, sid)
		}
	}
}

func (ps *peerState) removeTombLocked(sid uint32) {
	if tb := ps.tombs[sid]; tb != nil {
		ps.tombBytes -= tb.size
		delete(ps.tombs, sid)
	}
}

// replayTombLocked returns the stored terminal if the per-tombstone rate
// limit allows another replay (≤ 1 per RTI, PROTOCOL.md §9.2).
func (ps *peerState) replayTombLocked(tb *srvTomb, now time.Time, rti time.Duration) *Frame {
	if tb.term == nil || now.Sub(tb.lastReplay) < rti {
		return nil
	}
	tb.lastReplay = now
	ps.lastTx.Store(now.UnixNano())
	return tb.term
}

// pendingReset is a scheduled delayed RESET for an unknown-sid frame whose
// OPEN may merely be late (PROTOCOL.md §9.3).
type pendingReset struct {
	due  time.Time
	echo uint32 // epoch of the offending frame
}

// ensurePeerLocked returns the container for ek, creating it and enforcing
// the per-peer container cap (never evicting containers with live calls,
// PROTOCOL.md §15). Server.mu held. reliable applies on creation only: the
// mode is a property of the peer's channel and cannot change (§4.3) — an
// existing container keeps its first-captured value.
func (s *Server) ensurePeerLocked(ek epochKey, now time.Time, reliable bool) *peerState {
	ps := s.peers[ek]
	if ps != nil {
		return ps
	}

	// Cap dead containers of this transport peer.
	dead := make([]*peerState, 0, 4)
	for k, p := range s.peers {
		if k.peer == ek.peer && p.liveCalls == 0 {
			dead = append(dead, p)
		}
	}
	if len(dead) >= s.limits.MaxDeadPeers {
		oldest := dead[0]
		for _, p := range dead[1:] {
			if p.created.Before(oldest.created) {
				oldest = p
			}
		}
		delete(s.peers, epochKey{peer: oldest.peer, epoch: oldest.epoch})
	}

	txCtx := s.root
	if ek.peer != nil {
		txCtx = NewPeerContext(txCtx, ek.peer)
	}
	ps = &peerState{
		peer:         ek.peer,
		epoch:        ek.epoch,
		txCtx:        txCtx,
		reliable:     reliable,
		tombs:        map[uint32]*srvTomb{},
		created:      now,
		maxTombs:     s.limits.MaxTombstones,
		maxTombBytes: s.limits.MaxTombstoneBytes,
		maxLive:      s.limits.MaxLiveCalls,
	}
	ps.lastRx.Store(now.UnixNano())
	ps.lastTx.Store(now.UnixNano())
	s.peers[ek] = ps
	if !reliable {
		s.sawUnreliable.Store(true)
	}
	return ps
}

func (s *Server) kickSweep() {
	if !s.sawUnreliable.Load() {
		// Only unreliable-mode state needs timers; a server that has seen
		// none runs no sweeper at all.
		return
	}
	s.sw.kick(s.sweepLoop)
}

func (s *Server) hasWork() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingResets) > 0 || len(s.resetAt) > 0 {
		return true
	}
	for _, ps := range s.peers {
		// Reliable containers are not swept (no timers, no GC): only an
		// unreliable one keeps the sweeper alive.
		if !ps.reliable {
			return true
		}
	}
	return false
}

func (s *Server) sweepLoop() {
	tick := time.NewTicker(s.mode.timing.tick())
	defer tick.Stop()
	for {
		select {
		case <-s.sw.quit:
			return
		case now := <-tick.C:
			s.sweep(now)
			if s.sw.idle(s.hasWork) {
				return
			}
		}
	}
}

func (s *Server) sweep(now time.Time) {
	t := s.mode.timing
	n := now.UnixNano()

	type txJob struct {
		ctx context.Context
		f   *Frame
	}
	var jobs []txJob
	var lost []*serverStream

	s.mu.Lock()

	// Delayed RESETs: fire if the call is still unknown (§9.3).
	for key, pr := range s.pendingResets {
		if now.Before(pr.due) {
			continue
		}
		delete(s.pendingResets, key)
		if _, live := s.calls[key]; live {
			continue
		}
		if ps := s.peers[epochKey{peer: key.peer, epoch: key.epoch}]; ps != nil {
			if _, tombed := ps.tombs[key.sid]; tombed {
				continue
			}
		}
		r := &Frame{}
		r.SetFlags(FlagReset)
		r.SetEpoch(pr.echo)
		r.SetSid(key.sid)
		ctx := s.root
		if key.peer != nil {
			ctx = NewPeerContext(ctx, key.peer)
		}
		jobs = append(jobs, txJob{ctx, r})
	}

	// Prune the immediate-RESET rate-limit history.
	for key, at := range s.resetAt {
		if n-at > int64(t.Tombstone) {
			delete(s.resetAt, key)
		}
	}

	// Containers: checkpoints, tombstone expiry, liveness, keepalive, GC.
	for ek, ps := range s.peers {
		if ps.reliable {
			// A reliable peer runs no timers (PROTOCOL.md §10.6): no
			// liveness, no PING, no tombstones to expire, no aging (its
			// watermark is plain hwm), and no GC — state lives until
			// teardown (DisconnectPeer/Stop).
			continue
		}
		ps.cps = append(ps.cps, hwmCP{at: now, hwm: ps.hwm})
		for len(ps.cps) > 1 && now.Sub(ps.cps[1].at) >= t.Tombstone {
			// Keep exactly one checkpoint older than TTL: it defines hwm_aged.
			ps.cps = ps.cps[1:]
		}

		aged := ps.hwmAgedLocked(now, t.Tombstone, false)
		for sid, tb := range ps.tombs {
			// Expiry is coupled to the aged watermark (§9.2): a tombstone
			// dies only once hwm_aged covers its sid. Plain compare (§6.2).
			if now.After(tb.expire) && sid <= aged {
				ps.removeTombLocked(sid)
			}
		}
		if len(ps.tombOrder) > 2*len(ps.tombs)+16 {
			// Compact the eviction order of expired entries.
			kept := ps.tombOrder[:0]
			for _, sid := range ps.tombOrder {
				if _, ok := ps.tombs[sid]; ok {
					kept = append(kept, sid)
				}
			}
			ps.tombOrder = kept
		}

		if ps.liveCalls > 0 && !ps.dead {
			if n-ps.lastRx.Load() >= int64(t.Liveness) {
				// Peer lost (§10.4): cancel its calls, degrade tombstones.
				ps.dead = true
				for k, st := range s.calls {
					if k.peer == ek.peer && k.epoch == ek.epoch {
						st.suppressTerm.Store(true)
						lost = append(lost, st)
					}
				}
				for _, tb := range ps.tombs {
					ps.tombBytes -= tb.size
					tb.term, tb.size = nil, 0
				}
			} else if n-ps.lastTx.Load() >= int64(t.probe()) && n-ps.lastPing.Load() >= int64(t.probe()) {
				ps.lastPing.Store(n)
				ps.lastTx.Store(n)
				ping := &Frame{}
				ping.SetEpoch(s.epoch)
				ping.SetFlags(FlagPing)
				jobs = append(jobs, txJob{ps.txCtx, ping})
			}
		}

		// Containers outlive their tombstones (retention ≥ TTL_tomb after
		// the last activity, §9.4): the aged watermark must still be there
		// to reject stale OPENs once the tombstones are gone.
		if ps.liveCalls == 0 && len(ps.tombs) == 0 && n-ps.lastRx.Load() > int64(2*t.Tombstone) {
			delete(s.peers, ek)
		}
	}

	// Stream probes (§10.5). Calls on reliable channels are not probed.
	for _, st := range s.calls {
		if st.reliable {
			continue
		}
		if f := st.probeDue(now, t.probe(), s.epoch); f != nil {
			if ps := s.peers[epochKey{peer: st.key.peer, epoch: st.key.epoch}]; ps != nil {
				ps.lastTx.Store(n)
			}
			jobs = append(jobs, txJob{context.WithoutCancel(st.ctx), f})
		}
	}

	s.mu.Unlock()

	cause := status.Error(codes.Unavailable, "peer lost")
	for _, st := range lost {
		st.cancel(cause)
	}
	for _, j := range jobs {
		s.tx.Handle(j.ctx, j.f)
	}
}
