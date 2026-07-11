package drpc_test

import (
	"testing"
	"time"

	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestShutdown exercises Server.Stop / GracefulStop / Conn.Close while a call
// is in flight — the cases the old implementation could wedge on.
func TestShutdown(t *testing.T) {
	t.Run("Stop cancels an in-flight handler and fails the client", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			client, stop := unreliablePipe(nil, nil).Use(t)
			defer stop()

			// A blocking handler: Once with OverVoid blocks on ctx.Done.
			ctxHit := make(chan struct{})
			client.service.SetHit(func() { close(ctxHit) })

			errc := make(chan error, 1)
			go func() {
				_, err := client.Once(t.Context(), echo.Void())
				errc <- err
			}()
			<-ctxHit // the handler is running and blocked

			client.server.Stop() // must cancel the handler ctx

			select {
			case err := <-errc:
				// The client sees a failure status (server abort or its own
				// abort as the response is refused) — never a hang.
				if status.Code(err) == codes.OK {
					t.Fatalf("call must fail, got %v", err)
				}
			case <-time.After(2 * fastTiming.Liveness):
				t.Fatal("Stop did not release the in-flight call")
			}
		})
	})
	t.Run("Stop is idempotent and refuses new calls", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			client, stop := unreliablePipe(nil, nil).Use(t)
			defer stop()

			_, err := client.Once(t.Context(), &echo.EchoRequest{})
			x.NoError(t, err)

			client.server.Stop()
			client.server.Stop() // idempotent: no panic

			// A call after Stop is refused, not hung.
			start := time.Now()
			_, err = client.Once(t.Context(), &echo.EchoRequest{})
			x.True(t, err != nil, "post-Stop call must fail")
			x.True(t, time.Since(start) < 3*fastTiming.Call, "post-Stop call must fail fast, not hang")
		})
	})
	t.Run("GracefulStop waits for a completing handler", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			client, stop := unreliablePipe(nil, nil).Use(t)
			defer stop()

			res, err := client.Once(t.Context(), echo.EchoRequest_builder{
				Message:       "abc",
				CircularShift: 1,
			}.Build())
			x.NoError(t, err)
			x.Equal(t, "bca", res.GetMessage())

			done := make(chan struct{})
			go func() { client.server.GracefulStop(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * fastTiming.Liveness):
				t.Fatal("GracefulStop wedged with no in-flight calls")
			}
		})
	})
	t.Run("Conn.Close fails an in-flight call", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			client, stop := unreliablePipe(nil, nil).Use(t)
			defer stop()

			ctxHit := make(chan struct{})
			client.service.SetHit(func() { close(ctxHit) })

			errc := make(chan error, 1)
			go func() {
				_, err := client.Once(t.Context(), echo.Void())
				errc <- err
			}()
			<-ctxHit

			client.conn.Close(nil) // adapter-teardown path (PROTOCOL.md §4.5)

			select {
			case err := <-errc:
				x.Equal(t, codes.Unavailable, status.Code(err))
			case <-time.After(2 * fastTiming.Liveness):
				t.Fatal("Conn.Close did not release the in-flight call")
			}
		})
	})
}
