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
//     assumption safe (§4.2.1, Appendix B);
//   - the connection window beside the per-stream one (§4.2.1, §15), end to
//     end and cross-cutting: sid 0 silent in unreliable mode and stateless in
//     reliable mode, the raise, the single T_stall budget across both windows,
//     no credit held while parked on the other window, and the leak test —
//     one credit back for every data frame, over a long-lived Conn. The client
//     and server halves against scripted peers are flow_peer_client_test.go
//     and flow_peer_server_test.go.

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
	"google.golang.org/grpc/metadata"
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

// ---------------------------------------------------------------------------
// Connection window (§4.2.1, §15): the end-to-end and cross-cutting twins on
// this file's harnesses — the pipe, the crafted peer, the inject server. Each
// pins one sentence of §4.2.1 and says which.
// ---------------------------------------------------------------------------

// gateStreams is a stream interceptor that holds every streaming handler
// until gate is closed — a consumer that has not started reading yet — and
// then runs it.
func gateStreams(gate <-chan struct{}) drpc.ServerOption {
	return drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		select {
		case <-gate:
			return handler(srv, ss)
		case <-ss.Context().Done():
			return nil
		}
	})
}

// spendWConn opens W_conn/W_init client-streaming calls and sends exactly
// W_init on each: the whole connection window, with no stream window ever
// full — so whatever parks next parks on the connection window alone, and
// the events say so unambiguously.
func spendWConn(t *testing.T, client echo.EchoServiceClient) []echo.EchoService_BuffClient {
	t.Helper()
	streams := make([]echo.EchoService_BuffClient, 0, wConnTest/wInitTest)
	for range wConnTest / wInitTest {
		s, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, s, int(wInitTest))
		streams = append(streams, s)
	}
	return streams
}

// halfCloseFrame builds a client half-close: CLOSE without a code (§8).
func halfCloseFrame(epoch, sid, seq uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	f.SetFlags(drpc.FlagClose)
	return f
}

// Pins §4.2.1 Unreliable mode: "no assumption, no ledger, no raise, and a
// WINDOW sid = 0 is dropped like every other WINDOW there."
func TestPeerWindow_SilentInUnreliableMode(t *testing.T) {
	bubble(t, func(t *testing.T) {
		// A sid-0 grant of one message forged in each direction, right behind
		// the first frame each way. The OPEN names the client incarnation and
		// the creation ack echoes it (§6.1), so neither forgery is dropped for
		// its epoch: only the mode can drop them.
		forge := func(once *atomic.Bool) func(drpc.FrameHandler) drpc.FrameHandler {
			return func(next drpc.FrameHandler) drpc.FrameHandler {
				return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
					if err := next.Handle(ctx, f); err != nil {
						return err
					}
					if !once.CompareAndSwap(false, true) {
						return nil
					}
					return next.Handle(ctx, windowFrame(f.GetEpoch(), f.GetPeerEpoch(), 0, 1))
				})
			}
		}
		var c2s, s2c atomic.Bool
		clientEvents, serverEvents := &flowEvents{}, &flowEvents{}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{
				drpc.WithReliable(false), drpc.WithTiming(fastTiming), drpc.WithProtocolStats(clientEvents),
			},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(false), drpc.WithTiming(fastTiming), drpc.WithProtocolStats(serverEvents),
			},
			C2S: forge(&c2s),
			S2C: forge(&s2c),
		}.Use(t)
		defer stop()

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		synctest.Wait() // the OPEN, its ack and both forgeries have landed
		x.True(t, c2s.Load() && s2c.Load(), "both forgeries must have been delivered")

		// Well past what a (wrongly) adopted one-message window would allow,
		// in the client's direction...
		const burst = 40
		sendN(t, stream, burst)
		_, err = stream.CloseAndRecv()
		x.NoError(t, err)
		// ...and in the server's.
		many, err := client.Many(t.Context(), echo.EchoRequest_builder{Message: "m", Repeat: burst}.Build())
		x.NoError(t, err)
		n := 0
		for {
			if _, err := many.Recv(); err == io.EOF {
				break
			} else {
				x.NoError(t, err)
			}
			n++
		}
		x.Equal(t, burst, n)
		for _, ev := range []*flowEvents{clientEvents, serverEvents} {
			x.Equal(t, 0, ev.count(drpc.EventPeerFlowStall), "no connection window to park on")
			x.Equal(t, 0, ev.count(drpc.EventFlowStall))
		}

		// Neither side answered its forgery (§9.3), and neither grants on sid
		// 0 itself: the forgery is the only WINDOW on the wire each way.
		tx, rx := client.txFrames(), client.rxFrames()
		x.Equal(t, 0, countMatch(tx, isResetFrame), "the server must not RESET a stray sid-0 grant")
		x.Equal(t, 0, countMatch(rx, isResetFrame), "the client must not RESET a stray sid-0 grant")
		x.Equal(t, 1, countMatch(tx, isWindowFrame), "unreliable mode sends no WINDOW frames")
		x.Equal(t, 1, countMatch(rx, isWindowFrame))
		x.True(t, isPeerGrant(firstMatch(tx, isWindowFrame)) && isPeerGrant(firstMatch(rx, isWindowFrame)),
			"the one WINDOW each way is the forgery")
	})
}

// Pins §4.2.1 Grants: a sid-0 WINDOW the receiver holds no container for is
// dropped "never validated (§9.1), never answered with a RESET (§9.3), never
// creating state" — and it never enables: the call that follows runs on its
// per-stream window exactly as it would have without the stray grant. (The
// white-box half, no container and no ledger, is server_internal_test.go.)
func TestPeerWindow_Sid0NeverEnablesOrCreatesState(t *testing.T) {
	is := newInjectServer(t) // reliable: replies are immediate and synchronous
	const epoch, sid = uint32(0xC0FFEE), uint32(3)

	// Before any OPEN: silence.
	is.handle(windowFrame(epoch, 0, 0, 4))
	select {
	case f := <-is.out:
		t.Fatalf("a sid-0 WINDOW before any OPEN must be silent, got: %v", f)
	default:
	}

	// The OPEN that follows is admitted as usual...
	is.handle(flowBuffOpen(epoch, sid, wInitTest))
	ack := is.recv(t)
	x.True(t, ack != nil && isAckH(ack), "the call must be acked, got ", ack)
	x.Equal(t, wInitTest, ack.GetWindow())

	// ...and its 40-message Buff runs on per-stream grants alone — one per
	// half window consumed, on the call's own sid, which this scripted client
	// honours like a real one — with nothing on sid 0: at the default
	// MaxPeerWindow there is no raise, and 40 consumed is far from a batch.
	const n = 40
	item, err := proto.Marshal(echo.EchoRequest_builder{Message: "m", Repeat: 1}.Build())
	x.NoError(t, err)
	for i := range wInitTest {
		is.handle(lcData(epoch, sid, 2+i, item)) // exactly the advertised window
	}
	g := is.recv(t)
	x.True(t, g != nil && isWindowFrame(g), "half a window consumed grants half a window, got ", g)
	x.Equal(t, sid, g.GetSid(), "a per-stream grant, on the call's own sid")
	x.Equal(t, wInitTest/2, g.GetWindow())
	for i := wInitTest; i < n; i++ {
		is.handle(lcData(epoch, sid, 2+i, item)) // within the grant
	}
	is.handle(halfCloseFrame(epoch, sid, 2+n))
	grants := []*drpc.Frame{g}
	var term *drpc.Frame
	for term == nil {
		f := is.recv(t)
		x.True(t, f != nil, "the call must complete")
		switch {
		case isTerminal(f):
			term = f
		case isWindowFrame(f):
			grants = append(grants, f)
		default:
			t.Fatalf("unexpected frame: %v", f)
		}
	}
	x.Equal(t, codes.OK, codes.Code(term.GetCode()))
	res := &echo.EchoBatchResponse{}
	x.NoError(t, proto.Unmarshal(term.GetPayload(), res))
	x.Equal(t, n, len(res.GetItems()))
	x.Equal(t, 2, len(grants), "40 consumed against a window of 32: two grants of 16, none on sid 0")
	for _, g := range grants {
		x.Equal(t, sid, g.GetSid(), "every grant is a per-stream one")
		x.Equal(t, wInitTest/2, g.GetWindow())
	}
}

// Pins §4.2.1 Raise: "A receiver whose MaxPeerWindow exceeds W_conn MUST lift
// the sender's assumption once per peer incarnation with a sid = 0 grant of
// MaxPeerWindow − W_conn" — the server right behind its first H, the client
// right behind its first OPEN — and "A receiver at the floor sends none."
func TestPeerWindow_Raise(t *testing.T) {
	const window = 2 * wConnTest
	limits := drpc.WithLimits(drpc.Limits{MaxPeerWindow: window})
	isOpen := func(f *drpc.Frame) bool { return f.GetFlags()&drpc.FlagOpen != 0 }

	t.Run("server: behind the first H, naming the client incarnation", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			client, stop := PipeOption{
				ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true)},
				ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), limits},
			}.Use(t)
			defer stop()

			a, err := client.Buff(t.Context())
			x.NoError(t, err)
			b, err := client.Buff(t.Context())
			x.NoError(t, err)
			synctest.Wait()

			// Exactly: the first call's ack, the raise, the second call's ack.
			rx := client.rxFrames()
			x.Equal(t, 3, len(rx), "got ", rx)
			x.True(t, isAckH(rx[0]) && rx[0].GetSid() == 1, "the first H, got ", rx[0])
			raise := rx[1]
			x.True(t, isPeerGrant(raise), "the raise rides right behind the first H, got ", raise)
			x.Equal(t, uint32(window-wConnTest), raise.GetWindow())
			x.Equal(t, rx[0].GetEpoch(), raise.GetEpoch(), "the server's own epoch")
			x.Equal(t, firstMatch(client.txFrames(), isOpen).GetEpoch(), raise.GetPeerEpoch(),
				"names the client incarnation it lifts (§6.1)")
			x.True(t, isAckH(rx[2]) && rx[2].GetSid() == 2, "the second H draws no raise, got ", rx[2])

			for _, s := range []echo.EchoService_BuffClient{a, b} {
				_, err := s.CloseAndRecv()
				x.NoError(t, err)
			}
		})
	})
	t.Run("client: behind the first OPEN", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			client, stop := PipeOption{
				ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true), limits},
				ServerOpts: []drpc.ServerOption{drpc.WithReliable(true)},
			}.Use(t)
			defer stop()

			a, err := client.Buff(t.Context())
			x.NoError(t, err)
			b, err := client.Buff(t.Context())
			x.NoError(t, err)
			synctest.Wait()

			// Exactly: the first OPEN, the raise, the second OPEN.
			tx := client.txFrames()
			x.Equal(t, 3, len(tx), "got ", tx)
			x.True(t, isOpen(tx[0]) && tx[0].GetSid() == 1, "the first OPEN, got ", tx[0])
			raise := tx[1]
			x.True(t, isPeerGrant(raise), "the raise rides right behind the first OPEN, got ", raise)
			x.Equal(t, uint32(window-wConnTest), raise.GetWindow())
			x.Equal(t, tx[0].GetEpoch(), raise.GetEpoch(), "the client's own epoch")
			x.True(t, isOpen(tx[2]) && tx[2].GetSid() == 2, "the second OPEN draws no raise, got ", tx[2])

			for _, s := range []echo.EchoService_BuffClient{a, b} {
				_, err := s.CloseAndRecv()
				x.NoError(t, err)
			}
		})
	})
	t.Run("honoured: exactly MaxPeerWindow frames go out unparked", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			client, stop := PipeOption{
				ConnOpts: []drpc.ConnOption{drpc.WithReliable(true), drpc.WithProtocolStats(events)},
				// A per-stream window above MaxPeerWindow and a handler that
				// never reads: only the connection window can bind, and the
				// park is unambiguously its.
				ServerOpts: []drpc.ServerOption{
					drpc.WithReliable(true), limits, drpc.WithRxBuffer(2*window, drpc.DropNewest), blockStreams(),
				},
			}.Use(t)
			defer stop()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream, err := client.Buff(ctx)
			x.NoError(t, err)
			synctest.Wait() // the ack and the raise have landed
			sendN(t, stream, window)
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "W_conn + the raise go out unparked")
			x.Equal(t, 0, events.count(drpc.EventFlowStall))

			done := make(chan error, 1)
			go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
			defer func() { <-done }()
			synctest.Wait()
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "the next one parks: the lifted window is exact")
			x.Equal(t, 0, events.count(drpc.EventFlowStall))
			x.Equal(t, window, countMatch(client.txFrames(), isDataFrame))
			cancel() // releases the parked send
		})
	})
	t.Run("at the floor: none, and W_conn still fits", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			// MaxPeerWindow below W_conn on both sides is raised to it: a
			// receiver holding less than a sender assumes would be overrun
			// by a conforming sender (§4.2.1 Assumption).
			floored := drpc.WithLimits(drpc.Limits{MaxPeerWindow: 100})
			events := &flowEvents{}
			gate := make(chan struct{})
			client, stop := PipeOption{
				ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true), floored, drpc.WithProtocolStats(events)},
				ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), floored, gateStreams(gate)},
			}.Use(t)
			defer stop()

			streams := spendWConn(t, client) // W_conn messages buffered across 32 calls
			synctest.Wait()
			x.Equal(t, 0, countMatch(client.txFrames(), isPeerGrant), "nothing to raise by")
			x.Equal(t, 0, countMatch(client.rxFrames(), isPeerGrant))
			x.Equal(t, 0, countMatch(client.rxFrames(), isTerminal), "W_conn unread messages fit: no INTERNAL")
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))
			x.Equal(t, 0, events.count(drpc.EventFlowStall))

			close(gate)
			for _, s := range streams {
				_, err := s.CloseAndRecv()
				x.NoError(t, err)
			}
		})
	})
}

// Pins §4.2.1 Raise: the client raises "right behind its first OPEN (the OPEN
// creates the container the grant addresses, admitted or rejected — §9.4)".
func TestPeerWindow_RaiseLandsAfterARejectedFirstOpen(t *testing.T) {
	bubble(t, func(t *testing.T) {
		const window = 2 * wConnTest
		events := &flowEvents{}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{
				drpc.WithReliable(true),
				drpc.WithLimits(drpc.Limits{MaxPeerWindow: window}),
				drpc.WithRxBuffer(window, drpc.DropNewest), // the stream window never binds
			},
			ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), drpc.WithProtocolStats(events)},
		}.Use(t)
		defer stop()

		// The Conn's first OPEN names a streaming method the server does not
		// have: T{UNIMPLEMENTED}, no call — and, since the OPEN was validated,
		// a container for this incarnation (§9.4).
		nope, err := client.conn.NewStream(t.Context(),
			&grpc.StreamDesc{StreamName: "Nope", ServerStreams: true}, "/echo.EchoService/Nope")
		x.NoError(t, err)
		x.NoError(t, nope.SendMsg(&echo.EchoRequest{}))
		err = nope.RecvMsg(&echo.EchoResponse{})
		x.Equal(t, codes.Unimplemented, status.Code(err))
		synctest.Wait() // the pump has delivered everything the client sent
		tx := client.txFrames()
		x.True(t, len(tx) >= 2 && tx[0].GetFlags()&drpc.FlagOpen != 0 && isPeerGrant(tx[1]),
			"the raise rides right behind the rejected OPEN, got ", tx)

		// 1500 unread responses: past W_conn, within the lifted window. The
		// server's sender toward this incarnation runs on the raise the
		// rejection gave a home to — a server that had dropped it would park
		// at 1024.
		const burst = wConnTest + 476
		many, err := client.Many(t.Context(), echo.EchoRequest_builder{Message: "m", Repeat: burst}.Build())
		x.NoError(t, err)
		synctest.Wait()
		x.Equal(t, burst, countMatch(client.rxFrames(), isDataFrame))
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "no park at W_conn")

		n := 0
		for {
			if _, err := many.Recv(); err == io.EOF {
				break
			} else {
				x.NoError(t, err)
			}
			n++
		}
		x.Equal(t, burst, n)
	})
}

// readHalfWindowWhenMarked is a stream interceptor: the call carrying the
// metadata key waits for gate, reads half a window and stops; every other
// call never reads at all.
func readHalfWindowWhenMarked(key string, gate <-chan struct{}) drpc.ServerOption {
	return drpc.StreamInterceptor(func(_ any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
		if md, _ := metadata.FromIncomingContext(ss.Context()); len(md.Get(key)) > 0 {
			select {
			case <-gate:
			case <-ss.Context().Done():
				return nil
			}
			for range wInitTest / 2 {
				if err := ss.RecvMsg(&echo.EchoRequest{}); err != nil {
					return nil
				}
			}
		}
		<-ss.Context().Done()
		return nil
	})
}

// Pins §4.2.1 Sending: "One park, one bound: the same T_stall (§10.1), armed
// at the first park, measures the whole wait across both windows, and on
// expiry the call fails UNAVAILABLE naming the window it was parked on."
func TestPeerWindow_SingleStallBudget(t *testing.T) {
	const stall = 2 * time.Second
	msg := echo.EchoRequest_builder{Message: "m"}.Build()
	// The server never grants on sid 0: its connection grants are dropped on
	// the wire, so W_conn is all the connection credit the client ever has.
	noPeerGrants := dropEvery(isPeerGrant)

	t.Run("parked on the connection window", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			gate := make(chan struct{})
			client, stop := PipeOption{
				ConnOpts: []drpc.ConnOption{
					drpc.WithReliable(true), drpc.WithTiming(drpc.Timing{Stall: stall}), drpc.WithProtocolStats(events),
				},
				ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), gateStreams(gate)},
				S2C:        noPeerGrants,
			}.Use(t)
			defer stop()

			streams := spendWConn(t, client)
			x.Equal(t, 0, events.count(drpc.EventFlowStall))
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))

			// The 1025th streamed send, on a fresh call whose own window is
			// untouched: it parks on the connection window and nothing else.
			extra, err := client.Buff(t.Context())
			x.NoError(t, err)
			start := time.Now()
			err = extra.Send(msg)
			x.Equal(t, codes.Unavailable, status.Code(err))
			x.True(t, strings.Contains(status.Convert(err).Message(), "connection credit"),
				"the error must name the window that starved it, got: ", err)
			x.Equal(t, stall, time.Since(start), "the park ends exactly at T_stall")
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))
			x.Equal(t, 0, events.count(drpc.EventFlowStall), "the stream window had credit")

			// The failed send aborted its call (gRPC: a SendMsg error is
			// terminal); the 32 others complete once their consumers read.
			close(gate)
			for _, s := range streams {
				_, err := s.CloseAndRecv()
				x.NoError(t, err)
			}
		})
	})
	t.Run("a park that moves from the stream window to the connection window", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			const key = "flow-later"
			events := &flowEvents{}
			gate := make(chan struct{})
			client, stop := PipeOption{
				ConnOpts: []drpc.ConnOption{
					drpc.WithReliable(true), drpc.WithTiming(drpc.Timing{Stall: stall}), drpc.WithProtocolStats(events),
				},
				ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), readHalfWindowWhenMarked(key, gate)},
				S2C:        noPeerGrants,
			}.Use(t)
			defer stop()

			// 31 stuck consumers hold 992; the marked call's own 32 make it
			// W_conn: both windows are now empty for it, and a park short on
			// both is the connection window's — the peer's whole budget is
			// what it waits on, not one consumer (§14).
			for range wConnTest/wInitTest - 1 {
				s, err := client.Buff(t.Context())
				x.NoError(t, err)
				sendN(t, s, int(wInitTest))
			}
			marked, err := client.Buff(metadata.AppendToOutgoingContext(t.Context(), key, "1"))
			x.NoError(t, err)
			sendN(t, marked, int(wInitTest))
			synctest.Wait()

			// The 33rd parks on its stream window's waiter, holding no credit
			// at all, and the event names the connection window.
			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- marked.Send(msg) }()
			synctest.Wait()
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "short on both: the connection one is reported")
			x.Equal(t, 0, events.count(drpc.EventFlowStall))

			// Half-way through the budget its consumer reads half a window:
			// the per-stream grant lands, the connection grant is dropped, and
			// the send moves to the connection park — the budget does not
			// restart.
			time.Sleep(stall / 2)
			close(gate)
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("still short on the connection window, must stay parked, got %v", err)
			default:
			}
			x.True(t, firstMatch(client.rxFrames(), func(f *drpc.Frame) bool {
				return isWindowFrame(f) && f.GetSid() != 0
			}) != nil, "the per-stream grant did land")

			err = <-done
			x.Equal(t, codes.Unavailable, status.Code(err))
			x.True(t, strings.Contains(status.Convert(err).Message(), "connection credit"),
				"the error names the window it was parked on at expiry, got: ", err)
			x.Equal(t, stall, time.Since(start), "one budget, from the FIRST park")
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "one park, one stall event")
			x.Equal(t, 0, events.count(drpc.EventFlowStall))
			x.Equal(t, 0, events.count(drpc.EventFlowResume)+events.count(drpc.EventPeerFlowResume), "it never resumed")
		})
	})
}

// Pins §4.2.1 Sending: "A sender MUST NOT hold one window's credit while
// parked on the other".
func TestPeerWindow_AcquireHoldsNoCreditWhileParked(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		// Every stream window is one message; the connection window is at
		// the floor.
		srv, conn, client := clientFixture(t, srvEpochA, 1, events)
		defer conn.Close(nil)
		msg := echo.EchoRequest_builder{Message: "m"}.Build()

		stuck, err := client.Buff(t.Context())
		x.NoError(t, err)
		healthy, err := client.Buff(t.Context())
		x.NoError(t, err)
		sidHealthy := srv.lastOpen()

		// The stuck stream spends its one credit and parks on its own window.
		x.NoError(t, stuck.Send(msg))
		stuckDone := make(chan error, 1)
		go func() { stuckDone <- stuck.Send(msg) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventFlowStall))
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))

		// The healthy one, granted a message at a time by its consumer, gets
		// every connection credit the stuck one did not spend: W_conn − 1 of
		// them. Had the parked send kept the connection credit it took before
		// finding its stream window empty, the healthy stream would park one
		// message early.
		for i := range wConnTest - 1 {
			if err := healthy.Send(msg); err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
			x.NoError(t, conn.Handle(t.Context(), windowFrame(srvEpochA, srv.client(), sidHealthy, 1)))
		}
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "W_conn − 1 sends: no connection credit was hoarded")
		x.Equal(t, wConnTest, countMatch(srv.txFrames(), isDataFrame))

		// Now the connection window is genuinely empty: the healthy stream,
		// with a stream credit in hand, parks on it — not one frame past
		// W_conn.
		healthyDone := make(chan error, 1)
		go func() { healthyDone <- healthy.Send(msg) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))
		x.Equal(t, wConnTest, countMatch(srv.txFrames(), isDataFrame))

		// A connection grant releases exactly the healthy stream; the stuck
		// one is still short on its own window.
		srv.grant(t, srvEpochA, srv.client(), 1)
		x.NoError(t, <-healthyDone)
		synctest.Wait()
		select {
		case err := <-stuckDone:
			t.Fatalf("its stream window is still empty, must stay parked, got %v", err)
		default:
		}
		x.Equal(t, wConnTest+1, countMatch(srv.txFrames(), isDataFrame))

		conn.Close(nil)
		<-stuckDone
	})
}

// Pins §4.2.1 The receiver's ledger: a receiver "MUST return one credit for
// every reliable-mode data frame it received from that peer, once". The slow
// death this catches: over a long-lived Conn every frame a cancelled call left
// buffered, or sent after its end and drew a RESET for, is a permanent shrink
// unless it comes back — 32 cancelled calls of a stream window each would
// drain W_conn for good.
func TestPeerWindow_CancelledCallsLeakNoCredit(t *testing.T) {
	bubble(t, func(t *testing.T) {
		const cycles = 40 // × W_init = 1280 frames each way, past W_conn
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithReliable(true)},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(true),
				// Client-streaming handlers never read: whatever such a call
				// received is discarded with it at its end.
				drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
					if info.IsClientStream && !info.IsServerStream {
						<-ss.Context().Done()
						return nil
					}
					return handler(srv, ss)
				}),
			},
		}.Use(t)
		defer stop()

		// The ledger equation, on the wire: credited ≤ received (never twice)
		// and received − credited < MaxPeerWindow/2 (nothing lost — with
		// nothing buffered, all a receiver may still hold back is a batch
		// short of half its window).
		balanced := func(dir string, received int, credited uint32) {
			t.Helper()
			x.True(t, int(credited) <= received, dir, ": over-credited: received ", received, ", credited ", credited)
			x.True(t, received-int(credited) < wConnTest/2, dir, ": credit leaked: received ", received, ", credited ", credited)
		}

		// Client → server: a full stream window buffered at the server, then
		// the call is cancelled. A consume-only ledger parks the 33rd cycle
		// forever; this one may park briefly at the edge and resume.
		for range cycles {
			ctx, cancel := context.WithCancel(t.Context())
			s, err := client.Buff(ctx)
			x.NoError(t, err)
			sendN(t, s, int(wInitTest))
			cancel()
		}
		synctest.Wait()
		sent := countMatch(client.txFrames(), isDataFrame)
		x.Equal(t, cycles*int(wInitTest), sent)
		_, credited := peerGrants(client.rxFrames())
		balanced("client->server", sent, credited)

		// Server → client: responses pile up at the client, one is consumed,
		// then the call is cancelled — what is still buffered is discarded,
		// what the server sends after that draws a RESET (§9.3) — and every
		// one of them comes back.
		for range cycles {
			ctx, cancel := context.WithCancel(t.Context())
			s, err := client.Many(ctx, echo.EchoRequest_builder{Message: "m", Repeat: 2 * wInitTest}.Build())
			x.NoError(t, err)
			_, err = s.Recv()
			x.NoError(t, err)
			cancel()
		}
		synctest.Wait()
		received := countMatch(client.rxFrames(), isDataFrame)
		x.True(t, received >= cycles, "the server did send: ", received)
		_, credited = peerGrants(client.txFrames())
		balanced("server->client", received, credited)

		// And both senders are alive: a bidi call moves a window's worth
		// each way after all of it.
		live, err := client.Live(t.Context())
		x.NoError(t, err)
		for i := range wConnTest {
			x.NoError(t, live.Send(echo.EchoRequest_builder{Message: fmt.Sprint(i), Repeat: 1}.Build()))
			res, err := live.Recv()
			x.NoError(t, err)
			x.Equal(t, fmt.Sprint(i), res.GetMessage())
		}
		x.NoError(t, live.CloseSend())
		_, err = live.Recv()
		x.ErrorIs(t, err, io.EOF)
	})
}

// Pins §4.2.1 Restart: the Conn "locks ... on the first sequenced frame it
// hears from it, for a live call or for one it has already released" — so
// the raise that rides behind the creation ack of a call the client cancelled
// in the meantime still lands, end to end.
func TestPeerWindow_RaiseSurvivesACancelledFirstCall(t *testing.T) {
	bubble(t, func(t *testing.T) {
		// Above 2 × W_conn: a lost raise would be a forever-park, not a slow
		// one (no cadence of a 4096 receiver reaches a sender stuck at 1024).
		const window = 4 * wConnTest
		events := &flowEvents{}
		gate := make(chan struct{})
		// hold delays every server frame until the gate opens, in order.
		hold := func(next drpc.FrameHandler) drpc.FrameHandler {
			return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
				<-gate
				return next.Handle(ctx, f)
			})
		}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithReliable(true), drpc.WithProtocolStats(events)},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(true), drpc.WithLimits(drpc.Limits{MaxPeerWindow: window}),
			},
			S2C: hold,
		}.Use(t)
		defer stop()

		// The Conn's first streaming call is cancelled before the server's
		// creation ack — and the raise right behind it — reaches the client.
		ctx, cancel := context.WithCancel(t.Context())
		_, err := client.Buff(ctx)
		x.NoError(t, err)
		synctest.Wait() // the server answered: H(1) and the raise wait at the gate
		cancel()
		synctest.Wait()
		close(gate)
		synctest.Wait()
		rx := client.rxFrames()
		x.True(t, len(rx) >= 2 && isAckH(rx[0]) && rx[0].GetSid() == 1 && isPeerGrant(rx[1]),
			"the H, then the raise, got ", rx)
		x.Equal(t, uint32(window-wConnTest), rx[1].GetWindow())
		x.True(t, countMatch(client.txFrames(), isResetFrame) >= 1, "the H found no call: a RESET")

		// Past W_conn on the next call, nothing parks: the raise was honoured
		// although the H it followed landed on no call.
		s, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, s, int(wConnTest+500))
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "the raise landed")
		_, err = s.CloseAndRecv()
		x.NoError(t, err)
	})
}
