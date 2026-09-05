package drpc

import (
	"context"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/stats"
)

// This file holds the two observability surfaces (PROTOCOL.md §14):
//
//   - google.golang.org/grpc/stats.Handler, so existing gRPC instrumentation
//     (OpenTelemetry, opencensus, custom handlers) works unchanged;
//   - ProtocolStats, for the datagram-specific events gRPC has no concept of —
//     the skipped-message counter §14 promises, rx drops, RESETs,
//     retransmissions, probes, liveness expiry, tombstone replays and
//     flow-control stalls.

// ProtocolEventKind names a drpc protocol event.
type ProtocolEventKind int

const (
	// EventSkipped: a seq gap was observed on a stream. Count is the number of
	// messages the gap skipped — the counter PROTOCOL.md §14 promises.
	EventSkipped ProtocolEventKind = iota
	// EventDropped: a frame was dropped by the rx buffer's drop policy (§4.2).
	EventDropped
	// EventOffShape: a frame was dropped because its shape is illegal for the
	// call's RPC type (§8).
	EventOffShape
	// EventResetSent / EventResetReceived: a RESET was volunteered / acted on
	// (§9.3).
	EventResetSent
	EventResetReceived
	// EventRetransmit: a control frame was retransmitted (§10.3).
	EventRetransmit
	// EventProbeSent: a stream probe was sent (§10.5).
	EventProbeSent
	// EventKeepaliveSent: a peer keepalive PING was sent (§10.4).
	EventKeepaliveSent
	// EventLivenessExpired: a peer's liveness window elapsed (§10.4).
	EventLivenessExpired
	// EventTombstoneReplay: a stored terminal was replayed (§9.2).
	EventTombstoneReplay
	// EventDataLoss: a window overrun failed a call loudly (§6.3).
	EventDataLoss
	// EventFlowStall: a sender parked waiting for flow-control credit, and
	// EventFlowResume when it got some (§4.2, reliable mode).
	EventFlowStall
	EventFlowResume
	// EventPeerFlowStall: a sender parked because the peer's CONNECTION
	// window was empty — whether or not its stream window had credit too: a
	// send short on both waits on the peer's whole budget, and reports this
	// pair (§4.2.1, §14) — and EventPeerFlowResume when it got some
	// (reliable mode). Distinct
	// from EventFlowStall because the remedies differ: a stream stall is
	// "this consumer stopped", a peer stall is "raise MaxPeerWindow or find
	// the other slow consumer". Both carry the parked call's Sid and Method.
	EventPeerFlowStall
	EventPeerFlowResume
)

func (k ProtocolEventKind) String() string {
	switch k {
	case EventSkipped:
		return "skipped"
	case EventDropped:
		return "dropped"
	case EventOffShape:
		return "off-shape"
	case EventResetSent:
		return "reset-sent"
	case EventResetReceived:
		return "reset-received"
	case EventRetransmit:
		return "retransmit"
	case EventProbeSent:
		return "probe-sent"
	case EventKeepaliveSent:
		return "keepalive-sent"
	case EventLivenessExpired:
		return "liveness-expired"
	case EventTombstoneReplay:
		return "tombstone-replay"
	case EventDataLoss:
		return "data-loss"
	case EventFlowStall:
		return "flow-stall"
	case EventFlowResume:
		return "flow-resume"
	case EventPeerFlowStall:
		return "peer-flow-stall"
	case EventPeerFlowResume:
		return "peer-flow-resume"
	}
	return "unknown"
}

// ProtocolEvent describes one protocol event. Sid and Method are zero/empty
// for peer-scope events.
type ProtocolEvent struct {
	Kind   ProtocolEventKind
	Peer   any
	Sid    uint32
	Method string
	// Count carries the event's magnitude where one exists: messages skipped
	// (EventSkipped), frames dropped in this event (EventDropped, always 1).
	Count uint32
}

// ProtocolStats observes drpc protocol events. Implementations must not
// block: these fire on the receive and timer paths, and an endpoint's whole
// receive loop waits behind a slow observer. The same applies to a
// stats.Handler installed with WithStatsHandler.
type ProtocolStats interface {
	ProtocolEvent(ev ProtocolEvent)
}

// ProtocolStatsFunc adapts a function to ProtocolStats.
type ProtocolStatsFunc func(ev ProtocolEvent)

func (f ProtocolStatsFunc) ProtocolEvent(ev ProtocolEvent) { f(ev) }

// Counters is a ready-made ProtocolStats that just counts, for applications
// that want the §14 gap counter without writing a handler. The zero value is
// usable; read with Snapshot.
type Counters struct {
	skipped         atomic.Uint64
	dropped         atomic.Uint64
	offShape        atomic.Uint64
	resetSent       atomic.Uint64
	resetReceived   atomic.Uint64
	retransmit      atomic.Uint64
	probeSent       atomic.Uint64
	keepaliveSent   atomic.Uint64
	livenessExpired atomic.Uint64
	tombstoneReplay atomic.Uint64
	dataLoss        atomic.Uint64
	flowStall       atomic.Uint64
	flowResume      atomic.Uint64
	peerFlowStall   atomic.Uint64
	peerFlowResume  atomic.Uint64
}

// CounterSnapshot is a point-in-time read of Counters.
type CounterSnapshot struct {
	Skipped         uint64 // messages lost to gaps (§14)
	Dropped         uint64 // frames dropped by the rx drop policy (§4.2)
	OffShape        uint64 // frames dropped as illegal for the call shape (§8)
	ResetSent       uint64
	ResetReceived   uint64
	Retransmit      uint64 // control frames retransmitted (§10.3)
	ProbeSent       uint64
	KeepaliveSent   uint64
	LivenessExpired uint64
	TombstoneReplay uint64
	DataLoss        uint64 // calls failed by window overrun (§6.3)
	FlowStall       uint64 // sends parked on stream flow-control credit
	FlowResume      uint64 // parked sends that got credit and continued
	PeerFlowStall   uint64 // sends parked on the peer's connection window (§4.2.1)
	PeerFlowResume  uint64 // parked sends that got connection credit and continued
}

func (c *Counters) ProtocolEvent(ev ProtocolEvent) {
	n := uint64(max(ev.Count, 1))
	switch ev.Kind {
	case EventSkipped:
		c.skipped.Add(n)
	case EventDropped:
		c.dropped.Add(n)
	case EventOffShape:
		c.offShape.Add(n)
	case EventResetSent:
		c.resetSent.Add(1)
	case EventResetReceived:
		c.resetReceived.Add(1)
	case EventRetransmit:
		c.retransmit.Add(1)
	case EventProbeSent:
		c.probeSent.Add(1)
	case EventKeepaliveSent:
		c.keepaliveSent.Add(1)
	case EventLivenessExpired:
		c.livenessExpired.Add(1)
	case EventTombstoneReplay:
		c.tombstoneReplay.Add(1)
	case EventDataLoss:
		c.dataLoss.Add(1)
	case EventFlowStall:
		c.flowStall.Add(1)
	case EventFlowResume:
		c.flowResume.Add(1)
	case EventPeerFlowStall:
		c.peerFlowStall.Add(1)
	case EventPeerFlowResume:
		c.peerFlowResume.Add(1)
	}
}

func (c *Counters) Snapshot() CounterSnapshot {
	return CounterSnapshot{
		Skipped:         c.skipped.Load(),
		Dropped:         c.dropped.Load(),
		OffShape:        c.offShape.Load(),
		ResetSent:       c.resetSent.Load(),
		ResetReceived:   c.resetReceived.Load(),
		Retransmit:      c.retransmit.Load(),
		ProbeSent:       c.probeSent.Load(),
		KeepaliveSent:   c.keepaliveSent.Load(),
		LivenessExpired: c.livenessExpired.Load(),
		TombstoneReplay: c.tombstoneReplay.Load(),
		DataLoss:        c.dataLoss.Load(),
		FlowStall:       c.flowStall.Load(),
		FlowResume:      c.flowResume.Load(),
		PeerFlowStall:   c.peerFlowStall.Load(),
		PeerFlowResume:  c.peerFlowResume.Load(),
	}
}

// connBegin/connEnd report the client endpoint's lifetime to gRPC stats
// handlers, which key per-connection state off the ctx TagConn returns. A
// server has no equivalent: its peers come and go per frame, and drpc reports
// those through ProtocolStats instead.
func (c *Conn) connBegin() {
	if len(c.stats) == 0 {
		return
	}
	g := grpcStats(c.stats)
	info := &stats.ConnTagInfo{}
	if p := c.peer; p != nil {
		info.RemoteAddr, info.LocalAddr = p.Addr, p.LocalAddr
	}
	c.connCtx = g.tagConn(context.Background(), info)
	g.conn(c.connCtx, &stats.ConnBegin{Client: true})
}

func (c *Conn) connEnd() {
	if len(c.stats) == 0 {
		return
	}
	grpcStats(c.stats).conn(c.connCtx, &stats.ConnEnd{Client: true})
}

// statsSink fans one event out to the configured handlers.
type statsSink []ProtocolStats

func (s statsSink) emit(ev ProtocolEvent) {
	for _, h := range s {
		h.ProtocolEvent(ev)
	}
}

// grpcStats fans gRPC stats events out to the configured handlers.
type grpcStats []stats.Handler

func (g grpcStats) handle(ctx context.Context, s stats.RPCStats) {
	for _, h := range g {
		h.HandleRPC(ctx, s)
	}
}

func (g grpcStats) tagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	for _, h := range g {
		ctx = h.TagRPC(ctx, info)
	}
	return ctx
}

func (g grpcStats) tagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	for _, h := range g {
		ctx = h.TagConn(ctx, info)
	}
	return ctx
}

func (g grpcStats) conn(ctx context.Context, s stats.ConnStats) {
	for _, h := range g {
		h.HandleConn(ctx, s)
	}
}

// payloadOut reports one message leaving. payload is the message as the
// application produced it; wire is what the frame carried (compressed, if the
// call has a compressor), matching gRPC's Length / CompressedLength split.
func (g grpcStats) payloadOut(ctx context.Context, client bool, m any, payload []byte, wire int) {
	if len(g) == 0 {
		return
	}
	g.handle(ctx, &stats.OutPayload{
		Client:           client,
		Payload:          m,
		Length:           len(payload),
		CompressedLength: wire,
		WireLength:       wire,
		SentTime:         time.Now(),
	})
}

// payloadIn reports one message arriving; see payloadOut for the lengths.
func (g grpcStats) payloadIn(ctx context.Context, client bool, m any, payload []byte, wire int) {
	if len(g) == 0 {
		return
	}
	g.handle(ctx, &stats.InPayload{
		Client:           client,
		Payload:          m,
		Length:           len(payload),
		CompressedLength: wire,
		WireLength:       wire,
		RecvTime:         time.Now(),
	})
}
