package drpc_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	drpc "github.com/lesomnu/grpc-dgram"
	"github.com/lesomnu/grpc-dgram/internal/echo"
	"github.com/lesomnu/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

type PipeOption struct {
	ServerOpts []drpc.ServerOption
	ConnOpts   []drpc.ConnOption
}

func (o PipeOption) Build(t *testing.T) (*drpc.Server, *drpc.Conn, func()) {
	ctx, cancel := context.WithCancel(t.Context())

	const PrintBody = false

	ca := make(chan *drpc.Frame, 10)
	server := drpc.NewServer(drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		t.Logf("server->client %d:%d", f.GetSid(), f.GetSeq())
		if PrintBody {
			fmt.Printf("%v\n", protojson.Format(f))
		}
		ca <- f
		return nil
	}), o.ServerOpts...)

	cb := make(chan *drpc.Frame, 10)
	conn := drpc.NewConn(drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		t.Logf("client->server %d:%d", f.GetSid(), f.GetSeq())
		if PrintBody {
			fmt.Printf("%v\n", protojson.Format(f))
		}
		cb <- f
		return nil
	}), o.ConnOpts...)

	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-ca:
				if err := conn.Handle(ctx, f); err != nil {
					if err != ctx.Err() {
						panic(err)
					}
				}
			}
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-cb:
				if err := server.Handle(ctx, f); err != nil {
					if err != ctx.Err() {
						panic(err)
					}
				}
			}
		}
	})

	return server, conn, func() {
		server.GracefulStop()
		cancel()
		wg.Wait()
	}
}

func (o PipeOption) Use(t *testing.T) (*Client, func()) {
	server, conn, cancel := o.Build(t)

	s := &echo.EchoServer{}
	c := &Client{echo.NewEchoServiceClient(conn), s}
	echo.RegisterEchoServiceServer(server, s)

	return c, cancel
}

type Client struct {
	echo.EchoServiceClient
	server *echo.EchoServer
}

func TestE2E(t *testing.T) {
	pipe := PipeOption{}.Use

	t.Run("call", func(t *testing.T) {
		t.Run("Unary", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			res, err := client.Once(ctx, echo.EchoRequest_builder{
				Message:       "Royale with Cheese",
				CircularShift: 3,
			}.Build())
			x.NoError(t, err)
			x.Equal(t, "ale with CheeseRoy", res.GetMessage())
		})
		t.Run("Server Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

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

			client, stop := pipe(t)
			defer stop()

			stream, err := client.Buff(ctx)
			x.NoError(t, err)
			defer stream.CloseSend()

			err = stream.Send(echo.EchoRequest_builder{
				Message:       "bar",
				Repeat:        1,
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

			client, stop := pipe(t)
			defer stop()

			stream, err := client.Live(ctx)
			x.NoError(t, err)
			defer stream.CloseSend()

			err = stream.Send(echo.EchoRequest_builder{
				Message:       "bar",
				Repeat:        1,
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
	})
	t.Run("server responded not ok", func(t *testing.T) {
		st := status.New(codes.OutOfRange, "foo")

		t.Run("Unary", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			client.server.Err = st.Err()
			_, err := client.Once(ctx, &echo.EchoRequest{})
			x.Error(t, err)

			st_, ok := status.FromError(err)
			x.True(t, ok)
			x.Equal(t, st, st_)
		})
		t.Run("Server Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			client.server.Err = st.Err()
			stream, err := client.Many(ctx, &echo.EchoRequest{})
			x.NoError(t, err)

			_, err = stream.Recv()
			x.Error(t, err)

			st_, ok := status.FromError(err)
			x.True(t, ok)
			x.Equal(t, st, st_)
		})
		t.Run("Client Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			client.server.Err = st.Err()
			stream, err := client.Buff(ctx)
			x.NoError(t, err)

			err = stream.Send(&echo.EchoRequest{})
			x.NoError(t, err)

			// Wait for the client to receive the error from the server.
			time.Sleep(300 * time.Millisecond)

			err = stream.Send(&echo.EchoRequest{})
			x.ErrorIs(t, err, io.EOF)

			_, err = stream.CloseAndRecv()
			x.Error(t, err)

			st_, ok := status.FromError(err)
			x.True(t, ok)
			x.Equal(t, st, st_)
		})
		t.Run("Bidi Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			client.server.Err = st.Err()
			stream, err := client.Live(ctx)
			x.NoError(t, err)

			err = stream.Send(&echo.EchoRequest{})
			x.NoError(t, err)

			_, err = stream.Recv()
			x.Error(t, err)

			st_, ok := status.FromError(err)
			x.True(t, ok)
			x.Equal(t, st, st_)
		})
	})
	t.Run("unknown service", func(t *testing.T) {
		t.Run("Unary", func(t *testing.T) {
			ctx := t.Context()

			_, conn, stop := PipeOption{}.Build(t)
			defer stop()

			client := echo.NewEchoServiceClient(conn)
			_, err := client.Once(ctx, &echo.EchoRequest{})
			x.Error(t, err)

			code := status.Code(err)
			x.Equal(t, codes.Unimplemented, code)
		})
		t.Run("Server Streaming", func(t *testing.T) {
			ctx := t.Context()

			_, conn, stop := PipeOption{}.Build(t)
			defer stop()

			client := echo.NewEchoServiceClient(conn)
			stream, err := client.Many(ctx, &echo.EchoRequest{})
			x.NoError(t, err)

			_, err = stream.Recv()
			x.Error(t, err)

			code := status.Code(err)
			x.Equal(t, codes.Unimplemented, code)
		})
		t.Run("Client Streaming", func(t *testing.T) {
			ctx := t.Context()

			_, conn, stop := PipeOption{}.Build(t)
			defer stop()

			client := echo.NewEchoServiceClient(conn)
			stream, err := client.Buff(ctx)
			x.NoError(t, err)

			err = stream.Send(&echo.EchoRequest{})
			x.NoError(t, err)

			// Wait for the client to receive the error from the server.
			time.Sleep(300 * time.Millisecond)

			err = stream.Send(&echo.EchoRequest{})
			x.ErrorIs(t, err, io.EOF)

			_, err = stream.CloseAndRecv()
			x.Error(t, err)

			code := status.Code(err)
			x.Equal(t, codes.Unimplemented, code)
		})
		t.Run("Bidi Streaming", func(t *testing.T) {
			ctx := t.Context()

			_, conn, stop := PipeOption{}.Build(t)
			defer stop()

			client := echo.NewEchoServiceClient(conn)
			stream, err := client.Live(ctx)
			x.NoError(t, err)

			err = stream.Send(&echo.EchoRequest{})
			x.NoError(t, err)

			_, err = stream.Recv()
			x.Error(t, err)

			code := status.Code(err)
			x.Equal(t, codes.Unimplemented, code)
		})
	})
	t.Run("interceptor", func(t *testing.T) {
		t.Run("Unary interceptor", func(t *testing.T) {
			ctx := t.Context()

			msgs := []string{}
			client, stop := PipeOption{
				ServerOpts: []drpc.ServerOption{
					drpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
						msgs = append(msgs, fmt.Sprintf("[S] %s", req.(*echo.EchoRequest).GetMessage()))
						res, err := handler(ctx, req)
						if err != nil {
							return nil, err
						}

						msgs = append(msgs, fmt.Sprintf("[S] %s", res.(*echo.EchoResponse).GetMessage()))
						return res, nil
					}),
				},
				ConnOpts: []drpc.ConnOption{
					drpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
						msgs = append(msgs, fmt.Sprintf("[C] %s", req.(*echo.EchoRequest).GetMessage()))
						err := invoker(ctx, method, req, reply, cc, opts...)
						if err != nil {
							return err
						}

						msgs = append(msgs, fmt.Sprintf("[C] %s", reply.(*echo.EchoResponse).GetMessage()))
						return nil
					}),
				},
			}.Use(t)
			defer stop()

			_, err := client.Once(ctx, echo.EchoRequest_builder{
				Message:       "Royale with Cheese",
				CircularShift: 3,
			}.Build())
			x.NoError(t, err)

			_, err = client.Once(ctx, echo.EchoRequest_builder{
				Message:       "Le Big Mac",
				CircularShift: 3,
			}.Build())
			x.NoError(t, err)

			x.Equal(t, []string{
				"[C] Royale with Cheese",
				"[S] Royale with Cheese",
				"[S] ale with CheeseRoy",
				"[C] ale with CheeseRoy",
				"[C] Le Big Mac",
				"[S] Le Big Mac",
				"[S] Big MacLe ",
				"[C] Big MacLe ",
			}, msgs)
		})
		t.Run("Stream interceptor", func(t *testing.T) {
			ctx := t.Context()

			msgs := []string{}
			client, stop := PipeOption{
				ServerOpts: []drpc.ServerOption{
					drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
						msgs = append(msgs, fmt.Sprintf("[S:I] %s", info.FullMethod))
						err := handler(srv, ss)
						msgs = append(msgs, fmt.Sprintf("[S:O] %s", info.FullMethod))
						return err
					}),
				},
				ConnOpts: []drpc.ConnOption{
					drpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
						msgs = append(msgs, fmt.Sprintf("[C:I] %s", method))
						stream, err := streamer(ctx, desc, cc, method, opts...)
						if err != nil {
							return nil, err
						}

						msgs = append(msgs, fmt.Sprintf("[C:O] %s", method))
						return stream, nil
					}),
				},
			}.Use(t)
			defer stop()

			stream, err := client.Live(ctx)
			x.NoError(t, err)
			defer stream.CloseSend()

			err = stream.Send(echo.EchoRequest_builder{
				Message:       "bar",
				Repeat:        1,
				CircularShift: 1,
			}.Build())
			x.NoError(t, err)

			_, err = stream.Recv()
			x.NoError(t, err)

			err = stream.Send(echo.EchoRequest_builder{
				Message:       "bar",
				Repeat:        2,
				CircularShift: 1,
			}.Build())
			x.NoError(t, err)

			_, err = stream.Recv()
			x.NoError(t, err)

			_, err = stream.Recv()
			x.NoError(t, err)
			stream.CloseSend()
			stop()

			x.Equal(t, []string{
				"[C:I] /echo.EchoService/Live",
				"[C:O] /echo.EchoService/Live",
				"[S:I] /echo.EchoService/Live",
				"[S:O] /echo.EchoService/Live",
			}, msgs)
		})
	})
	t.Run("metadata", func(t *testing.T) {
		t.Run("Unary", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			md := metadata.Pairs("foo", "bar")
			ctx = metadata.NewOutgoingContext(ctx, md)

			header := metadata.MD{}
			trailer := metadata.MD{}

			_, err := client.Once(ctx, &echo.EchoRequest{},
				grpc.Header(&header),
				grpc.Trailer(&trailer),
			)
			x.NoError(t, err)
			x.Equal(t, md, client.server.MD)
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "header"), header)
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "trailer"), trailer)
		})
		t.Run("Server streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			md := metadata.Pairs("foo", "bar")
			ctx = metadata.NewOutgoingContext(ctx, md)

			stream, err := client.Many(ctx, &echo.EchoRequest{})
			x.NoError(t, err)

			_, err = stream.Recv()
			x.NoError(t, err)
			x.Equal(t, md, client.server.MD)

			header, err := stream.Header()
			x.NoError(t, err)
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "header"), header)

			trailer := stream.Trailer()
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "trailer"), trailer)
		})
		t.Run("Client streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			md := metadata.Pairs("foo", "bar")
			ctx = metadata.NewOutgoingContext(ctx, md)

			stream, err := client.Buff(ctx)
			x.NoError(t, err)

			_, err = stream.CloseAndRecv()
			x.NoError(t, err)
			x.Equal(t, md, client.server.MD)

			header, err := stream.Header()
			x.NoError(t, err)
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "header"), header)

			trailer := stream.Trailer()
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "trailer"), trailer)
		})
		t.Run("Bidi streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			md := metadata.Pairs("foo", "bar")
			ctx = metadata.NewOutgoingContext(ctx, md)

			stream, err := client.Live(ctx)
			x.NoError(t, err)

			err = stream.CloseSend()
			x.NoError(t, err)

			_, err = stream.Recv()
			x.NoError(t, err)
			x.Equal(t, md, client.server.MD)

			header, err := stream.Header()
			x.NoError(t, err)
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "header"), header)

			trailer := stream.Trailer()
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "trailer"), trailer)
		})
	})
}
