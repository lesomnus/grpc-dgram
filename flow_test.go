package drpc_test

// flow_test.go pins per-stream flow control (PROTOCOL.md §4.2.1, reliable
// mode only) end to end:
//
//   - the head-of-line fix it exists for: one call's consumer may stall
//     without touching any other call on the same channel (§4.2, §4.2.1);
//   - the park/resume boundary of a sender: exactly the credit it was given
//     reaches the wire, in order, no message lost or duplicated;
//   - the advertisement path on the wire — the client's OPEN, the server's
//     creation-ack H, and the WINDOW grant frame (§4.2.1, §7, §8);
//   - the T_stall bound of a park (§4.2.1, §10.1);
//   - unreliable mode ignoring window/WINDOW entirely (§4.2.1 scope);
//   - the two rules that keep a grant from becoming a weapon: a WINDOW for an
//     unknown or finished sid is dropped in silence, never answered with a
//     RESET (§4.2.1, §9.3), and credit taken for a frame the adapter refused
//     is refunded (§4.4) so a handler that ignores the error never parks
//     forever;
//   - the reliable-mode rx-buffer floor of W_init that makes the sender's
//     assumption safe (§4.2.1, Appendix B).

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// wInitTest is W_init, the initial per-stream window a sender assumes before
// the peer advertises its own (PROTOCOL.md §4.2.1, Appendix B). It is also the
// default rx buffer and the reliable-mode floor.
const wInitTest = uint32(32)

// txFrames snapshots the recorded client->server frames (the twin of
// rxFrames, header_md_test.go).
func (c *Client) txFrames() []*drpc.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*drpc.Frame(nil), c.tx...)
}

// isWindowFrame reports whether f is a well-formed grant: shape WINDOW alone,
// seq 0, no payload (PROTOCOL.md §7, §9.1).
func isWindowFrame(f *drpc.Frame) bool {
	return f.GetFlags() == drpc.FlagWindow && f.GetSeq() == 0 && !f.HasPayload()
}

// firstMatch returns the first frame satisfying match, or nil.
func firstMatch(frames []*drpc.Frame, match func(*drpc.Frame) bool) *drpc.Frame {
	for _, f := range frames {
		if match(f) {
			return f
		}
	}
	return nil
}

// flowEvents counts the protocol events an endpoint reports (§14); the
// flow-control tests read EventFlowStall / EventFlowResume out of it.
type flowEvents struct {
	mu sync.Mutex
	n  map[drpc.ProtocolEventKind]int
}

func (e *flowEvents) ProtocolEvent(ev drpc.ProtocolEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.n == nil {
		e.n = map[drpc.ProtocolEventKind]int{}
	}
	e.n[ev.Kind]++
}

func (e *flowEvents) count(k drpc.ProtocolEventKind) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n[k]
}

// ---------------------------------------------------------------------------
// §4.2 / §4.2.1: the head-of-line fix. A reliable adapter delivers every
// call's frames from ONE loop, so before v1.1 a consumer that stopped reading
// blocked that loop and with it every other call on the channel. Now the
// producer parks on credit instead, and the channel stays live.
// ---------------------------------------------------------------------------

func TestFlow_StalledConsumerDoesNotBlockOtherCalls(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithReliable(true)},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(true), drpc.WithProtocolStats(events),
			},
		}.Use(t)
		defer stop()

		const burst = 200 // far past the client's 32-message window
		stalled, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message: "m",
			Repeat:  burst,
		}.Build())
		x.NoError(t, err)

		// Nothing is read from `stalled`: its buffer fills and the producing
		// handler parks on flow control — a park in the SENDER, which is the
		// whole point (§4.2.1).
		synctest.Wait()
		x.True(t, events.count(drpc.EventFlowStall) > 0,
			"the producer must park on credit, not the receiver on its buffer")

		// The shared delivery goroutine is therefore free, and a second call
		// on the same channel completes. Under the pre-v1.1 core this Once
		// never returned: the pump was blocked handing frame 33 to the stalled
		// call.
		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		// And the parked call is only parked: as the application consumes, the
		// grants resume it and every message arrives, in order (§14).
		n := uint32(0)
		for {
			res, err := stalled.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err)
			x.Equal(t, n, res.GetSequence())
			n++
		}
		x.Equal(t, uint32(burst), n)
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 park/resume boundary, pinned against a crafted peer so the window is
// exact: the advertisement is authoritative, a sender emits exactly the credit
// it holds and not one frame more, and each grant releases exactly what it
// credits — in order, once each.
// ---------------------------------------------------------------------------

// flowPeer is a crafted server for one client-streaming call: it acks the
// OPEN with an H advertising `window`, records every client frame, and grants
// credit only when the test says so.
type flowPeer struct {
	conn   *drpc.Conn
	window uint32

	mu     sync.Mutex
	frames []*drpc.Frame
	epoch  uint32 // the client incarnation, learned from its OPEN
	sid    uint32
}

const flowPeerEpoch = uint32(0x5EED)

func (p *flowPeer) Handle(ctx context.Context, f *drpc.Frame) error {
	p.mu.Lock()
	p.frames = append(p.frames, proto.CloneOf(f))
	open := f.GetFlags()&drpc.FlagOpen != 0
	if open {
		p.epoch, p.sid = f.GetEpoch(), f.GetSid()
	}
	p.mu.Unlock()
	if !open {
		return nil
	}
	// The creation ack carries this side's advertisement (§4.2.1, §8).
	h := p.frame(0)
	h.SetSeq(1)
	h.SetWindow(p.window)
	return p.conn.Handle(ctx, h)
}

// frame builds a server frame addressed at the recorded call, echoing the
// client incarnation as every server frame must (§6.1).
func (p *flowPeer) frame(flags uint32) *drpc.Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := &drpc.Frame{}
	f.SetEpoch(flowPeerEpoch)
	f.SetPeerEpoch(p.epoch)
	f.SetSid(p.sid)
	f.SetFlags(flags)
	return f
}

// grant sends a WINDOW frame adding n messages of credit (§4.2.1, §7).
func (p *flowPeer) grant(ctx context.Context, n uint32) error {
	f := p.frame(drpc.FlagWindow)
	f.SetWindow(n)
	return p.conn.Handle(ctx, f)
}

// messages returns the payloads of the data frames that reached the wire, in
// order.
func (p *flowPeer) messages(t *testing.T) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, f := range p.frames {
		if f.GetFlags() != 0 || !f.HasPayload() {
			continue
		}
		req := &echo.EchoRequest{}
		x.NoError(t, proto.Unmarshal(f.GetPayload(), req))
		out = append(out, req.GetMessage())
	}
	return out
}

func TestFlow_SenderParksAtTheWindowAndResumesOnGrant(t *testing.T) {
	bubble(t, func(t *testing.T) {
		peer := &flowPeer{window: 4}
		conn := drpc.NewConn(peer, drpc.WithReliable(true))
		peer.conn = conn
		defer conn.Close(nil)

		stream, err := echo.NewEchoServiceClient(conn).Buff(t.Context())
		x.NoError(t, err) // the eager OPEN is never credited (§4.2.1)

		const burst = 8
		sent := make(chan error, 1)
		go func() {
			for i := range burst {
				m := echo.EchoRequest_builder{Message: fmt.Sprintf("m%d", i)}.Build()
				if err := stream.Send(m); err != nil {
					sent <- err
					return
				}
			}
			sent <- nil
		}()

		// The ack advertised 4 — authoritative, replacing the assumed W_init —
		// so exactly four messages reach the wire and the fifth parks.
		synctest.Wait()
		x.Equal(t, []string{"m0", "m1", "m2", "m3"}, peer.messages(t))

		// A grant releases exactly what it credits, and the order is the
		// application's, unchanged (§14).
		x.NoError(t, peer.grant(t.Context(), 2))
		synctest.Wait()
		x.Equal(t, []string{"m0", "m1", "m2", "m3", "m4", "m5"}, peer.messages(t))

		x.NoError(t, peer.grant(t.Context(), 16))
		x.NoError(t, <-sent)
		x.Equal(t, []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6", "m7"}, peer.messages(t))
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 advertisement path, asserted on the wire: the client advertises its
// per-call buffer on the OPEN, the server on its creation-ack H, and consumed
// messages come back as WINDOW grants — shape 16, seq 0, no payload (§7).
// ---------------------------------------------------------------------------

func TestFlow_AdvertisementsAndGrantsOnTheWire(t *testing.T) {
	const clientWindow, serverWindow = 48, 64 // both above the W_init floor

	client, stop := PipeOption{
		ConnOpts: []drpc.ConnOption{
			drpc.WithReliable(true), drpc.WithRxBuffer(clientWindow, drpc.DropNewest),
		},
		ServerOpts: []drpc.ServerOption{
			drpc.WithReliable(true), drpc.WithRxBuffer(serverWindow, drpc.DropNewest),
		},
	}.Use(t)
	defer stop()

	stream, err := client.Live(t.Context())
	x.NoError(t, err)

	// One request, half a client window of responses back: the grant is
	// batched at window/2, so consuming exactly that many elicits exactly one.
	const responses = clientWindow / 2
	x.NoError(t, stream.Send(echo.EchoRequest_builder{
		Message: "m",
		Repeat:  responses,
	}.Build()))
	for range responses {
		_, err := stream.Recv()
		x.NoError(t, err)
	}
	x.NoError(t, stream.CloseSend())
	_, err = stream.Recv()
	x.ErrorIs(t, err, io.EOF) // the terminal is behind the grant on the wire

	// The client's OPEN advertises its own rx buffer.
	open := firstMatch(client.txFrames(), func(f *drpc.Frame) bool {
		return f.GetFlags()&drpc.FlagOpen != 0
	})
	x.True(t, open != nil, "the call must have opened")
	x.Equal(t, uint32(clientWindow), open.GetWindow())

	// The creation ack H advertises the server's (§8: H is flags 0, no
	// payload).
	ack := firstMatch(client.rxFrames(), func(f *drpc.Frame) bool {
		return f.GetFlags() == 0 && !f.HasPayload()
	})
	x.True(t, ack != nil, "a streaming call must be acked")
	x.Equal(t, uint32(serverWindow), ack.GetWindow())

	// And the consumed messages came back as one WINDOW grant of window/2.
	grant := firstMatch(client.txFrames(), isWindowFrame)
	x.True(t, grant != nil, "consuming half a window must grant credit")
	x.Equal(t, open.GetSid(), grant.GetSid())
	x.Equal(t, uint32(responses), grant.GetWindow())
}

// ---------------------------------------------------------------------------
// §4.2.1 / §10.1: a park is bounded by T_stall. Reliable mode runs no protocol
// timers and the park sits before the adapter's write path, so this bound is
// the only thing that can break a sender whose peer never grants — the call
// fails UNAVAILABLE, it does not hang.
// ---------------------------------------------------------------------------

func TestFlow_StallBoundFailsUnavailable(t *testing.T) {
	bubble(t, func(t *testing.T) {
		const stall = 2 * time.Second
		events := &flowEvents{}

		// A peer that advertises one message of credit and then goes quiet: no
		// grants, no terminal, nothing.
		peer := &flowPeer{window: 1}
		conn := drpc.NewConn(peer,
			drpc.WithReliable(true),
			drpc.WithTiming(drpc.Timing{Stall: stall}),
			drpc.WithProtocolStats(events))
		peer.conn = conn
		defer conn.Close(nil)

		stream, err := echo.NewEchoServiceClient(conn).Buff(t.Context())
		x.NoError(t, err)

		msg := echo.EchoRequest_builder{Message: "m"}.Build()
		x.NoError(t, stream.Send(msg)) // spends the single advertised credit

		start := time.Now()
		err = stream.Send(msg)
		x.Equal(t, codes.Unavailable, status.Code(err))
		x.Equal(t, stall, time.Since(start), "the park must end exactly at T_stall")
		x.True(t, events.count(drpc.EventFlowStall) > 0, "the stall must be observable")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 scope: unreliable mode ignores window and WINDOW entirely — a peer
// that cannot be trusted to retransmit cannot be paced, and there a full
// buffer drops by policy (§4.2). So the OPEN carries no advertisement, and an
// injected grant must change nothing: it cannot switch flow control on (§15),
// and it draws no RESET.
// ---------------------------------------------------------------------------

func TestFlow_UnreliableModeIgnoresWindow(t *testing.T) {
	bubble(t, func(t *testing.T) {
		// The first server frame (the creation ack) is followed by a forged
		// grant of one message, addressed at the same live call.
		var once atomic.Bool
		inject := func(next drpc.FrameHandler) drpc.FrameHandler {
			return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
				if err := next.Handle(ctx, f); err != nil {
					return err
				}
				if !once.CompareAndSwap(false, true) {
					return nil
				}
				g := &drpc.Frame{}
				g.SetEpoch(f.GetEpoch())
				g.SetPeerEpoch(f.GetPeerEpoch())
				g.SetSid(f.GetSid())
				g.SetFlags(drpc.FlagWindow)
				g.SetWindow(1)
				return next.Handle(ctx, g)
			})
		}
		client, stop := unreliablePipe(nil, inject).Use(t)
		defer stop()

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)

		// Let the creation ack — and the grant riding its coattails — land
		// BEFORE anything is sent, so the sends really do run after the
		// forgery (§4.2.1: only an advertisement may enable flow control).
		synctest.Wait()
		x.True(t, once.Load(), "the forged grant must have been delivered")

		// Well past W_init: a sender that had (wrongly) adopted the forged
		// grant would park after one message and fail at T_stall.
		for i := range 40 {
			x.NoError(t, stream.Send(echo.EchoRequest_builder{
				Message: fmt.Sprintf("m%d", i),
			}.Build()))
		}
		_, err = stream.CloseAndRecv()
		x.NoError(t, err)

		// The OPEN of an unreliable-mode call carries no advertisement...
		open := firstMatch(client.txFrames(), func(f *drpc.Frame) bool {
			return f.GetFlags()&drpc.FlagOpen != 0
		})
		x.True(t, open != nil, "the call must have opened")
		x.Equal(t, uint32(0), open.GetWindow())

		// ...this side never grants credit it does not track...
		x.True(t, firstMatch(client.txFrames(), isWindowFrame) == nil,
			"unreliable mode must not send WINDOW frames")

		// ...and the forged grant was dropped in silence, not answered (§9.3).
		x.True(t, firstMatch(client.txFrames(), func(f *drpc.Frame) bool {
			return f.GetFlags()&drpc.FlagReset != 0
		}) == nil, "a stray WINDOW must never draw a RESET")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §9.3: a grant legitimately races the end of its call, so a WINDOW
// for an unknown, finished or tombstoned sid is dropped SILENTLY. Answering it
// with a RESET would turn every well-behaved stream's last grant into a RESET
// exchange — and hand an off-path attacker a free amplifier (§15).
// ---------------------------------------------------------------------------

// windowFrame builds a grant: shape WINDOW, seq 0, no payload (§7). A
// server-bound one needs no peer_epoch echo (pass 0); a client-bound one must
// name the incarnation it addresses or the Conn refuses it (§6.1).
func windowFrame(epoch, peerEpoch, sid, credit uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetPeerEpoch(peerEpoch)
	f.SetSid(sid)
	f.SetFlags(drpc.FlagWindow)
	f.SetWindow(credit)
	return f
}

func TestFlow_WindowForUnknownSidDrawsNoReset(t *testing.T) {
	t.Run("server", func(t *testing.T) {
		is := newInjectServer(t) // reliable: replies are immediate and synchronous
		const epoch, sid = uint32(0xC0FFEE), uint32(42)

		is.handle(windowFrame(epoch, 0, sid, 4))
		select {
		case f := <-is.out:
			t.Fatalf("a WINDOW for an unknown sid must be silent, got: %v", f)
		default:
		}

		// Control: the same unknown sid DOES draw a RESET for a data frame, so
		// the silence above is the rule at work, not a deaf harness (§9.3).
		is.handle(lcData(epoch, sid, 2, nil))
		r := is.recv(t)
		x.True(t, r != nil && r.GetFlags() == drpc.FlagReset, "a stray data frame draws a RESET")

		// A finished call's sid is just as silent: the grant the client sent
		// for its last consumed message arrives after the terminal.
		is.handle(openFrame(epoch, 7, 1, echo.EchoService_Once_FullMethodName))
		term := is.recv(t)
		x.True(t, term != nil && isTerminal(term), "the unary call must complete")
		is.handle(windowFrame(epoch, 0, term.GetSid(), 4))
		select {
		case f := <-is.out:
			t.Fatalf("a WINDOW for a finished sid must be silent, got: %v", f)
		default:
		}
	})
	t.Run("client", func(t *testing.T) {
		frames := make(chan *drpc.Frame, 8)
		conn := drpc.NewConn(drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
			frames <- proto.CloneOf(f)
			return nil
		}), drpc.WithReliable(true))
		defer conn.Close(nil)

		ctx, cancel := context.WithCancel(t.Context())
		_, err := echo.NewEchoServiceClient(conn).Buff(ctx)
		x.NoError(t, err)
		open := <-frames // the eager OPEN names this incarnation (§6.1)

		// An unknown sid: no call, no tombstone, nothing.
		x.NoError(t, conn.Handle(t.Context(),
			windowFrame(flowPeerEpoch, open.GetEpoch(), open.GetSid()+1, 4)))
		select {
		case f := <-frames:
			t.Fatalf("a WINDOW for an unknown sid must be silent, got: %v", f)
		default:
		}

		// And the call's own sid once it has finished: the abort ends it, and
		// a grant that was already in flight must not restart the exchange.
		cancel()
		abort := <-frames
		x.True(t, isTerminal(abort), "the cancelled call aborts (§10.3)")
		x.NoError(t, conn.Handle(t.Context(),
			windowFrame(flowPeerEpoch, open.GetEpoch(), open.GetSid(), 4)))
		select {
		case f := <-frames:
			t.Fatalf("a WINDOW for a finished sid must be silent, got: %v", f)
		default:
		}
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §4.4: credit is taken per data frame, so a frame the adapter
// refuses synchronously (ErrMessageTooLarge) — one that never reached the
// wire — must give its credit back. Without the refund a handler that ignores
// Send errors, as gRPC allows, leaks its whole window and then parks on every
// further message until T_stall.
// ---------------------------------------------------------------------------

func TestFlow_RefusedSendRefundsCredit(t *testing.T) {
	bubble(t, func(t *testing.T) {
		const refused = 40 // > W_init: a leak would run the window dry
		const delivered = 3
		events := &flowEvents{}

		var conn *drpc.Conn
		srv := drpc.NewServer(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			if len(f.GetPayload()) > 64 {
				return fmt.Errorf("refused %d bytes: %w", len(f.GetPayload()), drpc.ErrMessageTooLarge)
			}
			return conn.Handle(ctx, f)
		}),
			drpc.WithReliable(true),
			drpc.WithTiming(drpc.Timing{Stall: time.Second}),
			drpc.WithProtocolStats(events),
			// A handler that ignores what Send returns: legal, and the case the
			// refund exists for. Written as an interceptor so it replaces the
			// echo handler, which propagates Send errors.
			drpc.StreamInterceptor(func(_ any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
				big := echo.EchoResponse_builder{Message: strings.Repeat("x", 128)}.Build()
				for range refused {
					_ = ss.SendMsg(big)
				}
				for i := range delivered {
					_ = ss.SendMsg(echo.EchoResponse_builder{Sequence: uint32(i)}.Build())
				}
				return nil
			}))
		defer srv.Stop()
		echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

		conn = drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			return srv.Handle(ctx, f)
		}), drpc.WithReliable(true))
		defer conn.Close(nil)

		stream, err := echo.NewEchoServiceClient(conn).Many(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)

		var got []uint32
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err)
			got = append(got, res.GetSequence())
		}
		x.Equal(t, []uint32{0, 1, 2}, got, "every refusal must return its credit")
		x.Equal(t, 0, events.count(drpc.EventFlowStall), "no send ever ran out of credit")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 initial window: a sender paces itself by W_init until the peer's
// advertisement lands, so a reliable-mode receiver MUST buffer at least
// W_init — a smaller configured buffer is raised to it. And once flow control
// is on the receiver NEVER blocks: a full buffer means the peer overran the
// window it was granted, which fails THAT call with INTERNAL (§4.2).
// ---------------------------------------------------------------------------

// flowBuffOpen builds the eager, bare OPEN of a client-streaming call,
// advertising the client's window (§8, §4.2.1).
func flowBuffOpen(epoch, sid, window uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen)
	f.SetMethod(echo.EchoService_Buff_FullMethodName)
	f.SetWindow(window)
	return f
}

func TestFlow_ReliableRxBufferFloorAndOverrun(t *testing.T) {
	// The handler never consumes, so nothing is ever granted and the buffer
	// can only fill.
	is := newInjectServer(t,
		drpc.WithRxBuffer(2, drpc.DropNewest),
		drpc.StreamInterceptor(func(_ any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
			<-ss.Context().Done()
			return nil
		}))
	const epoch, sid = uint32(1), uint32(3)

	is.handle(flowBuffOpen(epoch, sid, wInitTest))
	ack := is.recv(t)
	x.True(t, ack != nil, "a streaming call must be acked")
	x.Equal(t, wInitTest, ack.GetWindow(), "the configured 2 is raised to W_init")

	// A client running on the assumption may send W_init messages before that
	// ack can reach it: every one of them must fit.
	for i := range wInitTest {
		is.handle(lcData(epoch, sid, 2+i, nil))
	}
	select {
	case f := <-is.out:
		t.Fatalf("W_init messages must fit the floored buffer, got: %v", f)
	default:
	}

	// One past the window is a contract violation. The receiver does not block
	// — blocking is what flow control removes — it fails that call loudly.
	is.handle(lcData(epoch, sid, 2+wInitTest, nil))
	term := is.recv(t)
	x.True(t, term != nil, "the overrun must fail the call")
	x.Equal(t, drpc.FlagClose, term.GetFlags())
	x.Equal(t, codes.Internal, codes.Code(term.GetCode()))
}
