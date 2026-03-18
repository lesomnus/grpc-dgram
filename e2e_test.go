package drpc_test

import (
	"context"
	"testing"

	drpc "github.com/lesomnu/grpc-dgram"
	"github.com/lesomnu/grpc-dgram/internal/echo"
	"github.com/lesomnu/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestE2E(t *testing.T) {
	pipe := func(ctx context.Context) (*drpc.Server, *drpc.Conn) {
		var h drpc.FrameHandler
		server := drpc.NewServer(func(_ context.Context, f *drpc.Frame) error {
			go h(ctx, f)
			return nil
		})
		conn := drpc.NewConn(func(_ context.Context, f *drpc.Frame) error {
			go server.Handle(ctx, f)
			return nil
		})
		h = conn.Handle

		return server, conn
	}

	t.Run("Unary", func(t *testing.T) {
		ctx := t.Context()

		server, conn := pipe(ctx)
		echo.RegisterEchoServiceServer(server, echo.EchoServer{})

		client := echo.NewEchoServiceClient(conn)
		res, err := client.Once(ctx, echo.EchoRequest_builder{
			Message:       "Royale with Cheese",
			CircularShift: 3,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "ale with CheeseRoy", res.GetMessage())
	})
	t.Run("Unary unknown service", func(t *testing.T) {
		ctx := t.Context()

		_, conn := pipe(ctx)

		client := echo.NewEchoServiceClient(conn)
		_, err := client.Once(ctx, &echo.EchoRequest{})
		x.Error(t, err)

		code := status.Code(err)
		x.Equal(t, codes.Unimplemented, code)
	})
	t.Run("Stream", func(t *testing.T) {
		ctx := t.Context()

		server, conn := pipe(ctx)
		echo.RegisterEchoServiceServer(server, echo.EchoServer{})

		client := echo.NewEchoServiceClient(conn)
		stream, err := client.Live(ctx)
		x.NoError(t, err)

		err = stream.Send(echo.EchoRequest_builder{
			Message:       "bar",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)

		res, err := stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "arb", res.GetMessage())
		x.Equal(t, 0, res.GetSequence())

		err = stream.Send(echo.EchoRequest_builder{
			Message:       "bar",
			CircularShift: 2,
		}.Build())
		x.NoError(t, err)

		res, err = stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "rba", res.GetMessage())
		x.Equal(t, 1, res.GetSequence())
	})
}
