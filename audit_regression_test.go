package drpc_test

// audit_regression_test.go pins the two protocol holes closed by the
// 2026-07-11 audit round:
//   - reliable-mode rx overflow blocks instead of silently dropping
//     (PROTOCOL.md §4.2, §14 exact sequence);
//   - server frames echo the client incarnation (peer_epoch, §6.1), so a
//     restarted client that re-allocates a colliding sid never receives the
//     dead incarnation's stream, and its RESETs kill exactly the old call
//     (§9.3).

import (
	"io"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
)

// §4.2 / §14: on a reliable channel a consumer that falls far behind the
// producer must stall the wire (transport flow control), never lose messages.
// The 2-frame buffer would have silently dropped 98 of the 100 frames before
// the fix — and the strict window advanced on accept, so no INTERNAL ever
// fired: the loss was a clean EOF.
func TestReliableRxOverflowBlocks(t *testing.T) {
	bubble(t, func(t *testing.T) {
		pipe := PipeOption{
			ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true), drpc.WithRxBuffer(2, drpc.DropNewest)},
			ServerOpts: []drpc.ServerOption{drpc.WithReliable(true)},
		}
		client, stop := pipe.Use(t)
		defer stop()

		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message: "m",
			Repeat:  100,
		}.Build())
		x.NoError(t, err)

		// Let the wire push as far as it can before the app reads anything:
		// the s2c pump is now blocked on the full buffer, holding frame 3.
		synctest.Wait()

		n := uint32(0)
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err)
			x.Equal(t, n, res.GetSequence()) // exact sequence: no gap, no dup
			n++
		}
		x.Equal(t, 100, n)
	})
}

// The server-side twin: a client-streaming burst through a 2-frame server
// buffer must reach the handler complete and in order. The handler is gated
// shut until the whole burst is on the wire, so the pump provably has to
// block — the old code, with nothing draining, deterministically dropped 98
// of the 100 frames here.
func TestReliableRxOverflowBlocksServer(t *testing.T) {
	bubble(t, func(t *testing.T) {
		gate := make(chan struct{})
		pipe := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithReliable(true)},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(true), drpc.WithRxBuffer(2, drpc.DropNewest),
				drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
					<-gate
					return handler(srv, ss)
				}),
			},
		}
		client, stop := pipe.Use(t)
		defer stop()

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		for range 100 {
			x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "m", Repeat: 1}.Build()))
		}
		x.NoError(t, stream.CloseSend())
		// Let the wire push as far as it can with the handler not consuming:
		// the c2s pump is now blocked on the full 2-frame buffer.
		synctest.Wait()
		close(gate)

		res, err := stream.CloseAndRecv()
		x.NoError(t, err)
		x.Equal(t, 100, len(res.GetItems()))
		for i, item := range res.GetItems() {
			x.Equal(t, uint32(i), item.GetSequence())
		}
	})
}

// §6.1 / §6.5 / §9.3: a client restart re-allocates sids from 1 while the old
// incarnation's call is still live at the server — and still pushing. The
// peer_epoch echo keeps the old stream out of the new call that reuses its
// sid, and the new client's RESETs (re-echoing that peer_epoch) reclaim
// exactly the old call, well before the T_live backstop — without touching
// the innocent new call that shares the sid.
func TestChar_ClientRestartSidCollision(t *testing.T) {
	bubble(t, func(t *testing.T) {
		p := newRestartPipe(t)
		defer p.stop()

		c1, dead1 := p.newConn()
		s1, err := c1.Live(p.ctx)
		x.NoError(t, err)
		x.NoError(t, s1.Send(echo.EchoRequest_builder{Message: "old", Repeat: 1}.Build()))
		_, err = s1.Recv()
		x.NoError(t, err) // the old incarnation's call (sid 1) is live

		// The restart happens first — the delivery target swaps to the new
		// incarnation — and only then does the old handler push five frames,
		// so every one of them lands on the new client, deterministically.
		c2, _ := p.newConn() // the client restarts at the same address...
		x.NoError(t, s1.Send(echo.EchoRequest_builder{Message: "old", Repeat: 5}.Build()))
		dead1.Store(true) // ...and the old incarnation's tx goes dark

		// The new incarnation's first call re-allocates sid 1, colliding with
		// the old call. Every response it sees must be its own: the old
		// stream's frames name the old incarnation and are refused at the
		// door. Before the peer_epoch echo, the first old frame (a mid-stream
		// seq) was ACCEPTED by the fresh window and the genuine seq 1,2,...
		// were then dedup-dropped — this Recv loop hung.
		s2, err := c2.Live(p.ctx)
		x.NoError(t, err)
		x.NoError(t, s2.Send(echo.EchoRequest_builder{Message: "new", Repeat: 3}.Build()))
		for range 3 {
			res, err := s2.Recv()
			x.NoError(t, err)
			x.Equal(t, "new", res.GetMessage()) // never the old stream's "old"
		}
		x.NoError(t, s2.CloseSend())
		_, err = s2.Recv()
		x.Equal(t, io.EOF, err) // the RESET storm did not kill the new call

		// The old handler died from the targeted RESETs the new incarnation
		// answered — not from the T_live backstop.
		synctest.Wait()
		done := make(chan struct{})
		go func() {
			p.srv.Load().GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(fastTiming.Liveness / 2):
			t.Fatal("old call not reclaimed by the RESET fast path (would need the T_live backstop)")
		}
	})
}
