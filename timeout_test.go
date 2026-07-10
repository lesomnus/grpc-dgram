package drpc_test

import (
	"context"
	"io"
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

// fastTiming keeps unreliable-mode tests quick. Derived values:
// T_probe = Liveness/3 = 200ms, sweep tick = 25ms.
var fastTiming = drpc.Timing{
	Call:       400 * time.Millisecond,
	Liveness:   600 * time.Millisecond,
	Retransmit: 50 * time.Millisecond,
	Tombstone:  1500 * time.Millisecond,
	Hold:       50 * time.Millisecond,
}

func unreliablePipe(c2s, s2c func(drpc.FrameHandler) drpc.FrameHandler, extra ...drpc.ServerOption) PipeOption {
	return PipeOption{
		ConnOpts: []drpc.ConnOption{drpc.WithReliable(false), drpc.WithTiming(fastTiming)},
		ServerOpts: append(
			[]drpc.ServerOption{drpc.WithReliable(false), drpc.WithTiming(fastTiming)},
			extra...),
		C2S: c2s,
		S2C: s2c,
	}
}

// killswitch drops every frame once tripped — the peer "vanishes".
type killswitch struct {
	next drpc.FrameHandler
	dead atomic.Bool
}

func (k *killswitch) Handle(ctx context.Context, f *drpc.Frame) error {
	if k.dead.Load() {
		return nil
	}
	return k.next.Handle(ctx, f)
}

// dropFirst drops the first frame matching match; everything else passes.
func dropFirst(match func(f *drpc.Frame) bool) func(drpc.FrameHandler) drpc.FrameHandler {
	var done atomic.Bool
	return func(next drpc.FrameHandler) drpc.FrameHandler {
		return lossy.New(next, lossy.Options{Drop: 1, Filter: func(f *drpc.Frame) bool {
			return match(f) && done.CompareAndSwap(false, true)
		}})
	}
}

func countExecs(n *atomic.Int32) drpc.ServerOption {
	return drpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		n.Add(1)
		return handler(ctx, req)
	})
}

func isTerminal(f *drpc.Frame) bool  { return f.GetFlags() == drpc.FlagClose && f.HasCode() }
func isHalfClose(f *drpc.Frame) bool { return f.GetFlags() == drpc.FlagClose && !f.HasCode() }

// TestEventualTermination is the M3 DoD (ROADMAP M3, PROTOCOL.md §10.7):
// under frame loss and peer disappearance, every call terminates within its
// bound — and where the machinery allows, it terminates *successfully*.
func TestEventualTermination(t *testing.T) {
	t.Run("blackhole unary fails within T_call", func(t *testing.T) {
		blackhole := func(next drpc.FrameHandler) drpc.FrameHandler {
			return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
				return nil // eat everything
			})
		}
		client, stop := unreliablePipe(blackhole, nil).Use(t)
		defer stop()

		start := time.Now()
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.Equal(t, codes.DeadlineExceeded, status.Code(err))
		x.True(t, time.Since(start) < 3*fastTiming.Call, "bounded by T_call")
	})
	t.Run("lost unary response is recovered, not re-executed", func(t *testing.T) {
		var execs atomic.Int32
		drop := dropFirst(isTerminal)
		client, stop := unreliablePipe(nil, drop, countExecs(&execs)).Use(t)
		defer stop()

		// The response T is dropped once; the client's OPEN retransmission
		// hits the server tombstone, which replays the stored T
		// (PROTOCOL.md §9.2, §10.3): the call *succeeds* under loss.
		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())
		x.Equal(t, 1, int(execs.Load()))
	})
	t.Run("lost half-close is retransmitted", func(t *testing.T) {
		drop := dropFirst(isHalfClose)
		client, stop := unreliablePipe(drop, nil).Use(t)
		defer stop()

		up, err := client.Buff(t.Context())
		x.NoError(t, err)
		err = up.Send(echo.EchoRequest_builder{
			Message:       "abc",
			Repeat:        1,
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)

		// CloseSend's frame is dropped once; without retransmission the
		// handler would wait for EOF forever and CloseAndRecv would hang.
		batch, err := up.CloseAndRecv()
		x.NoError(t, err)
		x.Equal(t, 1, len(batch.GetItems()))
	})
	t.Run("lost stream terminal is recovered by probe and replay", func(t *testing.T) {
		drop := dropFirst(isTerminal)
		client, stop := unreliablePipe(nil, drop).Use(t)
		defer stop()

		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        1,
		}.Build())
		x.NoError(t, err)

		_, err = stream.Recv()
		x.NoError(t, err)

		// The T is dropped; the client has nothing left to retransmit. The
		// stream probe pokes the server tombstone into replaying it
		// (PROTOCOL.md §10.5): bounded by ~T_probe + RTI.
		start := time.Now()
		_, err = stream.Recv()
		x.ErrorIs(t, err, io.EOF)
		x.True(t, time.Since(start) < 4*fastTiming.Liveness/3, "bounded by probe cadence")
	})
	t.Run("vanished client cannot wedge the server", func(t *testing.T) {
		ks := &killswitch{}
		c2s := func(next drpc.FrameHandler) drpc.FrameHandler {
			ks.next = next
			return ks
		}
		client, stop := unreliablePipe(c2s, nil).Use(t)
		defer stop()

		stream, err := client.Live(t.Context())
		x.NoError(t, err)
		err = stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build())
		x.NoError(t, err)
		_, err = stream.Recv()
		x.NoError(t, err) // the handler is live and blocked in Recv

		ks.dead.Store(true) // the client machine disappears

		// Peer liveness (PROTOCOL.md §10.4) cancels the handler; this is the
		// case that wedged GracefulStop forever in the old implementation.
		done := make(chan struct{})
		go func() {
			client.server.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(4 * fastTiming.Liveness):
			t.Fatal("GracefulStop wedged: handler leaked past the liveness bound")
		}
	})
	t.Run("vanished server fails client calls within T_live", func(t *testing.T) {
		ks := &killswitch{}
		s2c := func(next drpc.FrameHandler) drpc.FrameHandler {
			ks.next = next
			return ks
		}
		client, stop := unreliablePipe(nil, s2c).Use(t)
		defer stop()

		stream, err := client.Live(t.Context())
		x.NoError(t, err)
		err = stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build())
		x.NoError(t, err)
		_, err = stream.Recv()
		x.NoError(t, err)

		ks.dead.Store(true) // the server disappears

		start := time.Now()
		_, err = stream.Recv()
		x.Equal(t, codes.Unavailable, status.Code(err))
		x.True(t, time.Since(start) < 3*fastTiming.Liveness, "bounded by T_live")
	})
	t.Run("at-most-once: duplicate and stale OPENs never re-execute", func(t *testing.T) {
		var execs atomic.Int32
		client, stop := unreliablePipe(nil, nil, countExecs(&execs)).Use(t)
		defer stop()

		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)
		x.Equal(t, 1, int(execs.Load()))

		open := proto.CloneOf(client.firstTxPayload(t)) // the unary OPEN|CLOSE

		// A network-duplicated OPEN within TTL_tomb hits the tombstone
		// (PROTOCOL.md §9.2): replayed, never re-executed.
		err = client.server.Handle(t.Context(), proto.CloneOf(open))
		x.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		x.Equal(t, 1, int(execs.Load()))

		// One delivered later than TTL_tomb is rejected by the aged
		// watermark (PROTOCOL.md §9.4): RESET, never re-executed.
		time.Sleep(fastTiming.Tombstone + 300*time.Millisecond)
		err = client.server.Handle(t.Context(), proto.CloneOf(open))
		x.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
		x.Equal(t, 1, int(execs.Load()))
	})
}
