package drpc_test

import (
	"io"
	"testing"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/lossy"
	"github.com/lesomnus/grpc-dgram/internal/x"
)

// dataOnly selects pure data frames: no flags, payload present.
func dataOnly(f *drpc.Frame) bool { return f.GetFlags() == 0 && f.HasPayload() }

func TestFaultInjection(t *testing.T) {
	t.Run("duplication is invisible", func(t *testing.T) {
		// Every frame in both directions is delivered twice; seq dedup and
		// the duplicate-OPEN rules must make it invisible to the app.
		dup := func(next drpc.FrameHandler) drpc.FrameHandler {
			return lossy.New(next, lossy.Options{Seed: 1, Dup: 1})
		}
		client, stop := PipeOption{C2S: dup, S2C: dup}.Use(t)
		defer stop()

		ctx := t.Context()

		res, err := client.Once(ctx, echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		stream, err := client.Many(ctx, echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        3,
		}.Build())
		x.NoError(t, err)
		seqs := []uint32{}
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err)
			seqs = append(seqs, res.GetSequence())
		}
		x.Equal(t, []uint32{0, 1, 2}, seqs)

		up, err := client.Buff(ctx)
		x.NoError(t, err)
		err = up.Send(echo.EchoRequest_builder{
			Message:       "abc",
			Repeat:        2,
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		batch, err := up.CloseAndRecv()
		x.NoError(t, err)
		x.Equal(t, 2, len(batch.GetItems()))
	})
	t.Run("one-step reorder yields an ordered subsequence", func(t *testing.T) {
		// Every server data frame is held one step, so the wire order is
		// 2,1,4,3,... — the forward-only window accepts the newer frame and
		// drops the older one: the app sees an ordered subsequence
		// (PROTOCOL.md §14), deterministically [1,3,5,7] here.
		reorder := func(next drpc.FrameHandler) drpc.FrameHandler {
			return lossy.New(next, lossy.Options{Seed: 1, Hold: 1, Filter: dataOnly})
		}
		client, stop := PipeOption{S2C: reorder}.Use(t)
		defer stop()

		ctx := t.Context()
		stream, err := client.Many(ctx, echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        8,
		}.Build())
		x.NoError(t, err)

		seqs := []uint32{}
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err)
			seqs = append(seqs, res.GetSequence())
		}
		x.Equal(t, []uint32{1, 3, 5, 7}, seqs)
	})
	t.Run("reordered upload stays an ordered subsequence", func(t *testing.T) {
		// Client data frames are reordered on the way up; the handler must
		// still observe an ordered subsequence and the call must complete.
		reorder := func(next drpc.FrameHandler) drpc.FrameHandler {
			return lossy.New(next, lossy.Options{Seed: 1, Hold: 1, Filter: dataOnly})
		}
		client, stop := PipeOption{C2S: reorder}.Use(t)
		defer stop()

		ctx := t.Context()
		up, err := client.Buff(ctx)
		x.NoError(t, err)
		for range 8 {
			err := up.Send(echo.EchoRequest_builder{
				Message:       "abc",
				Repeat:        1,
				CircularShift: 1,
			}.Build())
			x.NoError(t, err)
		}
		batch, err := up.CloseAndRecv()
		x.NoError(t, err)
		// Deterministic with Hold=1: of 8 uploads, every second one survives.
		x.Equal(t, 4, len(batch.GetItems()))
	})
}
