package drpc_test

// dataloss_test.go pins the end-to-end plumbing of the two fail-loud paths:
//
//   - §6.3 window-overrun DATA_LOSS: K_loud (3) mutually consistent
//     beyond-window frames on a live stream fail the call loudly — the client
//     surfaces status DataLoss and emits an abort CLOSE{DATA_LOSS}; the server
//     cancels the handler and the unwind emits T{DATA_LOSS} (§10.7 last row).
//     The counter resets on any accepted frame; dedup'd frames are neutral.
//
//   - §10.6 reliable mode: any gap or DUPLICATE is a broken transport —
//     fail the call with INTERNAL. This includes a duplicate OPEN on a live
//     reliable call (no retransmission exists to explain it).
//
// The rxWindow unit behavior is pinned in seq_test.go; here every assertion
// goes through Conn.Handle / Server.Handle and observes only public surface:
// Recv status codes and the frames emitted on the tx side.

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// dlRecorder is a tx FrameHandler capturing every frame the Conn emits.
type dlRecorder struct {
	mu     sync.Mutex
	frames []*drpc.Frame
}

func (r *dlRecorder) Handle(_ context.Context, f *drpc.Frame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, proto.CloneOf(f))
	return nil
}

// findClose returns the first recorded pure CLOSE frame carrying code, or
// nil. The client's OPEN|CLOSE (flags Open|Close) and half-closes (no code)
// never match: an abort is exactly FlagClose + code (PROTOCOL.md §7).
func (r *dlRecorder) findClose(code codes.Code) *drpc.Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.frames {
		if f.GetFlags() == drpc.FlagClose && f.HasCode() && codes.Code(f.GetCode()) == code {
			return f
		}
	}
	return nil
}

// dlManyStream opens a server-streaming Many call on a fresh Conn wired to
// rec and returns the conn, the stream, and the identity every crafted server
// frame must echo: the conn epoch (§6.1) and the call's sid, both learned
// from the emitted OPEN. Streaming calls carry no default T_call deadline, so
// the tests are free of real-time pressure even in unreliable mode.
func dlManyStream(t *testing.T, rec *dlRecorder, reliable bool) (*drpc.Conn, grpc.ClientStream, uint32, uint32) {
	t.Helper()
	conn := drpc.NewConn(rec, drpc.WithReliable(reliable))
	t.Cleanup(func() { conn.Close(nil) })

	sd := &grpc.StreamDesc{StreamName: "Many", ServerStreams: true}
	stream, err := conn.NewStream(t.Context(), sd, echo.EchoService_Many_FullMethodName)
	x.NoError(t, err)
	// Server-streaming emits its OPEN|CLOSE on the first SendMsg; transmit is
	// synchronous, so the OPEN is recorded when SendMsg returns.
	x.NoError(t, stream.SendMsg(echo.EchoRequest_builder{Message: "req"}.Build()))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, f := range rec.frames {
		if f.GetFlags()&drpc.FlagOpen != 0 {
			return conn, stream, f.GetEpoch(), f.GetSid()
		}
	}
	t.Fatal("no OPEN emitted")
	return nil, nil, 0, 0
}

// The server incarnation the crafted frames impersonate; the stream locks to
// it on the first accepted frame (L3 hardening), so every later frame of the
// same call must reuse it.
const dlSrvEpoch uint32 = 7

// dlServerData crafts a server data frame toward a Conn: epoch names the
// server incarnation, peer_epoch echoes the client incarnation (§6.1 — a
// frame without the echo would draw a RESET instead of reaching the stream).
func dlServerData(cEpoch, sid, seq uint32, msg string) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(dlSrvEpoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	f.SetPeerEpoch(cEpoch)
	data, _ := proto.Marshal(echo.EchoResponse_builder{Message: msg}.Build())
	f.SetPayload(data)
	return f
}

// dlLiveOpen crafts a bare bidi OPEN (FlagOpen only — the package-level
// openFrame helper builds unary OPEN|CLOSE, which is the wrong shape here;
// a bidi OPEN is eager and bare, PROTOCOL.md §8).
func dlLiveOpen(epoch, sid uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch) // nonzero client incarnation (§6.1)
	f.SetSid(sid)
	f.SetSeq(1) // an OPEN's seq MUST be 1 (§8)
	f.SetFlags(drpc.FlagOpen)
	f.SetMethod(echo.EchoService_Live_FullMethodName)
	return f
}

// dlClientData crafts a client data frame toward a Server.
func dlClientData(epoch, sid, seq uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	// Payload presence makes it a data frame; a zero-value EchoRequest keeps
	// the Live handler looping in Recv (Repeat 0 sends nothing back).
	f.SetPayload([]byte{})
	return f
}

// dlRecvTerminal drains is.out until a terminal CLOSE appears (skipping H
// frames and any timer-driven probes an unreliable-mode server may emit;
// handler unwind is asynchronous, so this polls with a generous real-time
// bound — no timing is asserted).
func dlRecvTerminal(t *testing.T, is *injectServer) *drpc.Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-is.out:
			if f.GetFlags() == drpc.FlagClose && f.HasCode() {
				return f
			}
		case <-deadline:
			t.Fatal("no terminal CLOSE emitted")
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// §6.3 window-overrun fail-loud, client side: K_loud consistent beyond-window
// frames surface status DataLoss to the app AND emit an abort CLOSE{DATA_LOSS}
// so the server stops producing.
// ---------------------------------------------------------------------------

func TestDataLoss_ClientSurfacesStatusAndAbort(t *testing.T) {
	rec := &dlRecorder{}
	c, stream, cEpoch, sid := dlManyStream(t, rec, false) // unreliable: window mode

	// One accepted frame locks the stream to the server incarnation and sets
	// L = 1. Injections are handled synchronously: verdict, abort emission,
	// and status latching all complete before Handle returns.
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 1, "one")))
	got := &echo.EchoResponse{}
	x.NoError(t, stream.RecvMsg(got))
	x.Equal(t, "one", got.GetMessage())

	// kLoud = 3 mutually consistent beyond-window frames: 5000 is beyond
	// [L+1, L+4096] = [2, 4097]; each next is within [0, 4096] of the
	// previous (§6.3).
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5000, "lost")))
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5001, "lost")))
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5002, "lost")))

	// The app sees DATA_LOSS, not a hang and not io.EOF.
	err := stream.RecvMsg(&echo.EchoResponse{})
	x.Equal(t, codes.DataLoss, status.Code(err))

	// And the client told the server to stop: abort CLOSE{DATA_LOSS} (§6.3).
	x.True(t, rec.findClose(codes.DataLoss) != nil, "expected an abort CLOSE with code DataLoss")
}

// ---------------------------------------------------------------------------
// §6.3: the beyond-window counter resets on any accepted frame. Two beyond,
// one good, two more beyond — even mutually consistent across the accept —
// never reaches K_loud: the call survives and delivers the good frames.
// ---------------------------------------------------------------------------

func TestDataLoss_RunResetsOnAcceptedFrame(t *testing.T) {
	rec := &dlRecorder{}
	c, stream, cEpoch, sid := dlManyStream(t, rec, false)

	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 1, "one")))

	// Two beyond-window frames: run length 2, below K_loud.
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5000, "lost")))
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5001, "lost")))

	// An accepted in-window frame resets the counter (§6.3).
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 2, "two")))

	// Two more, deliberately consistent with the FIRST pair (deltas of 1):
	// only the accept-reset — not run inconsistency — keeps the call alive.
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5002, "lost")))
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5003, "lost")))

	got := &echo.EchoResponse{}
	x.NoError(t, stream.RecvMsg(got))
	x.Equal(t, "one", got.GetMessage())
	x.NoError(t, stream.RecvMsg(got))
	x.Equal(t, "two", got.GetMessage()) // survived: no DataLoss

	// Clean terminal so the call ends in OK, proving it was never failed.
	term := &drpc.Frame{}
	term.SetEpoch(dlSrvEpoch)
	term.SetSid(sid)
	term.SetSeq(3)
	term.SetPeerEpoch(cEpoch)
	term.SetFlags(drpc.FlagClose)
	term.SetCode(uint32(codes.OK))
	x.NoError(t, c.Handle(t.Context(), term))

	err := stream.RecvMsg(&echo.EchoResponse{})
	x.Equal(t, io.EOF, err)
	x.True(t, rec.findClose(codes.DataLoss) == nil, "no DataLoss abort may be emitted")
}

// ---------------------------------------------------------------------------
// §6.3: dedup'd (seq <= L) frames are NEUTRAL — they neither count toward nor
// reset the beyond-window run. beyond, dup, beyond, beyond still accumulates
// to K_loud = 3 and fails the call with DATA_LOSS.
// ---------------------------------------------------------------------------

func TestDataLoss_DedupFramesAreNeutral(t *testing.T) {
	rec := &dlRecorder{}
	c, stream, cEpoch, sid := dlManyStream(t, rec, false)

	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 1, "one")))
	got := &echo.EchoResponse{}
	x.NoError(t, stream.RecvMsg(got))
	x.Equal(t, "one", got.GetMessage())

	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5000, "lost"))) // run: 1
	// Duplicate of the accepted frame: dedup'd, must NOT reset the run.
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 1, "dup")))
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5001, "lost"))) // run: 2
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 5002, "lost"))) // run: 3 -> loud

	err := stream.RecvMsg(&echo.EchoResponse{})
	x.Equal(t, codes.DataLoss, status.Code(err))
	x.True(t, rec.findClose(codes.DataLoss) != nil, "expected an abort CLOSE with code DataLoss")
}

// ---------------------------------------------------------------------------
// §6.3 server side: the window overrun cancels the handler with a DataLoss
// cause, and the unwind emits T{DATA_LOSS} (§10.7 ">W_fwd loss burst" row).
// Runs in real time (unreliable mode has live timers); no timing is asserted.
// ---------------------------------------------------------------------------

func TestDataLoss_ServerEmitsTerminal(t *testing.T) {
	is := newInjectServerMode(t, false, drpc.WithTiming(fastTiming))

	is.handle(dlLiveOpen(1, 30))
	h := is.recv(t) // creation ack H (§8)
	x.True(t, h != nil && h.GetFlags() == 0, "expected creation ack")

	// The accepted OPEN set L = 1; three consistent beyond-window data
	// frames are the loss-burst evidence (§6.3). The blocked Live handler is
	// unblocked via its ctx: the cancel cause rides the terminal.
	is.handle(dlClientData(1, 30, 5000))
	is.handle(dlClientData(1, 30, 5001))
	is.handle(dlClientData(1, 30, 5002))

	r := dlRecvTerminal(t, is)
	x.Equal(t, codes.DataLoss, codes.Code(r.GetCode()))
}

// ---------------------------------------------------------------------------
// §10.6 reliable mode, client side: a duplicate on a reliable channel means
// the transport itself duplicated a frame — the strict window (anything other
// than L+1) fails the call with INTERNAL and emits an abort CLOSE{INTERNAL}.
// ---------------------------------------------------------------------------

func TestReliableDuplicateData_ClientInternal(t *testing.T) {
	rec := &dlRecorder{}
	c, stream, cEpoch, sid := dlManyStream(t, rec, true) // reliable: strict window

	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 1, "one")))
	got := &echo.EchoResponse{}
	x.NoError(t, stream.RecvMsg(got))
	x.Equal(t, "one", got.GetMessage())

	// The same frame again: strict mode admits exactly L+1 = 2.
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 1, "one")))

	err := stream.RecvMsg(&echo.EchoResponse{})
	x.Equal(t, codes.Internal, status.Code(err))
	x.True(t, rec.findClose(codes.Internal) != nil, "expected an abort CLOSE with code Internal")
}

// ---------------------------------------------------------------------------
// §10.6 reliable mode, server side: a duplicate client data frame cancels the
// handler with an Internal cause and the unwind emits T{INTERNAL}.
// ---------------------------------------------------------------------------

func TestReliableDuplicateData_ServerInternal(t *testing.T) {
	is := newInjectServer(t) // reliable: no timers, deterministic

	is.handle(dlLiveOpen(1, 40))
	h := is.recv(t)
	x.True(t, h != nil && h.GetFlags() == 0, "expected creation ack")

	// seq 2 = L+1: accepted. The same seq again is a duplicate the reliable
	// contract forbids (§10.6): fail loud with INTERNAL.
	is.handle(dlClientData(1, 40, 2))
	is.handle(dlClientData(1, 40, 2))

	r := dlRecvTerminal(t, is)
	x.Equal(t, codes.Internal, codes.Code(r.GetCode()))
}

// ---------------------------------------------------------------------------
// §10.6 reliable mode: a duplicate OPEN on a live reliable call is equally a
// transport-duplicated frame — no retransmission exists that could explain it.
// The call dies with T{INTERNAL} (this replaced the old absorb-and-replay-H
// behavior, which is unreliable-mode ack recovery, §8).
// ---------------------------------------------------------------------------

func TestReliableDuplicateOpen_ServerInternal(t *testing.T) {
	is := newInjectServer(t)

	open := dlLiveOpen(1, 50)
	is.handle(proto.CloneOf(open))
	h := is.recv(t)
	x.True(t, h != nil && h.GetFlags() == 0, "expected creation ack")

	// Byte-identical OPEN again on the live call.
	is.handle(proto.CloneOf(open))

	r := dlRecvTerminal(t, is)
	x.Equal(t, codes.Internal, codes.Code(r.GetCode()))
}
