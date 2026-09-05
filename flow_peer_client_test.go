package drpc_test

// flow_peer_client_test.go pins the CLIENT half of the connection window
// (PROTOCOL.md §4.2.1, reliable mode only) against a scripted server on the
// far end of the Conn's tx, so every rule can be observed without a Go server
// that grants on sid 0 yet:
//
//   - a sender assumes W_conn per Conn and parks — on the connection window,
//     not the stream window — once it has spent it, bounded by T_stall;
//   - only a sid-0 WINDOW from the server incarnation the Conn is locked to
//     credits it; a foreign one is dropped in silence, never RESET;
//   - only a streaming call's creation-ack H settles the window: a unary T
//     never does, and an H with window 0 turns it off;
//   - a new server incarnation starts the sender over (§10.6);
//   - the raise rides right behind the first OPEN, once per incarnation;
//   - an overrun fails the offending call INTERNAL and nothing else;
//   - every data frame received returns exactly one credit on sid 0:
//     consumed, discarded with its call, or never buffered;
//   - Conn.Close unparks a sender waiting on connection credit.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// wConnTest is W_conn, the connection window a sender assumes per peer before
// any sid-0 grant (PROTOCOL.md §4.2.1, Appendix B); also the MaxPeerWindow
// floor.
const wConnTest = 1024

// peerSrv is a scripted server on the far end of a Conn's tx. It records
// everything the client sends, answers every OPEN synchronously — a
// creation-ack H carrying its advertisement for a streaming call, a T for a
// unary one — and lets a test inject sequenced data frames or sid-0 grants
// under whichever server incarnation it currently is.
type peerSrv struct {
	conn   *drpc.Conn
	window uint32 // advertised on every H

	mu          sync.Mutex
	epoch       uint32 // the incarnation answering now; a restart changes it
	clientEpoch uint32 // learned from the first OPEN
	muted       bool   // answer no OPEN: the test injects the ack itself
	tx          []*drpc.Frame
	seq         map[uint32]uint32 // last server seq per sid
}

func newPeerSrv(epoch, window uint32) *peerSrv {
	return &peerSrv{epoch: epoch, window: window, seq: map[uint32]uint32{}}
}

func (p *peerSrv) Handle(ctx context.Context, f *drpc.Frame) error {
	p.mu.Lock()
	p.tx = append(p.tx, proto.CloneOf(f))
	open := f.GetFlags()&drpc.FlagOpen != 0
	if open {
		p.clientEpoch = f.GetEpoch()
	}
	muted := p.muted
	p.mu.Unlock()
	if !open || muted {
		return nil
	}
	if f.GetMethod() == echo.EchoService_Once_FullMethodName {
		// A unary call ends in its T: no H, no window (§8).
		t := p.frame(f.GetSid(), drpc.FlagClose)
		t.SetCode(uint32(codes.OK))
		data, _ := proto.Marshal(&echo.EchoResponse{})
		t.SetPayload(data)
		return p.conn.Handle(ctx, t)
	}
	h := p.frame(f.GetSid(), 0)
	h.SetWindow(p.window)
	return p.conn.Handle(ctx, h)
}

// frame builds the next sequenced server frame on sid under the current
// incarnation, echoing the client one as every server frame must (§6.1).
func (p *peerSrv) frame(sid, flags uint32) *drpc.Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq[sid]++
	f := &drpc.Frame{}
	f.SetEpoch(p.epoch)
	f.SetPeerEpoch(p.clientEpoch)
	f.SetSid(sid)
	f.SetSeq(p.seq[sid])
	f.SetFlags(flags)
	return f
}

// data injects one data frame on sid.
func (p *peerSrv) data(t *testing.T, sid uint32) {
	t.Helper()
	f := p.frame(sid, 0)
	payload, _ := proto.Marshal(&echo.EchoResponse{})
	f.SetPayload(payload)
	x.NoError(t, p.conn.Handle(context.Background(), f))
}

// grant injects a connection grant under the given server incarnation,
// addressed at the given client one (§4.2.1).
func (p *peerSrv) grant(t *testing.T, epoch, peerEpoch, n uint32) {
	t.Helper()
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetPeerEpoch(peerEpoch)
	f.SetFlags(drpc.FlagWindow)
	f.SetWindow(n)
	x.NoError(t, p.conn.Handle(context.Background(), f))
}

func (p *peerSrv) restart(epoch uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.epoch = epoch
}

// mute stops the automatic creation ack, so a test can release the call
// before answering it by hand.
func (p *peerSrv) mute(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muted = on
}

func (p *peerSrv) client() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clientEpoch
}

func (p *peerSrv) txFrames() []*drpc.Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*drpc.Frame(nil), p.tx...)
}

// lastOpen returns the sid of the most recent OPEN the client sent.
func (p *peerSrv) lastOpen() uint32 {
	frames := p.txFrames()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].GetFlags()&drpc.FlagOpen != 0 {
			return frames[i].GetSid()
		}
	}
	return 0
}

// isPeerGrant reports whether f is a connection grant: WINDOW on sid 0, seq
// 0, no payload, epoch = the sender's own (§4.2.1, §7).
func isPeerGrant(f *drpc.Frame) bool { return isWindowFrame(f) && f.GetSid() == 0 }

func peerGrants(frames []*drpc.Frame) (n int, total uint32) {
	for _, f := range frames {
		if isPeerGrant(f) {
			n++
			total += f.GetWindow()
		}
	}
	return n, total
}

func countMatch(frames []*drpc.Frame, match func(*drpc.Frame) bool) int {
	n := 0
	for _, f := range frames {
		if match(f) {
			n++
		}
	}
	return n
}

// clientFixture wires a Conn to a peerSrv. srvWindow is the per-stream window
// the fake advertises on its H — large, so that the STREAM window never binds
// and every park below is the connection window's.
func clientFixture(t *testing.T, srvEpoch, srvWindow uint32, events *flowEvents, opts ...drpc.ConnOption) (*peerSrv, *drpc.Conn, echo.EchoServiceClient) {
	t.Helper()
	srv := newPeerSrv(srvEpoch, srvWindow)
	opts = append([]drpc.ConnOption{
		drpc.WithReliable(true),
		drpc.WithTiming(drpc.Timing{Stall: 2 * time.Second}),
	}, opts...)
	if events != nil {
		opts = append(opts, drpc.WithProtocolStats(events))
	}
	conn := drpc.NewConn(srv, opts...)
	srv.conn = conn
	return srv, conn, echo.NewEchoServiceClient(conn)
}

const srvEpochA, srvEpochB = uint32(0xA11CE), uint32(0xB0B)

func sendN(t *testing.T, stream echo.EchoService_BuffClient, n int) {
	t.Helper()
	msg := echo.EchoRequest_builder{Message: "m"}.Build()
	for i := range n {
		if err := stream.Send(msg); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// §4.2.1 sending: W_conn is spent across the Conn, then the sender parks on
// the CONNECTION window — its stream window still has credit — and fails
// UNAVAILABLE at T_stall naming that window. One budget, not two.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Assumption: "Every sender assumes W_conn = 1024 messages (§10.1,
// Appendix B) toward each peer incarnation from the moment it holds state for
// it".
func TestPeerWindow_ClientParksAtWConn(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		_, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, wConnTest)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "W_conn messages go out unparked")

		start := time.Now()
		err = stream.Send(echo.EchoRequest_builder{Message: "m"}.Build())
		x.Equal(t, codes.Unavailable, status.Code(err))
		x.True(t, strings.Contains(status.Convert(err).Message(), "connection credit"),
			"the error must name the window that starved it, got: ", err)
		x.Equal(t, 2*time.Second, time.Since(start), "the park ends exactly at T_stall")
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))
		x.Equal(t, 0, events.count(drpc.EventFlowStall), "the stream window had credit")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §9.1 grants: a sid-0 WINDOW credits the connection window only
// when it comes from the server incarnation the Conn is locked to and names
// this client incarnation. Anything else is dropped in silence — no RESET,
// no credit.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Grants: a receiver applies a sid-0 WINDOW only "when it holds a
// connection sender for the incarnation the frame names — the client when the
// frame's epoch is the server incarnation the Conn is locked to and its
// peer_epoch the Conn's own epoch".
func TestPeerWindow_Sid0GrantGate(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, wConnTest)

		done := make(chan error, 1)
		go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "the next send parks")

		parked := func(why string) {
			t.Helper()
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("%s: the send must stay parked, returned %v", why, err)
			default:
			}
		}
		me := srv.client()

		// Another client incarnation's grant: not ours, not answered (§9.1).
		srv.grant(t, srvEpochA, me+1, 1)
		parked("foreign peer_epoch")
		x.Equal(t, 0, countMatch(srv.txFrames(), isResetFrame), "a foreign sid-0 grant draws no RESET")

		// Another server incarnation's grant: not the one we count against.
		srv.grant(t, srvEpochB, me, 1)
		parked("foreign server epoch")
		x.Equal(t, 0, countMatch(srv.txFrames(), isResetFrame))

		// The real one.
		srv.grant(t, srvEpochA, me, 1)
		synctest.Wait()
		x.NoError(t, <-done)
		x.Equal(t, 1, events.count(drpc.EventPeerFlowResume))
	})
}

// Pins §4.2.1 Grants: "A receiver applies it only in reliable mode".
func TestPeerWindow_Sid0GrantIgnoredInUnreliableMode(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv := newPeerSrv(srvEpochA, 0)
		conn := drpc.NewConn(srv, drpc.WithReliable(false), drpc.WithTiming(fastTiming), drpc.WithProtocolStats(events))
		srv.conn = conn
		defer conn.Close(nil)

		stream, err := echo.NewEchoServiceClient(conn).Buff(t.Context())
		x.NoError(t, err)
		srv.grant(t, srvEpochA, srv.client(), 1)
		// Well past what a (wrongly) adopted one-message window would allow.
		sendN(t, stream, 40)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))
		x.Equal(t, 0, countMatch(srv.txFrames(), isResetFrame), "dropped in silence")
		x.Equal(t, 0, countMatch(srv.txFrames(), isPeerGrant), "this side never grants on sid 0 either")
		x.NoError(t, stream.CloseSend())
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 settle: only a streaming call's creation-ack H settles the
// connection window. A unary T carries no window and must not switch it off
// while the server enforces; an H that advertises 0 does switch it off.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Settle: "A unary T and a SendHeader-flushed H carry no window
// (§7, §8) and MUST NOT settle; a unary-first Conn stays assumed until its
// first streaming ack".
func TestPeerWindow_UnaryTerminalNeverSettles(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		_, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)

		// The Conn's first server frame is a unary T (window 0).
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, wConnTest)
		done := make(chan error, 1)
		go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "the window is still on: the T settled nothing")
		conn.Close(nil)
		<-done
	})
}

// Pins §4.2.1 Settle: "window = 0 turns the connection window off toward that
// peer, as it turns the stream's off".
func TestPeerWindow_StreamingAckWithWindowZeroTurnsItOff(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		_, conn, client := clientFixture(t, srvEpochA, 0, events)
		defer conn.Close(nil)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, 2*wConnTest)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "the peer does no flow control")
		x.Equal(t, 0, events.count(drpc.EventFlowStall))
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §10.6 restart: a new server incarnation counts from zero, so the
// Conn starts its sender over when a call first accepts a frame from it — a
// cumulative count would park the honest new server's client forever. Grants
// of the dead incarnation are dropped from then on.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Restart: "When a call first accepts a frame from a different
// server epoch ... the Conn MUST start its sender over — assumed at W_conn,
// unsettled, nothing sent — ... [and] drop grants naming the old epoch".
func TestPeerWindow_ReassumeOnNewServerEpoch(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)

		old, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, old, 1000)

		srv.restart(srvEpochB)
		fresh, err := client.Buff(t.Context()) // its H comes from the new incarnation
		x.NoError(t, err)
		sendN(t, fresh, wConnTest)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "a fresh W_conn against the new server")

		done := make(chan error, 1)
		go func() { done <- fresh.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))

		srv.grant(t, srvEpochA, srv.client(), 1) // the dead incarnation
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("a grant from the old incarnation must be dropped, send returned %v", err)
		default:
		}
		srv.grant(t, srvEpochB, srv.client(), 1)
		x.NoError(t, <-done)
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 raise: a receiver whose MaxPeerWindow exceeds W_conn lifts the
// server's assumption once, with a sid-0 grant of the difference right behind
// its first OPEN — the eager one or the piggybacked one — and again for a new
// server incarnation. At the floor there is nothing to raise by.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Raise: "the client right behind its first OPEN ... and, when it
// hears a new server incarnation, at once"; "A receiver at the floor sends
// none".
func TestPeerWindow_RaiseBehindFirstOpen(t *testing.T) {
	afterFirstOpen := func(frames []*drpc.Frame) *drpc.Frame {
		for i, f := range frames {
			if f.GetFlags()&drpc.FlagOpen != 0 {
				if i+1 < len(frames) {
					return frames[i+1]
				}
				return nil
			}
		}
		return nil
	}
	limits := drpc.WithLimits(drpc.Limits{MaxPeerWindow: 2048})

	t.Run("eager OPEN", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, limits)
		defer conn.Close(nil)
		_, err := client.Buff(t.Context())
		x.NoError(t, err)
		_, err = client.Buff(t.Context())
		x.NoError(t, err)

		frames := srv.txFrames()
		raise := afterFirstOpen(frames)
		x.True(t, raise != nil && isPeerGrant(raise), "the raise rides right behind the first OPEN, got ", raise)
		x.Equal(t, uint32(2048-wConnTest), raise.GetWindow())
		x.Equal(t, srv.client(), raise.GetEpoch())
		x.Equal(t, 1, countMatch(frames, isPeerGrant), "once per Conn")
	})
	t.Run("piggybacked OPEN", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, limits)
		defer conn.Close(nil)
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)

		raise := afterFirstOpen(srv.txFrames())
		x.True(t, raise != nil && isPeerGrant(raise), "got ", raise)
		x.Equal(t, uint32(2048-wConnTest), raise.GetWindow())
	})
	t.Run("at the floor", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, drpc.WithLimits(drpc.Limits{MaxPeerWindow: 100}))
		defer conn.Close(nil)
		_, err := client.Buff(t.Context())
		x.NoError(t, err)
		x.Equal(t, 0, countMatch(srv.txFrames(), isPeerGrant), "MaxPeerWindow is floored at W_conn: nothing to raise by")
	})
	t.Run("again for a new server incarnation", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, limits)
		defer conn.Close(nil)
		_, err := client.Buff(t.Context())
		x.NoError(t, err)
		srv.restart(srvEpochB)
		_, err = client.Buff(t.Context())
		x.NoError(t, err)
		n, total := peerGrants(srv.txFrames())
		x.Equal(t, 2, n, "the new incarnation's sender starts at W_conn too")
		x.Equal(t, uint32(2*(2048-wConnTest)), total)
	})
}

// ---------------------------------------------------------------------------
// §4.2 / §4.2.1 / §15 receiving: the server may not have more than
// MaxPeerWindow buffered here across all its calls. The frame that would
// exceed it is never buffered and fails ITS call INTERNAL — the other call is
// untouched, and the failed call's frames come back as credit.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Overrun: "the receiver fails the call it is addressed to with
// INTERNAL ... and returns the frame's credit as never buffered. Never the
// peer".
func TestPeerWindow_OverrunFailsOnlyTheOffendingCall(t *testing.T) {
	// Per-stream windows of 1024, so only the connection window can trip.
	srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, drpc.WithRxBuffer(wConnTest, drpc.DropNewest))
	defer conn.Close(nil)

	a, err := client.Many(t.Context(), &echo.EchoRequest{})
	x.NoError(t, err)
	sidA := srv.lastOpen()
	b, err := client.Many(t.Context(), &echo.EchoRequest{})
	x.NoError(t, err)
	sidB := srv.lastOpen()

	// Nothing is read: exactly the window, split across the two calls.
	for range wConnTest / 2 {
		srv.data(t, sidA)
		srv.data(t, sidB)
	}
	isTerminalFor := func(sid uint32) func(*drpc.Frame) bool {
		return func(f *drpc.Frame) bool { return isTerminal(f) && f.GetSid() == sid }
	}
	x.Equal(t, 0, countMatch(srv.txFrames(), isTerminal), "the window fits")

	// One past it, on b: b fails, a does not.
	srv.data(t, sidB)
	frames := srv.txFrames()
	x.Equal(t, 1, countMatch(frames, isTerminalFor(sidB)), "the offending call aborts")
	x.Equal(t, 0, countMatch(frames, isTerminalFor(sidA)), "the other call is untouched")
	x.Equal(t, 0, countMatch(frames, isResetFrame))

	// b's buffered frames are still delivered, then the overrun surfaces.
	got := 0
	for {
		_, err := b.Recv()
		if err != nil {
			x.Equal(t, codes.Internal, status.Code(err))
			x.True(t, strings.Contains(status.Convert(err).Message(), "connection flow-control window"),
				"the error must name the connection window, got: ", err)
			break
		}
		got++
	}
	x.Equal(t, wConnTest/2, got)

	// Discarding b's frames returned their credit at once: half the window,
	// in one grant — and draining them afterwards returned nothing twice.
	n, total := peerGrants(srv.txFrames())
	x.Equal(t, 1, n)
	x.Equal(t, uint32(wConnTest/2), total)

	// a is live: with b's frames gone there is room for it again.
	srv.data(t, sidA)
	x.Equal(t, 0, countMatch(srv.txFrames(), isTerminalFor(sidA)))
	res, err := a.Recv()
	x.NoError(t, err)
	x.True(t, res != nil)
}

// ---------------------------------------------------------------------------
// §4.2.1 credit return: every reliable-mode data frame received returns one
// credit on sid 0 once it stops occupying a buffer — consumed (batched at half
// the window), or never buffered at all (a frame for a call this side no
// longer has draws a RESET and still returns its credit; an off-shape one is
// dropped and still returns it).
// ---------------------------------------------------------------------------

// Pins §4.2.1 Cadence: "grant once the credit it holds back reaches half its
// window (one small frame per MaxPeerWindow/2 messages)".
func TestPeerWindow_ConsumedFramesGrantOnSid0(t *testing.T) {
	srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, drpc.WithRxBuffer(wConnTest, drpc.DropNewest))
	defer conn.Close(nil)

	stream, err := client.Many(t.Context(), &echo.EchoRequest{})
	x.NoError(t, err)
	sid := srv.lastOpen()
	const injected = 600
	for range injected {
		srv.data(t, sid)
	}
	x.Equal(t, 0, countMatch(srv.txFrames(), isPeerGrant), "nothing consumed, nothing granted")

	for range injected {
		_, err := stream.Recv()
		x.NoError(t, err)
	}
	n, total := peerGrants(srv.txFrames())
	x.Equal(t, 1, n, "batched at half the window: 600 consumed is one grant")
	x.Equal(t, uint32(wConnTest/2), total)
	// The per-stream grant is still there, on the call's own sid (§4.2.1).
	x.Equal(t, 1, countMatch(srv.txFrames(), func(f *drpc.Frame) bool {
		return isWindowFrame(f) && f.GetSid() == sid
	}))
}

// Pins §4.2.1 The receiver's ledger: one credit back "when it was never
// buffered at all — dropped as off-shape (§8) ... or addressed to a call the
// receiver no longer has (RESET-drawn, §9.3)".
func TestPeerWindow_NeverBufferedFramesReturnCredit(t *testing.T) {
	t.Run("unknown sid: RESET and credit", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil)
		defer conn.Close(nil)
		// A finished call, so the Conn is locked to the server incarnation.
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)
		sid := srv.lastOpen()

		// Data for the finished call: unknown at a client (no tombstones in
		// reliable mode), each draws a RESET (§9.3, §10.6)...
		for range wConnTest / 2 {
			srv.data(t, sid)
		}
		frames := srv.txFrames()
		x.Equal(t, wConnTest/2, countMatch(frames, isResetFrame))
		// ...and the credit they spent comes back.
		n, total := peerGrants(frames)
		x.Equal(t, 1, n)
		x.Equal(t, uint32(wConnTest/2), total)
	})
	t.Run("off-shape: dropped and credit", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil)
		defer conn.Close(nil)
		stream, err := client.Buff(t.Context()) // client-streaming: no server data frames
		x.NoError(t, err)
		sid := srv.lastOpen()
		for range wConnTest / 2 {
			srv.data(t, sid)
		}
		frames := srv.txFrames()
		x.Equal(t, 0, countMatch(frames, isTerminal), "off-shape frames are dropped, the call lives")
		n, total := peerGrants(frames)
		x.Equal(t, 1, n)
		x.Equal(t, uint32(wConnTest/2), total)
		x.NoError(t, stream.CloseSend())
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §4.5: a sender parked on connection credit has no call left to
// wake it through when the Conn closes — Close releases the window itself.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Sending: "Conn.Close and DisconnectPeer (§4.5) release a parked
// connection sender, as a call's end releases a parked stream sender".
func TestPeerWindow_CloseUnparksASenderWaitingOnConnectionCredit(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		_, conn, client := clientFixture(t, srvEpochA, 4096, events)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, wConnTest)
		done := make(chan error, 1)
		go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))

		conn.Close(nil)
		err = <-done
		x.True(t, err == io.EOF || status.Code(err) == codes.Unavailable, "got ", err)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowResume), "no credit ever came")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 restart / raise: the Conn locks to a server incarnation on the first
// sequenced frame it hears from it — on a live call or not. The server's one
// raise rides right behind its first H; when that H lands on a call the
// client already released it draws a RESET, and a Conn that had not locked
// from it would drop the raise as a stranger's and stay at W_conn against a
// larger window for its whole life.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Restart: the Conn "locks ... on the first sequenced frame it
// hears from it, for a live call or for one it has already released".
func TestPeerWindow_RaiseLandsOnAReleasedCall(t *testing.T) {
	// Above 2 × W_conn: with the raise lost, no cadence of a 4096 receiver
	// reaches a sender stuck at 1024 (pending ≥ 2048, or outstanding +
	// pending ≥ 4096) — a forever-park, not a slow one.
	const window = 4 * wConnTest
	const raise = window - wConnTest
	msg := echo.EchoRequest_builder{Message: "m"}.Build()

	// lifted sends MaxPeerWindow messages unparked, then parks on the next:
	// the raise landed, and it was exactly the raise.
	lifted := func(t *testing.T, conn *drpc.Conn, client echo.EchoServiceClient, events *flowEvents) {
		t.Helper()
		s, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, s, int(window))
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "the raise landed")
		done := make(chan error, 1)
		go func() { done <- s.Send(msg) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "exactly the lifted window")
		conn.Close(nil) // releases the parked send
		<-done
	}

	t.Run("the Conn's first streaming call", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			srv, conn, client := clientFixture(t, srvEpochA, window, events)
			defer conn.Close(nil)

			// Released before its ack: the H then lands on no call — a
			// RESET — and the raise rides right behind it.
			srv.mute(true)
			ctx, cancel := context.WithCancel(t.Context())
			_, err := client.Buff(ctx)
			x.NoError(t, err)
			cancel()
			synctest.Wait()
			h := srv.frame(srv.lastOpen(), 0)
			h.SetWindow(window)
			x.NoError(t, conn.Handle(context.Background(), h))
			x.Equal(t, 1, countMatch(srv.txFrames(), isResetFrame), "the H found no call")
			srv.grant(t, srvEpochA, srv.client(), raise)
			srv.mute(false)

			lifted(t, conn, client, events)
		})
	})
	t.Run("the first streaming call to a new incarnation", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			srv, conn, client := clientFixture(t, srvEpochA, window, events)
			defer conn.Close(nil)

			// Locked to A by a call it answered, lifted by A, some sent.
			warm, err := client.Buff(t.Context())
			x.NoError(t, err)
			srv.grant(t, srvEpochA, srv.client(), raise)
			sendN(t, warm, 10)

			// The server restarts; the Conn's first call to B is released
			// before B's ack, which lands on no call. The Conn re-locks to B
			// from it all the same — sender started over — and B's raise
			// right behind it lands.
			srv.restart(srvEpochB)
			srv.mute(true)
			ctx, cancel := context.WithCancel(t.Context())
			_, err = client.Buff(ctx)
			x.NoError(t, err)
			cancel()
			synctest.Wait()
			h := srv.frame(srv.lastOpen(), 0)
			h.SetWindow(window)
			x.NoError(t, conn.Handle(context.Background(), h))
			srv.grant(t, srvEpochB, srv.client(), raise)
			srv.mute(false)

			lifted(t, conn, client, events)
		})
	})
}
