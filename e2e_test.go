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
	pipe := func(t *testing.T) (echo.EchoServiceClient, func()) {
		ctx, cancel := context.WithCancel(t.Context())

		ca := make(chan *drpc.Frame, 10)
		server := drpc.NewServer(func(_ context.Context, f *drpc.Frame) error {
			t.Logf("server->client %d:%d", f.GetSid(), f.GetSeq())
			ca <- f
			return nil
		})

		cb := make(chan *drpc.Frame, 10)
		conn := drpc.NewConn(func(_ context.Context, f *drpc.Frame) error {
			t.Logf("client->server %d:%d", f.GetSid(), f.GetSeq())
			cb <- f
			return nil
		})

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case f := <-ca:
					if err := conn.Handle(ctx, f); err != nil {
						panic(err)
					}
				case f := <-cb:
					if err := server.Handle(ctx, f); err != nil {
						panic(err)
					}
				}
			}
		}()

		echo.RegisterEchoServiceServer(server, echo.EchoServer{})
		return echo.NewEchoServiceClient(conn), cancel
	}

	t.Run("Unary", func(t *testing.T) {
		ctx := t.Context()

		client, cancel := pipe(t)
		defer cancel()

		res, err := client.Once(ctx, echo.EchoRequest_builder{
			Message:       "Royale with Cheese",
			CircularShift: 3,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "ale with CheeseRoy", res.GetMessage())
	})
	t.Run("Unary unknown service", func(t *testing.T) {
		ctx := t.Context()

		client, cancel := pipe(t)
		defer cancel()

		_, err := client.Once(ctx, &echo.EchoRequest{})
		x.Error(t, err)

		code := status.Code(err)
		x.Equal(t, codes.Unimplemented, code)
	})
	t.Run("Server Streaming", func(t *testing.T) {
		ctx := t.Context()

		client, cancel := pipe(t)
		defer cancel()

		stream, err := client.Many(ctx, echo.EchoRequest_builder{
			Message:       "bar",
			CircularShift: 1,
			Repeat:        2,
		}.Build())
		x.NoError(t, err)

		res, err := stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "arb", res.GetMessage())
		x.Equal(t, 0, res.GetSequence())

		res, err = stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "rba", res.GetMessage())
		x.Equal(t, 1, res.GetSequence())
	})
	t.Run("Client Streaming", func(t *testing.T) {
		ctx := t.Context()

		client, cancel := pipe(t)
		defer cancel()

		stream, err := client.Buff(ctx)
		x.NoError(t, err)

		err = stream.Send(echo.EchoRequest_builder{
			Message:       "bar",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)

		err = stream.Send(echo.EchoRequest_builder{
			Message:       "baz",
			Repeat:        2,
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)

		res, err := stream.CloseAndRecv()
		x.NoError(t, err)
		x.Equal(t, 3, len(res.GetItems()))

		item := res.GetItems()[0]
		x.Equal(t, "arb", item.GetMessage())
		x.Equal(t, 0, item.GetSequence())

		item = res.GetItems()[1]
		x.Equal(t, "azb", item.GetMessage())
		x.Equal(t, 1, item.GetSequence())

		item = res.GetItems()[2]
		x.Equal(t, "zba", item.GetMessage())
		x.Equal(t, 2, item.GetSequence())
	})
	t.Run("Bidi Streaming", func(t *testing.T) {
		ctx := t.Context()

		client, cancel := pipe(t)
		defer cancel()

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
			Repeat:        2,
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)

		res, err = stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "arb", res.GetMessage())
		x.Equal(t, 1, res.GetSequence())

		res, err = stream.Recv()
		x.NoError(t, err)
		x.Equal(t, "rba", res.GetMessage())
		x.Equal(t, 2, res.GetSequence())
	})
}
