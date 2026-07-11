package drpc_test

import (
	"io"
	"testing"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
)

// TestRxBufferOptions checks that the Q3 buffer knobs are accepted per method,
// per Conn, and globally, and that streams still work under a tight buffer and
// each drop policy.
func TestRxBufferOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy drpc.DropPolicy
	}{
		{"DropNewest", drpc.DropNewest},
		{"DropOldest", drpc.DropOldest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				client, stop := PipeOption{
					ServerOpts: []drpc.ServerOption{
						drpc.WithRxBuffer(2, tc.policy),
						drpc.WithMethodRxBuffer(echo.EchoService_Live_FullMethodName, 4, tc.policy),
					},
					ConnOpts: []drpc.ConnOption{drpc.WithRxBuffer(2, tc.policy)},
				}.Use(t)
				defer stop()

				ctx := t.Context()

				// Server-streaming into a tight (size-2) client buffer: when the
				// producer outruns the reader the buffer sheds frames per the
				// policy, so the app sees an ordered SUBSEQUENCE (§14) — the
				// sensor "freshest-wins" contract — ending in a clean io.EOF,
				// never an error.
				stream, err := client.Many(ctx, echo.EchoRequest_builder{
					Message:       "abc",
					CircularShift: 1,
					Repeat:        5,
				}.Build())
				x.NoError(t, err)
				var seqs []uint32
				for {
					res, err := stream.Recv()
					if err == io.EOF {
						break
					}
					x.NoError(t, err)
					seqs = append(seqs, res.GetSequence())
				}
				x.True(t, len(seqs) >= 1 && len(seqs) <= 5, "bounded subsequence")
				for i := 1; i < len(seqs); i++ {
					x.True(t, seqs[i] > seqs[i-1], "must stay ordered")
				}

				// Bidi read/write interleaved (per-method buffer of 4).
				bidi, err := client.Live(ctx)
				x.NoError(t, err)
				for range 3 {
					err := bidi.Send(echo.EchoRequest_builder{Message: "x", Repeat: 1}.Build())
					x.NoError(t, err)
					_, err = bidi.Recv()
					x.NoError(t, err)
				}
				x.NoError(t, bidi.CloseSend())
				for {
					_, err := bidi.Recv()
					if err == io.EOF {
						break
					}
					x.NoError(t, err)
				}
			})
		})
	}
}

// TestLimitsSmoke checks WithLimits/WithRxBuffer are accepted on both roles and
// the happy path still works under tight caps.
func TestLimitsSmoke(t *testing.T) {
	bubble(t, func(t *testing.T) {
		client, stop := PipeOption{
			ServerOpts: []drpc.ServerOption{
				drpc.WithRxBuffer(4, drpc.DropNewest),
				drpc.WithLimits(drpc.Limits{
					MaxTombstones:     8,
					MaxTombstoneBytes: 256,
					MaxDeadPeers:      2,
					MaxPendingResets:  16,
				}),
			},
			ConnOpts: []drpc.ConnOption{
				drpc.WithRxBuffer(4, drpc.DropOldest),
				drpc.WithLimits(drpc.Limits{MaxPendingResets: 16}),
			},
		}.Use(t)
		defer stop()

		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())
	})
}
