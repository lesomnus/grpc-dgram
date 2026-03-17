package drpc_test

import (
	"context"
	"testing"

	drpc "github.com/lesomnu/grpc-dgram"
	"github.com/lesomnu/grpc-dgram/internal/echo"
	"github.com/lesomnu/grpc-dgram/internal/x"
)

func TestE2E(t *testing.T) {
	pipe := func() (*drpc.Server, *drpc.Conn) {
		var h drpc.FrameHandler
		server := drpc.NewServer(func(ctx context.Context, f *drpc.Frame) error {
			go h(ctx, f)
			return nil
		})
		conn := drpc.NewConn(func(ctx context.Context, f *drpc.Frame) error {
			go server.Handle(ctx, f)
			return nil
		})
		h = conn.Handle

		return server, conn
	}

	t.Run("Unary", func(t *testing.T) {
		server, conn := pipe()
		echo.RegisterEchoServiceServer(server, echo.EchoServer{})

		client := echo.NewEchoServiceClient(conn)
		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "Royale with Cheese",
			CircularShift: 3,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "eseRoyale with Che", res.GetMessage())
	})
}
