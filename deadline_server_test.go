package drpc_test

// deadline_server_test.go pins the server side of PROTOCOL.md §10.2
// (Deadlines) and its Appendix B knob: the remaining caller budget travels as
// Frame.timeout on OPEN; the server bounds the handler ctx by it, clamped by
// WithMaxHandlerTimeout (off by default); a present but non-positive budget
// yields an already-expired handler ctx; on expiry the server sends — and, in
// unreliable mode, tombstone-stores — T{DEADLINE_EXCEEDED}. Both sides
// enforce independently: enforcement never depends on a frame arriving.

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// reliablePipe keeps protocol timers out of the picture (PROTOCOL.md §10.6):
// what remains is exactly the deadline machinery under test.
func reliablePipe() PipeOption {
	return PipeOption{
		ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true)},
		ServerOpts: []drpc.ServerOption{drpc.WithReliable(true)},
	}
}

// captureServer is a registered echo server whose emitted frames land on a
// buffered channel. Unlike injectServer.recv (a 500ms REAL-time window), its
// waits run on the caller's clock, so it is bubble-safe. The caller must stop
// it inside the bubble (defer cs.srv.Stop()) or the sweeper/handler
// goroutines would leak past the bubble and panic synctest.
type captureServer struct {
	srv     *drpc.Server
	service *echo.EchoServer
	out     chan *drpc.Frame
}

func newCaptureServer(opts ...drpc.ServerOption) *captureServer {
	cs := &captureServer{
		service: &echo.EchoServer{},
		out:     make(chan *drpc.Frame, 64),
	}
	tx := drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		cs.out <- proto.CloneOf(f)
		return nil
	})
	cs.srv = drpc.NewServer(tx, opts...)
	echo.RegisterEchoServiceServer(cs.srv, cs.service)
	return cs
}

func (cs *captureServer) handle(f *drpc.Frame) { cs.srv.Handle(context.Background(), f) }

// recvWithin returns the next emitted frame, failing the test if none shows
// up within d on the caller's (possibly fake) clock.
func (cs *captureServer) recvWithin(t *testing.T, d time.Duration) *drpc.Frame {
	t.Helper()
	select {
	case f := <-cs.out:
		return f
	case <-time.After(d):
		t.Fatal("no frame emitted within the window")
		return nil
	}
}

// tryRecv returns an already-emitted frame, or nil without waiting.
func (cs *captureServer) tryRecv() *drpc.Frame {
	select {
	case f := <-cs.out:
		return f
	default:
		return nil
	}
}

// voidOpen crafts a unary OPEN|CLOSE for Once whose OverVoid request makes
// the handler call service.Hit and then block in <-ctx.Done(), carrying
// budget as Frame.timeout. The client epoch is arbitrary nonzero; an OPEN's
// seq MUST be 1 (PROTOCOL.md §7).
func voidOpen(sid uint32, budget time.Duration) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(1)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen | drpc.FlagClose)
	f.SetMethod(echo.EchoService_Once_FullMethodName)
	data, err := proto.Marshal(echo.Void())
	if err != nil {
		panic(err)
	}
	f.SetPayload(data)
	f.SetTimeout(durationpb.New(budget))
	return f
}

// §10.2 wire propagation: the remaining caller budget travels as
// Frame.timeout on OPEN — and only when there is one to travel (a streaming
// call without a caller deadline has no absolute deadline by design).
func TestDeadline_BudgetTravelsOnOpen(t *testing.T) {
	t.Run("unary caller deadline propagates", func(t *testing.T) {
		client, stop := reliablePipe().Use(t)
		defer stop()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		_, err := client.Once(ctx, &echo.EchoRequest{})
		x.NoError(t, err)

		open := client.firstTxPayload(t) // the unary OPEN|CLOSE
		x.True(t, open.HasTimeout(), "the caller budget must travel on OPEN")
		d := open.GetTimeout().AsDuration()
		x.True(t, d > 0, "the propagated budget must be positive")
		x.True(t, d <= 3*time.Second, "the budget must not exceed the caller's")
	})
	t.Run("unary without deadline injects T_call (unreliable)", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			client, stop := unreliablePipe(nil, nil).Use(t)
			defer stop()

			_, err := client.Once(t.Context(), &echo.EchoRequest{})
			x.NoError(t, err)

			open := client.firstTxPayload(t)
			x.True(t, open.HasTimeout(), "a unary call always carries a budget")
			d := open.GetTimeout().AsDuration()
			x.True(t, d > 0 && d <= fastTiming.Call, "the injected budget is T_call")
		})
	})
	t.Run("streaming without deadline sends no timeout", func(t *testing.T) {
		client, stop := reliablePipe().Use(t)
		defer stop()

		stream, err := client.Many(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)
		_, err = stream.Recv() // Repeat 0: clean EOF, so the OPEN is recorded
		x.ErrorIs(t, err, io.EOF)

		open := client.firstTxPayload(t) // Many's OPEN|CLOSE carries the request
		x.False(t, open.HasTimeout(), "no caller deadline -> no timeout on a stream OPEN")
	})
	t.Run("streaming caller deadline propagates", func(t *testing.T) {
		client, stop := reliablePipe().Use(t)
		defer stop()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		stream, err := client.Many(ctx, &echo.EchoRequest{})
		x.NoError(t, err)
		_, err = stream.Recv()
		x.ErrorIs(t, err, io.EOF)

		open := client.firstTxPayload(t)
		x.True(t, open.HasTimeout(), "a caller deadline, if present, propagates on OPEN")
		d := open.GetTimeout().AsDuration()
		x.True(t, d > 0 && d <= 3*time.Second)
	})
}

// §10.2: the received budget actually bounds the handler ctx end to end — a
// handler blocked in <-ctx.Done() unwinds, and the client observes
// DEADLINE_EXCEEDED within ~the caller budget, well before T_call. The bubble
// additionally proves the handler did not leak past its bound.
func TestDeadline_BlockedHandlerFailsWithinBudget(t *testing.T) {
	bubble(t, func(t *testing.T) {
		client, stop := unreliablePipe(nil, nil).Use(t)
		defer stop()
		client.service.SetHit(func() {})

		const budget = 150 * time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()

		start := time.Now()
		_, err := client.Once(ctx, echo.Void())
		x.Equal(t, codes.DeadlineExceeded, status.Code(err))
		elapsed := time.Since(start)
		x.True(t, elapsed >= budget, "nothing may fail before the budget")
		x.True(t, elapsed < fastTiming.Call, "the caller budget governs, not T_call")
	})
}

// §10.2 "Both sides enforce independently: enforcement never depends on a
// frame arriving": with no client past the crafted OPEN — no abort, no
// retransmission, nothing — the handler ctx expires by itself and the server
// emits T{DEADLINE_EXCEEDED}.
func TestDeadline_ServerEnforcesWithoutClientFrames(t *testing.T) {
	bubble(t, func(t *testing.T) {
		cs := newCaptureServer(drpc.WithReliable(true))
		defer cs.srv.Stop()
		hit := make(chan struct{})
		cs.service.SetHit(func() { close(hit) })

		const budget = 100 * time.Millisecond
		start := time.Now()
		cs.handle(voidOpen(1, budget))

		f := cs.recvWithin(t, time.Minute)
		elapsed := time.Since(start)

		select {
		case <-hit:
		default:
			t.Fatal("handler never started")
		}
		x.Equal(t, drpc.FlagClose, f.GetFlags())
		x.Equal(t, codes.DeadlineExceeded, codes.Code(f.GetCode()))
		x.True(t, elapsed >= budget, "the T must come from expiry, not an early failure")
		x.True(t, elapsed < 10*budget, "expiry must be prompt")
	})
}

// §10.2 + Appendix B: WithMaxHandlerTimeout clamps a client-asserted budget;
// it is OFF by default (gRPC-equivalent trust) — without the option a huge
// budget is honored.
func TestDeadline_MaxHandlerTimeout(t *testing.T) {
	const huge = time.Hour
	const short = 100 * time.Millisecond

	t.Run("clamps a huge client budget", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			cs := newCaptureServer(drpc.WithReliable(true), drpc.WithMaxHandlerTimeout(short))
			defer cs.srv.Stop()
			cs.service.SetHit(func() {})

			start := time.Now()
			cs.handle(voidOpen(1, huge))

			f := cs.recvWithin(t, time.Minute)
			elapsed := time.Since(start)
			x.Equal(t, codes.DeadlineExceeded, codes.Code(f.GetCode()))
			x.True(t, elapsed >= short, "the clamp is a deadline, not a rejection")
			x.True(t, elapsed < 10*short, "the clamp governs, not the client's budget")
		})
	})
	t.Run("off by default: a huge budget is honored", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			cs := newCaptureServer(drpc.WithReliable(true))
			defer cs.srv.Stop()
			hit := make(chan struct{})
			cs.service.SetHit(func() { close(hit) })

			cs.handle(voidOpen(1, huge))
			<-hit // the handler is running and blocked in <-ctx.Done()

			// Well past any would-be default clamp the handler still runs: no
			// terminal has been emitted (any unwind would emit one). Bounded
			// check — the huge budget is not waited out; the deferred Stop
			// unblocks the handler at teardown.
			time.Sleep(10 * short)
			x.True(t, cs.tryRecv() == nil, "no default clamp may exist")
		})
	})
}

// §10.2: a present but non-positive budget (the deadline expired while the
// OPEN was in flight, or a crafted frame) yields an already-expired handler
// ctx — the handler unwinds into T{DEADLINE_EXCEEDED} at once, never running
// unbounded, never blocking.
func TestDeadline_NonPositiveBudget(t *testing.T) {
	bubble(t, func(t *testing.T) {
		var execs atomic.Int32
		cs := newCaptureServer(drpc.WithReliable(true), countExecs(&execs))
		defer cs.srv.Stop()
		cs.service.SetHit(func() {})

		start := time.Now()
		cs.handle(voidOpen(1, -time.Second))

		f := cs.recvWithin(t, time.Minute)
		x.Equal(t, drpc.FlagClose, f.GetFlags())
		x.Equal(t, codes.DeadlineExceeded, codes.Code(f.GetCode()))
		// The blocking point (<-ctx.Done()) is released before the handler
		// reaches it: the answer costs no (fake) time at all.
		x.True(t, time.Since(start) < 50*time.Millisecond, "must answer at once")
		// The handler is invoked — the expired ctx is the handler's to
		// observe, gRPC-equivalent shape — but unwinds immediately.
		x.Equal(t, 1, int(execs.Load()))
	})
}

// §10.2 expiry + §9.2: in unreliable mode the T{DEADLINE_EXCEEDED} is
// tombstone-stored — a duplicate OPEN for the same sid draws a byte-identical
// replay, never a re-execution.
func TestDeadline_ExpiryTombstoneReplay(t *testing.T) {
	bubble(t, func(t *testing.T) {
		var execs atomic.Int32
		cs := newCaptureServer(drpc.WithReliable(false), drpc.WithTiming(fastTiming), countExecs(&execs))
		defer cs.srv.Stop()
		cs.service.SetHit(func() {})

		open := voidOpen(1, 100*time.Millisecond)
		cs.handle(proto.CloneOf(open))

		first := cs.recvWithin(t, time.Minute)
		x.Equal(t, codes.DeadlineExceeded, codes.Code(first.GetCode()))
		x.Equal(t, 1, int(execs.Load()))

		// Tombstone replays are rate-limited to 1/RTI (§9.2): space the
		// duplicate past RTI on the fake clock. (The per-peer aggregate reply
		// budget — 64/RTI, §15 — is nowhere near.)
		time.Sleep(2 * fastTiming.Retransmit)

		cs.handle(proto.CloneOf(open))
		replay := cs.recvWithin(t, time.Minute)

		b1, err := proto.Marshal(first)
		x.NoError(t, err)
		b2, err := proto.Marshal(replay)
		x.NoError(t, err)
		x.True(t, bytes.Equal(b1, b2), "the replay must be byte-identical to the stored T")
		x.Equal(t, 1, int(execs.Load()), "a duplicate OPEN must never re-execute")
	})
}
