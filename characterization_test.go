package drpc_test

// characterization_test.go pins down, by execution, the exact end-state of the
// client and the server under adversarial conditions. Each test documents an
// observed guarantee or limitation; together they are the evidence behind the
// GUARANTEES / LIMITATIONS sections of README.md. Findings that are inherently
// timing-dependent use fastTiming and generous bounds.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/lossy"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Sensor-streaming core: server-stream data loss is a silent ordered gap.
// ---------------------------------------------------------------------------

func TestChar_ServerStreamDataLossIsSilentGap(t *testing.T) {
	bubble(t, func(t *testing.T) {
		// Drop every third server data frame. The delivery contract (§14) is an
		// ordered SUBSEQUENCE: the client sees fewer messages, in order, then a
		// clean io.EOF — never an error. This is the sensor use case.
		dropEveryThird := func(next drpc.FrameHandler) drpc.FrameHandler {
			var n atomic.Int64
			return lossy.New(next, lossy.Options{
				Drop: 1,
				Filter: func(f *drpc.Frame) bool {
					if f.GetFlags() == 0 && f.HasPayload() { // data frame
						return n.Add(1)%3 == 0
					}
					return false
				},
			})
		}
		client, stop := unreliablePipe(nil, dropEveryThird).Use(t)
		defer stop()

		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        9,
		}.Build())
		x.NoError(t, err)

		var seqs []uint32
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err) // never a mid-stream error
			seqs = append(seqs, res.GetSequence())
		}
		// A strict subsequence of 0..8, strictly increasing, ending in EOF.
		if !(len(seqs) > 0 && len(seqs) < 9) {
			t.Fatalf("expected a proper subsequence, got %d", len(seqs))
		}
		for i := 1; i < len(seqs); i++ {
			x.True(t, seqs[i] > seqs[i-1], "must stay ordered")
		}
	})
}

// ---------------------------------------------------------------------------
// Direct frame injection: the adversarial / out-of-state matrix. Each probe
// feeds a crafted frame straight into Server.Handle and records the reply.
// ---------------------------------------------------------------------------

// injectServer wires a registered server whose tx frames are captured, so a
// test can assert exactly what the server emits for an injected frame.
type injectServer struct {
	srv *drpc.Server
	out chan *drpc.Frame
}

// newInjectServer builds a reliable-mode server (no timers, RESET immediate)
// so injected-frame outcomes are deterministic.
func newInjectServer(t *testing.T, opts ...drpc.ServerOption) *injectServer {
	return newInjectServerMode(t, true, opts...)
}

func newInjectServerMode(t *testing.T, reliable bool, opts ...drpc.ServerOption) *injectServer {
	is := &injectServer{out: make(chan *drpc.Frame, 64)}
	tx := drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		is.out <- proto.CloneOf(f)
		return nil
	})
	is.srv = drpc.NewServer(tx, append([]drpc.ServerOption{drpc.WithReliable(reliable)}, opts...)...)
	echo.RegisterEchoServiceServer(is.srv, &echo.EchoServer{})
	t.Cleanup(is.srv.Stop)
	return is
}

func (is *injectServer) handle(f *drpc.Frame) { is.srv.Handle(context.Background(), f) }

// recv returns the next emitted frame or nil within a short window.
func (is *injectServer) recv(t *testing.T) *drpc.Frame {
	t.Helper()
	select {
	case f := <-is.out:
		return f
	case <-time.After(500 * time.Millisecond):
		return nil
	}
}

func openFrame(epoch, sid, seq uint32, method string) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	f.SetFlags(drpc.FlagOpen | drpc.FlagClose)
	f.SetMethod(method)
	// A zero-value request is a valid Once input.
	data, _ := proto.Marshal(&echo.EchoRequest{})
	f.SetPayload(data)
	return f
}

func TestChar_InjectionMatrix(t *testing.T) {
	t.Run("data frame for unknown sid -> RESET echoing its epoch", func(t *testing.T) {
		is := newInjectServer(t)
		f := &drpc.Frame{}
		f.SetEpoch(0xAABBCCDD)
		f.SetSid(42)
		f.SetSeq(2)
		f.SetPayload([]byte{})
		is.handle(f)

		r := is.recv(t)
		x.True(t, r != nil, "expected a RESET")
		x.Equal(t, drpc.FlagReset, r.GetFlags())
		x.Equal(t, 0xAABBCCDD, r.GetEpoch()) // echoes the offender's epoch
		x.Equal(t, 42, r.GetSid())
	})
	t.Run("OPEN with seq != 1 -> no call created, RESET", func(t *testing.T) {
		is := newInjectServer(t)
		f := openFrame(1, 5, 2, echo.EchoService_Once_FullMethodName) // seq 2
		is.handle(f)
		r := is.recv(t)
		x.True(t, r != nil && r.GetFlags() == drpc.FlagReset, "seq!=1 OPEN must not create a call")
	})
	t.Run("OPEN with out-of-range method_index -> UNIMPLEMENTED terminal", func(t *testing.T) {
		is := newInjectServer(t)
		f := &drpc.Frame{}
		f.SetEpoch(1)
		f.SetSid(6)
		f.SetSeq(1)
		f.SetFlags(drpc.FlagOpen | drpc.FlagClose)
		f.SetMethodIndex(1 << 20)
		f.SetPayload([]byte{})
		is.handle(f)
		r := is.recv(t)
		x.True(t, r != nil, "expected a terminal")
		x.Equal(t, drpc.FlagClose, r.GetFlags())
		x.Equal(t, codes.Unimplemented, codes.Code(r.GetCode()))
	})
	t.Run("OPEN for unknown method string -> UNIMPLEMENTED terminal", func(t *testing.T) {
		is := newInjectServer(t)
		f := openFrame(1, 7, 1, "/echo.EchoService/Nope")
		is.handle(f)
		r := is.recv(t)
		x.True(t, r != nil && codes.Code(r.GetCode()) == codes.Unimplemented, "unknown method")
	})
	t.Run("RESET with a foreign epoch is ignored (not our incarnation)", func(t *testing.T) {
		is := newInjectServer(t)
		// Start a real streaming call so there is live state to (not) kill.
		open := &drpc.Frame{}
		open.SetEpoch(1)
		open.SetSid(8)
		open.SetSeq(1)
		open.SetFlags(drpc.FlagOpen)
		open.SetMethod(echo.EchoService_Live_FullMethodName)
		is.handle(open)
		_ = is.recv(t) // creation ack H

		// A RESET whose echoed epoch is NOT the server's is dropped.
		reset := &drpc.Frame{}
		reset.SetEpoch(0xDEADBEEF) // not the server epoch
		reset.SetSid(8)
		reset.SetFlags(drpc.FlagReset)
		is.handle(reset)

		// The call is still alive: a subsequent data frame is accepted (the
		// server does not RESET a live sid), so no RESET is emitted.
		x.True(t, is.recv(t) == nil, "foreign-epoch RESET must be ignored")
	})
	t.Run("huge seq on a live stream is dropped, no wedge (unreliable)", func(t *testing.T) {
		is := newInjectServerMode(t, false) // default timing: no probe within the recv window
		open := &drpc.Frame{}
		open.SetEpoch(1)
		open.SetSid(9)
		open.SetSeq(1)
		open.SetFlags(drpc.FlagOpen)
		open.SetMethod(echo.EchoService_Live_FullMethodName)
		is.handle(open)
		_ = is.recv(t) // H

		// A single beyond-window data frame is silently dropped (no DATA_LOSS
		// from one frame, no RESET, no crash).
		poison := &drpc.Frame{}
		poison.SetEpoch(1)
		poison.SetSid(9)
		poison.SetSeq(1 << 30)
		poison.SetPayload([]byte{})
		is.handle(poison)
		x.True(t, is.recv(t) == nil, "a lone beyond-window frame is dropped silently")
	})
}

// ---------------------------------------------------------------------------
// At-most-once boundary: replay within TTL, RESET after the aged watermark.
// (The successful-recovery and stale-OPEN cases are in TestEventualTermination;
// here we pin the DUPLICATE-OPEN-on-a-live-call behavior: it never forks.)
// ---------------------------------------------------------------------------

func TestChar_DuplicateOpenOnLiveCallDoesNotFork(t *testing.T) {
	var execs atomic.Int32
	is := newInjectServer(t, countExecs(&execs))

	// Two identical unary OPENs for the same sid, back to back.
	f := openFrame(1, 3, 1, echo.EchoService_Once_FullMethodName)
	is.handle(proto.CloneOf(f))
	is.handle(proto.CloneOf(f))
	time.Sleep(100 * time.Millisecond)

	// Exactly one execution; at least one terminal emitted.
	x.Equal(t, 1, int(execs.Load()))
	x.True(t, is.recv(t) != nil, "a terminal must be emitted")
}

// ---------------------------------------------------------------------------
// LIMITATION: a RESET echoing the server's own epoch tears down a live call.
// On raw UDP this is spoofable by anyone who can observe the epoch (§15).
// The control shows a foreign-epoch RESET is ignored; the spoof is not.
// ---------------------------------------------------------------------------

func TestChar_SpoofedResetTearsDownLiveCall(t *testing.T) {
	is := newInjectServer(t)

	open := &drpc.Frame{}
	open.SetEpoch(1)
	open.SetSid(20)
	open.SetSeq(1)
	open.SetFlags(drpc.FlagOpen)
	open.SetMethod(echo.EchoService_Live_FullMethodName)
	is.handle(open)
	h := is.recv(t) // creation ack carries the server epoch
	x.True(t, h != nil, "expected creation ack")
	serverEpoch := h.GetEpoch()

	// Spoof: a RESET echoing the SERVER's epoch is acted on and kills the call.
	reset := &drpc.Frame{}
	reset.SetEpoch(serverEpoch)
	reset.SetSid(20)
	reset.SetFlags(drpc.FlagReset)
	is.handle(reset)
	time.Sleep(100 * time.Millisecond) // let the handler unwind

	// The call is gone: a later data frame for the sid is now for an unknown
	// call and draws a RESET (in reliable mode there is no tombstone).
	data := &drpc.Frame{}
	data.SetEpoch(1)
	data.SetSid(20)
	data.SetSeq(2)
	data.SetPayload([]byte{})
	is.handle(data)
	r := is.recv(t)
	x.True(t, r != nil && r.GetFlags() == drpc.FlagReset, "spoofed reset should have torn down the call")
}

// ---------------------------------------------------------------------------
// A healthy but idle bidi stream must NOT be killed by liveness — PING and the
// stream probe keep it alive with no application traffic (the anti-pattern an
// idle-timeout design would fail).
// ---------------------------------------------------------------------------

func TestChar_HealthyIdleBidiNotKilled(t *testing.T) {
	bubble(t, func(t *testing.T) {
		client, stop := unreliablePipe(nil, nil).Use(t)
		defer stop()

		stream, err := client.Live(t.Context())
		x.NoError(t, err)
		err = stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build())
		x.NoError(t, err)
		_, err = stream.Recv()
		x.NoError(t, err)

		// Idle well past T_live (600ms) with zero application traffic.
		time.Sleep(3 * fastTiming.Liveness)

		// The stream is still fully usable.
		err = stream.Send(echo.EchoRequest_builder{Message: "b", Repeat: 1}.Build())
		x.NoError(t, err)
		res, err := stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "b", res.GetMessage())

		// Clean shutdown so teardown is prompt.
		x.NoError(t, stream.CloseSend())
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
	})
}

// ---------------------------------------------------------------------------
// DIVERGENCE from gRPC: rich status details (status.WithDetails) are dropped.
// Only code + message survive the wire (documented in PROTOCOL.md §5).
// ---------------------------------------------------------------------------

func TestChar_StatusDetailsDropped(t *testing.T) {
	bubble(t, func(t *testing.T) {
		client, stop := unreliablePipe(nil, nil).Use(t)
		defer stop()

		// Server returns a status carrying details.
		st := status.New(codes.FailedPrecondition, "nope")
		withDetails, derr := st.WithDetails(echo.EchoRequest_builder{Message: "detail"}.Build())
		if derr != nil {
			withDetails = st // some environments cannot attach; still asserts code+msg
		}
		client.service.Err = withDetails.Err()

		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		got, ok := status.FromError(err)
		x.True(t, ok, "must be a status")
		x.Equal(t, codes.FailedPrecondition, got.Code()) // code preserved
		x.Equal(t, "nope", got.Message())                // message preserved
		x.Equal(t, 0, len(got.Details()))                // details DROPPED (divergence)
	})
}

// ---------------------------------------------------------------------------
// L5 fix: in reliable mode a seq gap or duplicate fails the call with INTERNAL
// (§10.6, decision Q6) — a broken "reliable" transport is surfaced, not hidden.
// ---------------------------------------------------------------------------

func TestChar_ReliableModeGapIsInternal(t *testing.T) {
	// Reliable mode; drop the FIRST server data frame of a multi-message
	// stream so the next arrives with a seq gap.
	dropFirstData := dropFirst(func(f *drpc.Frame) bool {
		return f.GetFlags() == 0 && f.HasPayload()
	})
	client, stop := PipeOption{
		ServerOpts: []drpc.ServerOption{drpc.WithReliable(true)},
		ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true)},
		S2C:        dropFirstData,
	}.Use(t)
	defer stop()

	stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
		Repeat:        3,
	}.Build())
	x.NoError(t, err)

	// The gap is detected and the call fails loudly with INTERNAL.
	var lastErr error
	for {
		_, err := stream.Recv()
		if err != nil {
			lastErr = err
			break
		}
	}
	x.Equal(t, codes.Internal, status.Code(lastErr))
}

// ---------------------------------------------------------------------------
// L4 fix: a per-peer live-call cap bounds the handler goroutines a single peer
// can spawn (§15). A flood of valid OPENs past the cap is refused with
// RESOURCE_EXHAUSTED instead of growing without bound.
// ---------------------------------------------------------------------------

func TestChar_LiveCallCap(t *testing.T) {
	const cap = 3
	is := newInjectServer(t, drpc.WithLimits(drpc.Limits{MaxLiveCalls: cap}))

	// Open `cap` long-lived bidi calls (each handler blocks in Recv).
	for sid := uint32(1); sid <= cap; sid++ {
		open := &drpc.Frame{}
		open.SetEpoch(1)
		open.SetSid(sid)
		open.SetSeq(1)
		open.SetFlags(drpc.FlagOpen)
		open.SetMethod(echo.EchoService_Live_FullMethodName)
		is.handle(open)
		h := is.recv(t)
		x.True(t, h != nil && h.GetFlags() == 0, "creation ack expected")
	}

	// One more distinct sid is over the cap: refused with RESOURCE_EXHAUSTED.
	over := &drpc.Frame{}
	over.SetEpoch(1)
	over.SetSid(cap + 1)
	over.SetSeq(1)
	over.SetFlags(drpc.FlagOpen)
	over.SetMethod(echo.EchoService_Live_FullMethodName)
	is.handle(over)
	r := is.recv(t)
	x.True(t, r != nil, "expected a terminal")
	x.Equal(t, codes.ResourceExhausted, codes.Code(r.GetCode()))
}

// ---------------------------------------------------------------------------
// L3 hardening: a client stream locks to the server incarnation of its first
// accepted frame — a foreign-epoch frame on a live client sid cannot inject
// data, poison the window, or flip the learned-index epoch (symmetric with
// the server's incarnation isolation). Raw-UDP injection is still possible
// with a *matching* epoch; encrypted transport is the real mitigation (§15).
// ---------------------------------------------------------------------------

func TestChar_ClientRejectsForeignEpochFrame(t *testing.T) {
	frames := make(chan *drpc.Frame, 16)
	conn := drpc.NewConn(drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		frames <- f
		return nil
	}), drpc.WithReliable(true))
	defer conn.Close(nil)

	sd := &grpc.StreamDesc{StreamName: "Many", ServerStreams: true}
	stream, err := conn.NewStream(t.Context(), sd, echo.EchoService_Many_FullMethodName)
	x.NoError(t, err)
	// Server-streaming emits its OPEN|CLOSE on the first SendMsg.
	x.NoError(t, stream.SendMsg(echo.EchoRequest_builder{}.Build()))
	<-frames // the client's OPEN

	sid := uint32(1) // first sid

	// The real server (epoch 7) sends a data frame: the stream locks to 7.
	good := &drpc.Frame{}
	good.SetEpoch(7)
	good.SetSid(sid)
	good.SetSeq(1)
	data, _ := proto.Marshal(echo.EchoResponse_builder{Message: "real"}.Build())
	good.SetPayload(data)
	x.NoError(t, conn.Handle(t.Context(), good))

	got := &echo.EchoResponse{}
	x.NoError(t, stream.RecvMsg(got))
	x.Equal(t, "real", got.GetMessage())

	// A spoofer (epoch 99) injects a forward-seq data frame on the live sid.
	// It is dropped: the next legitimate frame from epoch 7 still delivers.
	evil := &drpc.Frame{}
	evil.SetEpoch(99)
	evil.SetSid(sid)
	evil.SetSeq(2)
	edata, _ := proto.Marshal(echo.EchoResponse_builder{Message: "spoof"}.Build())
	evil.SetPayload(edata)
	x.NoError(t, conn.Handle(t.Context(), evil))

	good2 := &drpc.Frame{}
	good2.SetEpoch(7)
	good2.SetSid(sid)
	good2.SetSeq(2)
	data2, _ := proto.Marshal(echo.EchoResponse_builder{Message: "real2"}.Build())
	good2.SetPayload(data2)
	x.NoError(t, conn.Handle(t.Context(), good2))

	got2 := &echo.EchoResponse{}
	x.NoError(t, stream.RecvMsg(got2))
	x.Equal(t, "real2", got2.GetMessage()) // spoof was dropped, not delivered
}

// ---------------------------------------------------------------------------
// §4.4: message size is the adapter's concern. When the adapter's Handle
// refuses a send with ErrMessageTooLarge, the core fails the owning call with
// ResourceExhausted — synchronously, never fragmenting or retrying.
// ---------------------------------------------------------------------------

func TestChar_AdapterRefusesTooLargeSend(t *testing.T) {
	// A reliable-mode conn keeps protocol timers out of the picture; the
	// failure is synchronous and needs no peer at all.
	newConn := func(limit int) *drpc.Conn {
		return drpc.NewConn(drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
			if len(f.GetPayload()) > limit {
				return fmt.Errorf("refused %d bytes: %w", len(f.GetPayload()), drpc.ErrMessageTooLarge)
			}
			return nil
		}), drpc.WithReliable(true))
	}

	t.Run("unary: status surfaces via the invoke result", func(t *testing.T) {
		conn := newConn(8)
		defer conn.Close(nil)
		client := echo.NewEchoServiceClient(conn)

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message: "far larger than eight bytes",
		}.Build())
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
	})
	t.Run("client-streaming: Send returns the status itself", func(t *testing.T) {
		conn := newConn(8)
		defer conn.Close(nil)
		client := echo.NewEchoServiceClient(conn)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err) // the eager OPEN is tiny and passes
		err = stream.Send(echo.EchoRequest_builder{
			Message: "far larger than eight bytes",
		}.Build())
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
	})
	t.Run("server-side refusal surfaces the handler's status, not INTERNAL", func(t *testing.T) {
		// A refused frame must not burn its seq: in reliable mode the strict
		// window would then reject the terminal carrying the real status and
		// fail the call with INTERNAL "lost or reordered a frame" instead.
		var conn *drpc.Conn
		srv := drpc.NewServer(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			if len(f.GetPayload()) > 64 {
				return fmt.Errorf("refused %d bytes: %w", len(f.GetPayload()), drpc.ErrMessageTooLarge)
			}
			return conn.Handle(ctx, f)
		}), drpc.WithReliable(true))
		defer srv.Stop()
		echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
		conn = drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			return srv.Handle(ctx, f)
		}), drpc.WithReliable(true))
		defer conn.Close(nil)
		client := echo.NewEchoServiceClient(conn)

		// The response mirrors the 100-byte request: over the server's limit.
		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message: strings.Repeat("x", 100),
			Repeat:  1,
		}.Build())
		x.NoError(t, err)
		_, err = stream.Recv()
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
	})
}

// ---------------------------------------------------------------------------
// L8 verification: an OK response that marshals to zero bytes round-trips as a
// valid empty message, not an Internal error (SetPayload forces presence).
// ---------------------------------------------------------------------------

func TestChar_EmptyOkResponseRoundTrips(t *testing.T) {
	client, stop := unreliablePipe(nil, nil).Use(t)
	defer stop()

	// Noop echoes the request; an all-default request marshals to 0 bytes.
	res, err := client.Noop(t.Context(), &echo.EchoRequest{})
	x.NoError(t, err) // not Internal
	x.Equal(t, "", res.GetMessage())
}
