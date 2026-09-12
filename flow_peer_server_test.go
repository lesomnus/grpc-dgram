package drpc_test

// flow_peer_server_test.go pins the SERVER half of the connection window
// (PROTOCOL.md §4.2.1, reliable mode only) against a scripted client driving
// Server.Handle directly, and then the whole Go↔Go path end to end:
//
//   - the server paces itself per client incarnation by the window that
//     incarnation's OPEN advertised and parks — on the connection window, not
//     the stream window — bounded by T_stall;
//   - a sid-0 WINDOW credits only an existing (peer, client-epoch) container
//     and only in reliable mode; anything else is dropped in silence and
//     creates no state;
//   - the OPEN that creates the container — admitted or rejected — creates
//     its sender from the advertisement it carries: absent turns it off, and
//     a later OPEN's value is ignored;
//   - every H and T the server sends carries its MaxPeerWindow as
//     conn_window; nothing rides behind the first H on sid 0;
//   - an overrun fails the offending call INTERNAL and nothing else;
//   - every reliable-mode data frame received returns exactly one credit on
//     sid 0 — consumed, discarded with its call, or never buffered — except
//     the server-streaming request that rode the OPEN uncredited;
//   - the starvation clause: stuck consumers pinning most of the window do
//     not starve a healthy stream (grants fire below half the window);
//   - end to end: a park across streams keeps the channel live, and three
//     streams past W_conn both ways complete on sid-0 grants.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// srvFixture is a reliable Server whose tx frames are recorded without
// bound, so a scripted client can inject any number of frames and read back
// exactly what the server emitted. Handler goroutines belong to whatever
// bubble the fixture was built in.
type srvFixture struct {
	srv *drpc.Server

	mu sync.Mutex
	rx []*drpc.Frame // server -> client
}

func newSrvFixture(t *testing.T, opts ...drpc.ServerOption) *srvFixture {
	t.Helper()
	sf := &srvFixture{}
	tx := drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		sf.mu.Lock()
		defer sf.mu.Unlock()
		sf.rx = append(sf.rx, proto.CloneOf(f))
		return nil
	})
	sf.srv = drpc.NewServer(tx, append([]drpc.ServerOption{drpc.WithReliable(true)}, opts...)...)
	echo.RegisterEchoServiceServer(sf.srv, &echo.EchoServer{})
	t.Cleanup(sf.srv.Stop)
	return sf
}

func (sf *srvFixture) handle(f *drpc.Frame) { _ = sf.srv.Handle(context.Background(), f) }

func (sf *srvFixture) handleAs(peer any, f *drpc.Frame) {
	_ = sf.srv.Handle(drpc.NewPeerContext(context.Background(), peer), f)
}

func (sf *srvFixture) frames() []*drpc.Frame {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return append([]*drpc.Frame(nil), sf.rx...)
}

// blockStreams is a stream interceptor that never reads or writes — a
// consumer that stopped — for every call.
func blockStreams() drpc.ServerOption {
	return drpc.StreamInterceptor(func(_ any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
		<-ss.Context().Done()
		return nil
	})
}

// blockUnlessMarked is a stream interceptor that never reads or writes — a
// consumer that stopped — unless the call carries the given metadata key.
// Handler goroutines start in no particular order, so a count would not
// say which call is the healthy one; the client marks it.
func blockUnlessMarked(key string) drpc.ServerOption {
	return drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if md, _ := metadata.FromIncomingContext(ss.Context()); len(md.Get(key)) > 0 {
			return handler(srv, ss)
		}
		<-ss.Context().Done()
		return nil
	})
}

// streamOpen builds the eager, bare OPEN of a client-streaming or bidi call,
// advertising the client's stream window and, as every reliable-mode OPEN
// does, its connection window — W_conn unless a test overrides it (§8,
// §4.2.1).
func streamOpen(epoch, sid, window uint32, method string) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen)
	f.SetMethod(method)
	f.SetWindow(window)
	f.SetConnWindow(wConnTest)
	return f
}

// manyOpen builds a server-streaming OPEN|CLOSE asking for repeat responses,
// advertising the client's stream window and, as every reliable-mode OPEN
// does, its connection window — W_conn unless a test overrides it (§8,
// §4.2.1).
func manyOpen(epoch, sid, window, repeat uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen | drpc.FlagClose)
	f.SetMethod(echo.EchoService_Many_FullMethodName)
	f.SetWindow(window)
	f.SetConnWindow(wConnTest)
	data, _ := proto.Marshal(echo.EchoRequest_builder{Message: "m", Repeat: repeat}.Build())
	f.SetPayload(data)
	return f
}

// abortFrame builds a client abort: a terminal on the call (§10.3).
func abortFrame(epoch, sid, seq uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	f.SetFlags(drpc.FlagClose)
	f.SetCode(uint32(codes.Canceled))
	return f
}

func dataOn(sid uint32) func(*drpc.Frame) bool {
	return func(f *drpc.Frame) bool { return isDataFrame(f) && f.GetSid() == sid }
}

func terminalOn(sid uint32) func(*drpc.Frame) bool {
	return func(f *drpc.Frame) bool { return isTerminal(f) && f.GetSid() == sid }
}

const clientEpochA, clientEpochB = uint32(0xC1A), uint32(0xC1B)

// ---------------------------------------------------------------------------
// §4.2.1 sending: the W_conn the client's OPENs advertise is spent across
// every call to one client incarnation, then the handler parks on the
// CONNECTION window — its stream window still has credit — resumes on a
// sid-0 grant, and fails UNAVAILABLE at T_stall naming that window.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Scope: "the sender's credit is per peer incarnation: on the
// server one window per (peer, client-epoch) container".
func TestPeerWindow_ServerParksAtWConn(t *testing.T) {
	t.Run("parks, then resumes on a sid-0 grant", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			sf := newSrvFixture(t, drpc.WithProtocolStats(events))

			// Two calls in turn: the connection window is per incarnation,
			// not per call, so the second one parks after the first one's
			// 300 plus 724 of its own.
			sf.handle(manyOpen(clientEpochA, 1, 4096, 300))
			synctest.Wait()
			x.Equal(t, 1, countMatch(sf.frames(), terminalOn(1)))
			sf.handle(manyOpen(clientEpochA, 2, 4096, 1100))
			synctest.Wait()
			frames := sf.frames()
			x.Equal(t, wConnTest, countMatch(frames, isDataFrame), "exactly W_conn data frames reach the wire")
			x.Equal(t, 0, countMatch(frames, terminalOn(2)), "the second call is parked, not over")
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "one sender parked on the connection window")
			x.Equal(t, 0, events.count(drpc.EventFlowStall), "the stream windows had credit")

			sf.handle(windowFrame(clientEpochA, 0, 0, 400))
			synctest.Wait()
			x.Equal(t, 1400, countMatch(sf.frames(), isDataFrame), "the grant releases exactly what it credits")
			x.Equal(t, 1, countMatch(sf.frames(), terminalOn(2)))
			x.Equal(t, 1, events.count(drpc.EventPeerFlowResume))
		})
	})
	t.Run("the park ends at T_stall, UNAVAILABLE naming the window", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			const stall = 2 * time.Second
			events := &flowEvents{}
			sf := newSrvFixture(t, drpc.WithTiming(drpc.Timing{Stall: stall}), drpc.WithProtocolStats(events))

			sf.handle(manyOpen(clientEpochA, 1, 4096, wConnTest+1))
			synctest.Wait()
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))
			x.Equal(t, 0, countMatch(sf.frames(), isTerminal))

			time.Sleep(stall)
			synctest.Wait()
			term := firstMatch(sf.frames(), isTerminal)
			x.True(t, term != nil, "the park must end at T_stall")
			x.Equal(t, codes.Unavailable, codes.Code(term.GetCode()))
			x.True(t, strings.Contains(term.GetDesc(), "connection credit"),
				"the error must name the window that starved it, got: ", term.GetDesc())
			x.Equal(t, 0, events.count(drpc.EventPeerFlowResume))
		})
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §9.1 grants: a sid-0 WINDOW credits the (peer, client-epoch)
// container it names, in reliable mode, when that container exists. Anything
// else is dropped in silence — no RESET, no credit, no container.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Grants: the server applies a sid-0 WINDOW only "when a container
// exists for (peer, epoch) — and otherwise drops it silently: never validated
// (§9.1), never answered with a RESET (§9.3), never creating state".
func TestPeerWindow_ServerSid0GrantGate(t *testing.T) {
	t.Run("before any OPEN: silent, and no state", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			// MaxDeadPeers 1: had the stray grant created a container, the
			// OPEN's own container would have had to evict it — invisible.
			// So instead: a grant that DID land would be spent by the call
			// below, which asks for W_conn + 100 and would complete.
			sf := newSrvFixture(t)
			sf.handle(windowFrame(clientEpochA, 0, 0, 100))
			x.Equal(t, 0, len(sf.frames()), "a sid-0 grant before any OPEN is silent")

			sf.handle(manyOpen(clientEpochA, 1, 4096, wConnTest+100))
			synctest.Wait()
			frames := sf.frames()
			x.True(t, firstMatch(frames, isAckH) != nil, "the OPEN is admitted")
			x.Equal(t, wConnTest, countMatch(frames, isDataFrame), "the sender is created at the OPEN's W_conn: the early grant credited nothing")
		})
	})
	t.Run("a foreign client epoch: silent", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			sf := newSrvFixture(t)
			sf.handle(manyOpen(clientEpochA, 1, 4096, wConnTest+100))
			synctest.Wait()
			sf.handle(windowFrame(clientEpochB, 0, 0, 100)) // no container for B
			synctest.Wait()
			frames := sf.frames()
			x.Equal(t, wConnTest, countMatch(frames, isDataFrame))
			x.Equal(t, 0, countMatch(frames, isResetFrame), "never answered with a RESET")

			sf.handle(windowFrame(clientEpochA, 0, 0, 100)) // the real one
			synctest.Wait()
			x.Equal(t, wConnTest+100, countMatch(sf.frames(), isDataFrame))
		})
	})
	t.Run("unreliable mode: silent", func(t *testing.T) {
		is := newInjectServerMode(t, false, drpc.WithTiming(fastTiming))
		is.handle(windowFrame(clientEpochA, 0, 0, 100))
		x.True(t, is.recv(t) == nil, "unreliable mode has no connection window")
	})
	t.Run("after an OPEN with no advertisement: off, and a grant never enables", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			sf := newSrvFixture(t, drpc.WithProtocolStats(events))
			// Neither advertisement on the OPEN that creates the container:
			// the client does no flow control, on either window.
			open := manyOpen(clientEpochA, 1, 0, 2*wConnTest)
			open.SetConnWindow(0)
			sf.handle(open)
			synctest.Wait()
			x.Equal(t, 2*wConnTest, countMatch(sf.frames(), isDataFrame), "no window binds")
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))
			x.Equal(t, 0, events.count(drpc.EventFlowStall))

			sf.handle(windowFrame(clientEpochA, 0, 0, 1)) // dropped: never enables
			// Nor does a later OPEN that advertises one: the first counted
			// (§4.2.1).
			sf.handle(manyOpen(clientEpochA, 2, 0, 2*wConnTest))
			synctest.Wait()
			x.Equal(t, 4*wConnTest, countMatch(sf.frames(), isDataFrame))
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))
		})
	})
	t.Run("after an OPEN with a stream window but no connection window: streams pace, the peer does not", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			sf := newSrvFixture(t, drpc.WithProtocolStats(events))
			// window = 32, conn_window absent: the two windows are advertised
			// independently, and the connection one is off whatever the
			// per-stream one said (§4.2.1).
			open := manyOpen(clientEpochA, 1, 32, 2*wConnTest)
			open.SetConnWindow(0)
			sf.handle(open)
			synctest.Wait()
			x.Equal(t, 32, countMatch(sf.frames(), isDataFrame), "the stream window binds at 32")
			x.Equal(t, 1, events.count(drpc.EventFlowStall))
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))

			sf.handle(windowFrame(clientEpochA, 0, 0, 1))           // sid 0: dropped, never enables
			sf.handle(windowFrame(clientEpochA, 0, 1, 2*wConnTest)) // the stream's own grant
			synctest.Wait()
			x.Equal(t, 2*wConnTest, countMatch(sf.frames(), isDataFrame), "past W_conn with no connection window to park on")
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))
		})
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 advertisement, the server's half: every H and every T it sends in
// reliable mode carries its MaxPeerWindow as conn_window — the creation ack,
// a SendHeader-flushed H, a unary T, a streaming T, a rejection T, Stop's
// UNAVAILABLE — and nothing rides behind the first H on sid 0. Data frames
// and grants carry none.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Advertisement: "the server on every H and T (§7). Every frame
// of those kinds carries it, and the value is fixed for the advertiser's
// lifetime".
func TestPeerWindow_ServerAdvertisesOnEveryHAndT(t *testing.T) {
	const window = 2048
	limits := drpc.WithLimits(drpc.Limits{MaxPeerWindow: window})
	// advertised checks every frame: an H or T carries want, anything else
	// carries nothing.
	advertised := func(t *testing.T, frames []*drpc.Frame, want uint32) {
		t.Helper()
		for _, f := range frames {
			if isAckH(f) || isTerminal(f) {
				x.Equal(t, want, f.GetConnWindow(), "an H or T without the advertisement: ", f)
			} else {
				x.Equal(t, uint32(0), f.GetConnWindow(), "a frame that must not carry it: ", f)
			}
		}
	}

	t.Run("creation ack H, every one, nothing on sid 0", func(t *testing.T) {
		sf := newSrvFixture(t, limits, blockStreams())
		sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Buff_FullMethodName))
		sf.handle(streamOpen(clientEpochA, 2, 32, echo.EchoService_Buff_FullMethodName))
		sf.handle(streamOpen(clientEpochB, 1, 32, echo.EchoService_Buff_FullMethodName))
		frames := sf.frames()
		x.Equal(t, 3, len(frames), "the three acks and nothing else, got ", frames)
		for _, f := range frames {
			x.True(t, isAckH(f), "got ", f)
		}
		advertised(t, frames, window)
		x.Equal(t, 0, countMatch(frames, isPeerGrant), "nothing rides behind an H on sid 0")
	})
	t.Run("a unary T, and a SendHeader-flushed H before it", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			sf := newSrvFixture(t, limits)
			sf.handle(openFrame(clientEpochA, 1, 1, echo.EchoService_Once_FullMethodName))
			synctest.Wait()
			frames := sf.frames()
			x.Equal(t, 1, len(frames), "a plain unary call: its T alone, got ", frames)
			x.True(t, isTerminal(frames[0]), "got ", frames[0])
			advertised(t, frames, window)

			// Request metadata makes the echo handler flush a header: an H
			// before the T, both advertising.
			open := openFrame(clientEpochA, 2, 1, echo.EchoService_Once_FullMethodName)
			open.SetHeader(wireMd(metadata.Pairs("k", "v")))
			sf.handle(open)
			synctest.Wait()
			frames = sf.frames()[1:]
			x.Equal(t, 2, len(frames), "the flushed H, then the T, got ", frames)
			x.True(t, isAckH(frames[0]) && isTerminal(frames[1]), "got ", frames)
			advertised(t, frames, window)
		})
	})
	t.Run("rejection Ts: unknown method, live-call cap", func(t *testing.T) {
		sf := newSrvFixture(t, drpc.WithLimits(drpc.Limits{MaxPeerWindow: window, MaxLiveCalls: 1}), blockStreams())
		sf.handle(streamOpen(clientEpochA, 1, 32, "/echo.EchoService/Nope"))
		sf.handle(streamOpen(clientEpochA, 2, 32, echo.EchoService_Live_FullMethodName)) // admitted: the cap is 1
		sf.handle(streamOpen(clientEpochA, 3, 32, echo.EchoService_Live_FullMethodName)) // refused
		frames := sf.frames()
		x.Equal(t, 3, len(frames), "got ", frames)
		x.True(t, isTerminal(frames[0]) && codes.Code(frames[0].GetCode()) == codes.Unimplemented, "got ", frames[0])
		x.True(t, isAckH(frames[1]), "got ", frames[1])
		x.True(t, isTerminal(frames[2]) && codes.Code(frames[2].GetCode()) == codes.ResourceExhausted, "got ", frames[2])
		advertised(t, frames, window)
	})
	t.Run("a streaming T, and Stop's UNAVAILABLE", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			sf := newSrvFixture(t, limits, blockStreams())
			sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Live_FullMethodName))
			sf.handle(streamOpen(clientEpochA, 2, 32, echo.EchoService_Live_FullMethodName))
			synctest.Wait()
			sf.handle(abortFrame(clientEpochA, 1, 2)) // the handler unwinds into its T
			synctest.Wait()
			sf.srv.Stop() // and the other into T{UNAVAILABLE}
			synctest.Wait()
			frames := sf.frames()
			x.Equal(t, 2, countMatch(frames, isTerminal), "got ", frames)
			x.Equal(t, codes.Canceled, codes.Code(firstMatch(frames, terminalOn(1)).GetCode()))
			x.Equal(t, codes.Unavailable, codes.Code(firstMatch(frames, terminalOn(2)).GetCode()))
			advertised(t, frames, window)
		})
	})
	t.Run("data frames and grants carry none", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			sf := newSrvFixture(t, limits, drpc.WithRxBuffer(wConnTest, drpc.DropNewest))
			sf.handle(manyOpen(clientEpochA, 1, 4096, 5))
			synctest.Wait()
			// Half a window consumed by a Buff handler: a per-stream grant
			// and a sid-0 grant.
			sf.handle(streamOpen(clientEpochA, 2, 32, echo.EchoService_Buff_FullMethodName))
			for i := range uint32(window / 2) {
				sf.handle(lcData(clientEpochA, 2, 2+i, nil))
			}
			synctest.Wait()
			frames := sf.frames()
			x.Equal(t, 5, countMatch(frames, isDataFrame))
			x.True(t, countMatch(frames, isPeerGrant) >= 1, "a sid-0 grant, got ", frames)
			x.True(t, countMatch(frames, isWindowFrame) > countMatch(frames, isPeerGrant), "and a per-stream one, got ", frames)
			advertised(t, frames, window)
		})
	})
	t.Run("at the floor and by default: W_conn", func(t *testing.T) {
		sf := newSrvFixture(t, drpc.WithLimits(drpc.Limits{MaxPeerWindow: 100}), blockStreams())
		sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Buff_FullMethodName))
		advertised(t, sf.frames(), wConnTest)
		x.Equal(t, 1, len(sf.frames()))

		def := newSrvFixture(t, blockStreams())
		def.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Buff_FullMethodName))
		advertised(t, def.frames(), wConnTest)
		x.Equal(t, 1, len(def.frames()))
	})
}

// ---------------------------------------------------------------------------
// §9.4 container-on-reject: a reliable-mode rejected OPEN creates the (peer,
// client-epoch) container, and its sender from the advertisement the OPEN
// carries — the server's sender toward that incarnation runs on that window
// from the first admitted call on, whatever later OPENs say.
// ---------------------------------------------------------------------------

// Pins §9.4 Container on rejection: "the OPEN was validated (§9.1) and
// carries the client's advertisement, from which the container's sender is
// created", and §4.2.1 Advertisement: "A peer applies the first advertisement
// it hears from a peer incarnation and ignores the rest".
func TestPeerWindow_RejectedFirstOpenCreatesTheSender(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		sf := newSrvFixture(t, drpc.WithProtocolStats(events))

		// The incarnation's first OPEN names a method that does not exist,
		// and advertises 2048.
		open := streamOpen(clientEpochA, 1, 32, "/echo.EchoService/Nope")
		open.SetConnWindow(2048)
		sf.handle(open)
		rej := firstMatch(sf.frames(), isTerminal)
		x.True(t, rej != nil && codes.Code(rej.GetCode()) == codes.Unimplemented, "rejected, got ", rej)
		x.Equal(t, clientEpochA, rej.GetPeerEpoch())
		x.Equal(t, uint32(wConnTest), rej.GetConnWindow(), "the rejection advertises this side's window like any T")

		// 2049 responses asked for on a call whose OPEN says 4096: exactly
		// 2048 go out — the window the rejected OPEN advertised; the later
		// value is ignored.
		open = manyOpen(clientEpochA, 2, 4096, 2048+1)
		open.SetConnWindow(4096)
		sf.handle(open)
		synctest.Wait()
		x.Equal(t, 2048, countMatch(sf.frames(), isDataFrame), "the sender was created at 2048 by the rejected OPEN")
		x.Equal(t, 0, countMatch(sf.frames(), terminalOn(2)), "and parks on the 2049th")
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))

		sf.handle(windowFrame(clientEpochA, 0, 0, 1))
		synctest.Wait()
		x.Equal(t, 2049, countMatch(sf.frames(), isDataFrame))
		x.Equal(t, 1, countMatch(sf.frames(), terminalOn(2)))
	})
}

// Pins §4.2.1 Advertisement, the server as sender: "the server from the OPEN
// that creates the (peer, client-epoch) container", the first one counting
// and each incarnation its own.
func TestPeerWindow_ServerHonoursTheOpenAdvertisement(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		sf := newSrvFixture(t, drpc.WithProtocolStats(events))

		open := manyOpen(clientEpochA, 1, 4096, 2048+1)
		open.SetConnWindow(2048)
		sf.handle(open)
		synctest.Wait()
		x.Equal(t, 2048, countMatch(sf.frames(), isDataFrame), "exactly the advertised 2048 reach the wire")
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "the 2049th parks")

		// A second OPEN of the same incarnation advertising more changes
		// nothing: the sender is the incarnation's, created once.
		open = manyOpen(clientEpochA, 2, 4096, 1)
		open.SetConnWindow(8192)
		sf.handle(open)
		synctest.Wait()
		x.Equal(t, 2048, countMatch(sf.frames(), isDataFrame), "the later advertisement is ignored")
		x.Equal(t, 2, events.count(drpc.EventPeerFlowStall), "its call parks like the first")

		// Another incarnation of the same peer is its own sender, created
		// from its own OPEN.
		open = manyOpen(clientEpochB, 1, 4096, 1500+1)
		open.SetConnWindow(1500)
		sf.handle(open)
		synctest.Wait()
		x.Equal(t, 2048+1500, countMatch(sf.frames(), isDataFrame))
		x.Equal(t, 3, events.count(drpc.EventPeerFlowStall))

		// A grant to A releases A's two parked sends and nothing of B's.
		sf.handle(windowFrame(clientEpochA, 0, 0, 2))
		synctest.Wait()
		x.Equal(t, 2048+1500+2, countMatch(sf.frames(), isDataFrame))
		x.Equal(t, 2, events.count(drpc.EventPeerFlowResume))
	})
}

// ---------------------------------------------------------------------------
// §4.2 / §4.2.1 / §15 receiving: one transport peer may not have more than
// MaxPeerWindow buffered here across all of its calls. The frame that would
// exceed it is never buffered and fails ITS call INTERNAL — the other call is
// untouched — and the credit of both the refused frame and the failed call's
// discarded frames comes back on sid 0.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Overrun: "A data frame that would take the peer past
// MaxPeerWindow is not buffered; the receiver fails the call it is addressed
// to with INTERNAL ... and returns the frame's credit as never buffered".
func TestPeerWindow_ServerOverrunFailsOnlyTheOffendingCall(t *testing.T) {
	bubble(t, func(t *testing.T) {
		// Per-stream buffers of W_conn each, handlers that never read: only
		// the connection window can trip.
		sf := newSrvFixture(t, drpc.WithRxBuffer(wConnTest, drpc.DropNewest), blockStreams())
		sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Live_FullMethodName))
		sf.handle(streamOpen(clientEpochA, 2, 32, echo.EchoService_Live_FullMethodName))

		// Exactly the window, split across the two calls.
		for i := range uint32(wConnTest / 2) {
			sf.handle(lcData(clientEpochA, 1, 2+i, nil))
			sf.handle(lcData(clientEpochA, 2, 2+i, nil))
		}
		synctest.Wait()
		x.Equal(t, 0, countMatch(sf.frames(), isTerminal), "the window fits")
		x.Equal(t, 0, countMatch(sf.frames(), isPeerGrant), "nothing consumed, nothing granted")

		// One past it, on 2: call 2 fails, call 1 does not.
		sf.handle(lcData(clientEpochA, 2, 2+wConnTest/2, nil))
		synctest.Wait()
		frames := sf.frames()
		term := firstMatch(frames, terminalOn(2))
		x.True(t, term != nil, "the offending call aborts")
		x.Equal(t, codes.Internal, codes.Code(term.GetCode()))
		x.True(t, strings.Contains(term.GetDesc(), "connection flow-control window"),
			"the error must name the connection window, got: ", term.GetDesc())
		x.Equal(t, 0, countMatch(frames, terminalOn(1)), "the other call is untouched")
		x.Equal(t, 0, countMatch(frames, isResetFrame))

		// The refused frame's credit came back at once (the starvation
		// clause: the window was full), and the failed call's 512 discarded
		// frames came back in one grant when it finished.
		n, total := peerGrants(frames)
		x.Equal(t, 2, n)
		x.Equal(t, uint32(wConnTest/2+1), total)
		x.Equal(t, clientEpochA, frames[len(frames)-1].GetPeerEpoch())

		// Call 1 is live and, with call 2's frames gone, there is room again.
		sf.handle(lcData(clientEpochA, 1, 2+wConnTest/2, nil))
		synctest.Wait()
		x.Equal(t, 0, countMatch(sf.frames(), terminalOn(1)))
		x.Equal(t, 0, countMatch(sf.frames(), isResetFrame))
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 credit return, the leak test: every reliable-mode data frame received
// returns one credit on sid 0 once it stops occupying a buffer — the frames
// pipelined behind an abort (RESET-drawn or discarded with the call), an
// off-shape frame, a frame for a finished sid, a frame from an incarnation
// whose container the cap evicted (§9.4) — and the server-streaming
// request that rode the OPEN returns none, because the client never charged
// it. Grants batch at half the window, so each case is primed to the edge.
// ---------------------------------------------------------------------------

// Pins §4.2.1 The receiver's ledger: "Two exclusions, both because the sender
// never charged the frame: the server-streaming OPEN payload (§8)" —
// everything else comes back, buffered or not.
func TestPeerWindow_ServerCreditReturnedForEveryNonBufferedFrame(t *testing.T) {
	// prime returns credit for n frames of a call this server never had:
	// each draws a RESET (§9.3, §10.6) and is never buffered.
	prime := func(sf *srvFixture, n uint32) {
		for i := range n {
			sf.handle(lcData(clientEpochA, 99, 2+i, nil))
		}
	}
	t.Run("frames behind an abort", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			const behind = 40
			sf := newSrvFixture(t, blockStreams())
			sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Live_FullMethodName))
			prime(sf, wConnTest/2-behind)
			x.Equal(t, wConnTest/2-behind, countMatch(sf.frames(), isResetFrame))
			x.Equal(t, 0, countMatch(sf.frames(), isPeerGrant), "one short of the batch")

			// The abort, then 40 data frames pipelined behind it: each is
			// either discarded with the call or RESET-drawn — and either way
			// its credit is exactly what completes the batch.
			sf.handle(abortFrame(clientEpochA, 1, 2))
			for i := range uint32(behind) {
				sf.handle(lcData(clientEpochA, 1, 3+i, nil))
			}
			synctest.Wait()
			n, total := peerGrants(sf.frames())
			x.Equal(t, 1, n)
			x.Equal(t, uint32(wConnTest/2), total)
		})
	})
	t.Run("off-shape data on a server-streaming call", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			sf := newSrvFixture(t, blockStreams())
			sf.handle(manyOpen(clientEpochA, 1, 32, 1))
			prime(sf, wConnTest/2-1)
			sf.handle(lcData(clientEpochA, 1, 2, nil)) // no client data frames on this shape
			x.Equal(t, 0, countMatch(sf.frames(), isTerminal), "off-shape is dropped, the call lives")
			n, total := peerGrants(sf.frames())
			x.Equal(t, 1, n)
			x.Equal(t, uint32(wConnTest/2), total)
		})
	})
	t.Run("off-shape data on a unary call", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			// A unary handler that parks, so the call is live — and has no
			// client data frames in its shape — when the frame lands.
			sf := newSrvFixture(t, drpc.UnaryInterceptor(func(ctx context.Context, _ any, _ *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}))
			sf.handle(openFrame(clientEpochA, 1, 1, echo.EchoService_Once_FullMethodName))
			synctest.Wait()
			prime(sf, wConnTest/2-1)
			sf.handle(lcData(clientEpochA, 1, 2, nil)) // no client data frames on this shape
			synctest.Wait()
			x.Equal(t, 0, countMatch(sf.frames(), isTerminal), "off-shape is dropped, the call lives")
			n, total := peerGrants(sf.frames())
			x.Equal(t, 1, n)
			x.Equal(t, uint32(wConnTest/2), total)
		})
	})
	t.Run("a frame from an incarnation the container cap evicted", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			// Several Conns on one socket: the cap evicts an idle one's
			// container and the ledger keeps its sender position (§9.4). A
			// data frame that Conn still had in flight for a call the server
			// finished draws its RESET and returns its credit to that
			// position, exactly as it would to the container.
			sf := newSrvFixture(t, drpc.WithLimits(drpc.Limits{MaxDeadPeers: 2}))
			idle := func(epoch uint32) {
				sf.handle(openFrame(epoch, 1, 1, echo.EchoService_Once_FullMethodName))
				synctest.Wait()         // the call ran: the container is idle
				time.Sleep(time.Second) // containers are evicted oldest first
			}
			idle(clientEpochA)
			prime(sf, wConnTest/2-1)
			x.Equal(t, 0, countMatch(sf.frames(), isPeerGrant), "one short of the batch")
			idle(clientEpochB)
			idle(clientEpochB + 1) // the third idle incarnation evicts A, the oldest
			// A's straggler, for its finished call.
			sf.handle(lcData(clientEpochA, 1, 2, nil))
			x.Equal(t, wConnTest/2, countMatch(sf.frames(), isResetFrame), "RESET-drawn like any other")
			frames := sf.frames()
			n, total := peerGrants(frames)
			x.Equal(t, 1, n)
			x.Equal(t, uint32(wConnTest/2), total)
			x.Equal(t, clientEpochA, firstMatch(frames, isPeerGrant).GetPeerEpoch(), "granted to the incarnation that spent it")
		})
	})
	t.Run("the server-streaming request rode the OPEN uncredited", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			sf := newSrvFixture(t)
			sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Buff_FullMethodName))
			prime(sf, wConnTest/2-1)
			// A whole server-streaming call: its request is read by the
			// handler out of the same buffer as any data frame would be.
			sf.handle(manyOpen(clientEpochA, 2, 32, 1))
			synctest.Wait()
			x.Equal(t, 1, countMatch(sf.frames(), terminalOn(2)))
			x.Equal(t, 0, countMatch(sf.frames(), isPeerGrant), "the request returned nothing: one short still")
			prime(sf, 1) // control: the edge is exactly here
			n, total := peerGrants(sf.frames())
			x.Equal(t, 1, n)
			x.Equal(t, uint32(wConnTest/2), total)
		})
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 consumption: frames the handler consumes return their credit on
// sid 0, batched at half the window, beside the per-stream grant.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Cadence: "each is granted exactly what its own frames returned,
// in a grant naming it (`peer_epoch`)".
func TestPeerWindow_ServerConsumedFramesGrantOnSid0(t *testing.T) {
	bubble(t, func(t *testing.T) {
		sf := newSrvFixture(t, drpc.WithRxBuffer(wConnTest, drpc.DropNewest))
		sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Buff_FullMethodName))
		const injected = 600
		for i := range uint32(injected) {
			sf.handle(lcData(clientEpochA, 1, 2+i, nil))
		}
		synctest.Wait() // the Buff handler consumes everything it is given
		frames := sf.frames()
		n, total := peerGrants(frames)
		x.Equal(t, 1, n, "batched at half the window: 600 consumed is one grant")
		x.Equal(t, uint32(wConnTest/2), total)
		x.Equal(t, clientEpochA, firstMatch(frames, isPeerGrant).GetPeerEpoch())
		// The per-stream grant is still there, on the call's own sid.
		x.Equal(t, 1, countMatch(frames, func(f *drpc.Frame) bool {
			return isWindowFrame(f) && f.GetSid() == 1
		}))
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 starvation clause, end to end: 17 client-streaming consumers stuck at
// full 32-message windows pin 544 of the 1024 connection window, so pending
// can never reach half of it — yet an 18th stream consumed promptly moves 2000
// messages, because the receiver grants whenever outstanding + pending reaches
// its window.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Cadence: "It MUST grant, whatever it holds back, whenever
// buffered + held back ≥ MaxPeerWindow".
func TestPeerWindow_StarvationClause(t *testing.T) {
	bubble(t, func(t *testing.T) {
		const stuck, moved = 17, 2000
		const healthyKey = "flow-healthy"
		events := &flowEvents{}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithReliable(true), drpc.WithProtocolStats(events)},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(true), blockUnlessMarked(healthyKey),
			},
		}.Use(t)
		defer stop()

		msg := echo.EchoRequest_builder{Message: "m", Repeat: 1}.Build() // one item back per request
		for range stuck {
			s, err := client.Buff(t.Context())
			x.NoError(t, err)
			for range wInitTest {
				x.NoError(t, s.Send(msg)) // exactly a window each: never parks
			}
		}
		synctest.Wait()
		x.Equal(t, 0, events.count(drpc.EventFlowStall))
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))

		healthy, err := client.Buff(metadata.AppendToOutgoingContext(t.Context(), healthyKey, "1"))
		x.NoError(t, err)
		for range moved {
			x.NoError(t, healthy.Send(msg))
		}
		res, err := healthy.CloseAndRecv()
		x.NoError(t, err)
		x.Equal(t, moved, len(res.GetItems()))

		x.True(t, events.count(drpc.EventPeerFlowStall) > 0, "the sender did run out of connection credit")
		x.Equal(t, events.count(drpc.EventPeerFlowStall), events.count(drpc.EventPeerFlowResume), "and was released every time")
		grants := 0
		for _, f := range client.rxFrames() {
			if !isPeerGrant(f) {
				continue
			}
			grants++
			x.True(t, f.GetWindow() < wConnTest/2, "granted below half the window: ", f.GetWindow())
		}
		x.True(t, grants > 0, "the starvation clause must have fired")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 end to end, the head-of-line twin: two server-streaming calls left
// unread exceed the client's connection window, the server parks on it — not
// on either stream window — and the channel stays live for a unary call. As
// the application consumes, sid-0 grants resume it and every message arrives.
// ---------------------------------------------------------------------------

// Pins §4.2.1: "The connection window, one per peer, bounds what a peer can
// pin across all of its calls (§15)" — and the channel stays live under it
// (§4.2).
func TestPeerWindow_ParksAcrossStreamsAndKeepsTheChannelLive(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithReliable(true), drpc.WithRxBuffer(wConnTest, drpc.DropNewest)},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(true), drpc.WithRxBuffer(wConnTest, drpc.DropNewest), drpc.WithProtocolStats(events),
			},
		}.Use(t)
		defer stop()

		const burst = 600 // two of them: past W_conn, within each stream window
		var stalled [2]echo.EchoService_ManyClient
		for i := range stalled {
			s, err := client.Many(t.Context(), echo.EchoRequest_builder{Message: "m", Repeat: burst}.Build())
			x.NoError(t, err)
			stalled[i] = s
		}
		synctest.Wait()
		x.True(t, events.count(drpc.EventPeerFlowStall) > 0, "the producer parks on the connection window")
		x.Equal(t, 0, events.count(drpc.EventFlowStall), "neither stream window is full")

		res, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: "abc", CircularShift: 1}.Build())
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		for _, s := range stalled {
			n := uint32(0)
			for {
				res, err := s.Recv()
				if err == io.EOF {
					break
				}
				x.NoError(t, err)
				x.Equal(t, n, res.GetSequence())
				n++
			}
			x.Equal(t, uint32(burst), n)
		}
		x.True(t, events.count(drpc.EventPeerFlowResume) >= 1)
		x.True(t, countMatch(client.txFrames(), isPeerGrant) > 0, "the client granted on sid 0")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 end to end, both directions past W_conn under default options — the
// Go twin of the cross-language case: three bidi streams interleaved, 400
// messages each way, complete only because both sides grant on sid 0 and
// honour it. The grants are well-formed and the per-stream ones stay on their
// own sids.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Grants: a sid-0 WINDOW "is the only thing that adds connection
// credit" — both sides send it and honour it, or neither direction passes
// W_conn.
func TestPeerWindow_GoToGoPastWConnBothWays(t *testing.T) {
	client, stop := PipeOption{
		ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true)},
		ServerOpts: []drpc.ServerOption{drpc.WithReliable(true)},
	}.Use(t)
	defer stop()

	const streams, each = 3, 400 // 1200 data frames each way > W_conn
	var live [streams]echo.EchoService_LiveClient
	for i := range live {
		s, err := client.Live(t.Context())
		x.NoError(t, err)
		live[i] = s
	}
	for i := range each {
		for k, s := range live {
			x.NoError(t, s.Send(echo.EchoRequest_builder{Message: fmt.Sprintf("%d/%d", k, i), Repeat: 1}.Build()))
			res, err := s.Recv()
			x.NoError(t, err)
			x.Equal(t, fmt.Sprintf("%d/%d", k, i), res.GetMessage())
		}
	}
	sids := map[uint32]bool{}
	for _, s := range live {
		x.NoError(t, s.CloseSend())
		_, err := s.Recv()
		x.ErrorIs(t, err, io.EOF)
	}
	for _, f := range client.txFrames() {
		if f.GetFlags()&drpc.FlagOpen != 0 {
			sids[f.GetSid()] = true
		}
	}
	x.Equal(t, streams, len(sids))
	clientEpoch := firstMatch(client.txFrames(), func(f *drpc.Frame) bool { return f.GetFlags()&drpc.FlagOpen != 0 }).GetEpoch()

	check := func(dir string, frames []*drpc.Frame, serverSent bool) {
		t.Helper()
		n, total := peerGrants(frames)
		x.True(t, n > 0, dir, ": no sid-0 grant")
		x.True(t, total >= wConnTest/2, dir, ": too little credit returned: ", total)
		for _, f := range frames {
			if !isWindowFrame(f) {
				continue
			}
			if f.GetSid() == 0 {
				x.True(t, f.GetWindow() > 0)
				if serverSent {
					x.Equal(t, clientEpoch, f.GetPeerEpoch(), dir, ": a server grant names the client incarnation")
				}
				continue
			}
			x.True(t, sids[f.GetSid()], dir, ": a per-stream grant on a live sid, got ", f.GetSid())
		}
	}
	check("client->server", client.txFrames(), false)
	check("server->client", client.rxFrames(), true)
}

// ---------------------------------------------------------------------------
// Counters: a park on the connection window is a PeerFlowStall, not a
// FlowStall, and the event carries the parked call's sid and method.
// ---------------------------------------------------------------------------

// Pins §14: "flow-stall counters (per stream and per peer, §4.2.1)" — the
// server's half; the client's is TestStats_CountersPeerFlow.
func TestPeerWindow_CountersPeerFlow(t *testing.T) {
	bubble(t, func(t *testing.T) {
		counters := &drpc.Counters{}
		var mu sync.Mutex
		var stalls []drpc.ProtocolEvent
		record := drpc.ProtocolStatsFunc(func(ev drpc.ProtocolEvent) {
			if ev.Kind == drpc.EventPeerFlowStall {
				mu.Lock()
				stalls = append(stalls, ev)
				mu.Unlock()
			}
		})
		sf := newSrvFixture(t, drpc.WithProtocolStats(counters), drpc.WithProtocolStats(record))
		sf.handle(manyOpen(clientEpochA, 7, 4096, wConnTest+1))
		synctest.Wait()

		snap := counters.Snapshot()
		x.Equal(t, uint64(1), snap.PeerFlowStall)
		x.Equal(t, uint64(0), snap.FlowStall)
		x.Equal(t, uint64(0), snap.PeerFlowResume)
		mu.Lock()
		x.Equal(t, 1, len(stalls))
		x.Equal(t, uint32(7), stalls[0].Sid)
		x.Equal(t, echo.EchoService_Many_FullMethodName, stalls[0].Method)
		mu.Unlock()

		sf.handle(windowFrame(clientEpochA, 0, 0, 1))
		synctest.Wait()
		x.Equal(t, uint64(1), counters.Snapshot().PeerFlowResume)
		x.Equal(t, 1, countMatch(sf.frames(), isTerminal))
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 sending, the late Send: a handler's Send after the client's abort is
// the ordinary "Send until it errors" loop racing a cancel. It must spend
// nothing on the connection window toward that client — shared by every call
// to it and cumulative — or each cancelled call is a permanent shrink.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Sending: "a send that never reaches the wire — the adapter
// refused it (§4.4), or the call ended first — refunds both".
func TestPeerWindow_ServerSendAfterCancelSpendsNothing(t *testing.T) {
	bubble(t, func(t *testing.T) {
		const cancelled = 40
		const key = "flow-healthy"
		events := &flowEvents{}
		// Unmarked calls wait for the client's abort and then Send once; the
		// marked one runs the real handler.
		lateSend := drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if md, _ := metadata.FromIncomingContext(ss.Context()); len(md.Get(key)) > 0 {
				return handler(srv, ss)
			}
			<-ss.Context().Done()
			_ = ss.SendMsg(&echo.EchoResponse{})
			return nil
		})
		sf := newSrvFixture(t, lateSend, drpc.WithProtocolStats(events))
		for sid := uint32(1); sid <= cancelled; sid++ {
			sf.handle(manyOpen(clientEpochA, sid, 4096, 1))
			synctest.Wait() // the handler waits on its ctx
			sf.handle(abortFrame(clientEpochA, sid, 2))
			synctest.Wait() // it Sent into the ended call and unwound
		}
		x.Equal(t, 0, countMatch(sf.frames(), isDataFrame), "no late Send reached the wire")

		// A healthy call now moves the whole W_conn: none of the late Sends
		// spent a credit. A leak of one per cancelled call would park it 40
		// short of the window.
		open := manyOpen(clientEpochA, cancelled+1, 4096, wConnTest)
		open.SetHeader(wireMd(metadata.Pairs(key, "1")))
		sf.handle(open)
		synctest.Wait()
		x.Equal(t, wConnTest, countMatch(sf.frames(), isDataFrame), "exactly W_conn data frames reach the wire")
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "nothing was spent on the connection window")
		x.Equal(t, 1, countMatch(sf.frames(), terminalOn(cancelled+1)), "and the call completed")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 cadence across incarnations: the ledger is per transport peer, the
// grants are per incarnation. A dead incarnation's bulk return must not carry
// the live one's credit away — that credit would be dropped by the client
// and lost for good.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Cadence: "credit is held back and granted per incarnation:
// each is granted exactly what its own frames returned".
func TestPeerWindow_CreditIsGrantedToTheIncarnationThatSpentIt(t *testing.T) {
	bubble(t, func(t *testing.T) {
		const key = "flow-live"
		const half = wConnTest / 2
		sf := newSrvFixture(t, drpc.WithRxBuffer(wConnTest, drpc.DropNewest), blockUnlessMarked(key))

		// Incarnation A: a client-streaming call under a stuck consumer, 300
		// buffered. Then the client restarts at the same key as B: its call
		// is consumed promptly, 300 sent.
		sf.handle(streamOpen(clientEpochA, 1, 32, echo.EchoService_Buff_FullMethodName))
		for i := range uint32(300) {
			sf.handle(lcData(clientEpochA, 1, 2+i, nil))
		}
		open := streamOpen(clientEpochB, 1, 32, echo.EchoService_Buff_FullMethodName)
		open.SetHeader(wireMd(metadata.Pairs(key, "1")))
		sf.handle(open)
		for i := range uint32(300) {
			sf.handle(lcData(clientEpochB, 1, 2+i, nil))
		}
		synctest.Wait()
		x.Equal(t, 0, countMatch(sf.frames(), isPeerGrant), "300 held for B, 300 buffered for A: nothing due")

		// A's call ends and its 300 are discarded: that credit is A's, dead
		// with it. B's 300 stay held for B — nothing is due to anyone.
		sf.handle(abortFrame(clientEpochA, 1, 302))
		synctest.Wait()
		x.Equal(t, 0, countMatch(sf.frames(), isPeerGrant), "A's bulk return carries nothing of B's")

		// B reaches its own half window and is granted exactly that, to B.
		for i := range uint32(half - 300) {
			sf.handle(lcData(clientEpochB, 1, 302+i, nil))
		}
		synctest.Wait()
		var grants []*drpc.Frame
		for _, f := range sf.frames() {
			if isPeerGrant(f) {
				grants = append(grants, f)
			}
		}
		x.Equal(t, 1, len(grants), "one grant, got ", grants)
		x.Equal(t, uint32(half), grants[0].GetWindow())
		x.Equal(t, clientEpochB, grants[0].GetPeerEpoch(), "B's credit goes to B")
	})
}
