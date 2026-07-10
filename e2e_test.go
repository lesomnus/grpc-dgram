package drpc_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type PipeOption struct {
	ServerOpts []drpc.ServerOption
	ConnOpts   []drpc.ConnOption

	// C2S and S2C decorate the frame path of each direction, e.g. with
	// x.NewLossy for fault injection. The decorated handler sits on the
	// wire side: frames it drops are never recorded nor delivered.
	C2S func(next drpc.FrameHandler) drpc.FrameHandler
	S2C func(next drpc.FrameHandler) drpc.FrameHandler
}

func (o PipeOption) Build(t *testing.T) (*Client, func()) {
	ctx, cancel := context.WithCancel(t.Context())

	const PrintBody = false
	var l x.Logger = t
	if os.Getenv("CI") != "" {
		l = x.NopLogger{}
	}

	// The wire carries one marshaled Envelop per message (PROTOCOL.md §4.1),
	// so frames round-trip through real serialization.
	ca := make(chan []byte, 256) // server -> client
	cb := make(chan []byte, 256) // client -> server

	wire := func(ch chan []byte) drpc.FrameHandler {
		return drpc.Wrap1(drpc.EnvelopHandlerFunc(func(_ context.Context, e *drpc.Envelop) error {
			data, err := proto.Marshal(e)
			if err != nil {
				return err
			}
			ch <- data
			return nil
		}))
	}

	server := drpc.NewServer(wire(ca), o.ServerOpts...)
	conn := drpc.NewConn(wire(cb), o.ConnOpts...)

	s := &echo.EchoServer{}
	c := &Client{
		EchoServiceClient: echo.NewEchoServiceClient(conn),

		conn:    conn,
		server:  server,
		service: s,
	}

	var s2c drpc.FrameHandler = drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		l.Logf("server->client %d:%d", f.GetSid(), f.GetSeq())
		if PrintBody {
			fmt.Printf("%v\n", protojson.Format(f))
		}
		c.recordRx(f)
		return conn.Handle(ctx, f)
	})
	if o.S2C != nil {
		s2c = o.S2C(s2c)
	}
	var c2s drpc.FrameHandler = drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		l.Logf("client->server %d:%d", f.GetSid(), f.GetSeq())
		if PrintBody {
			fmt.Printf("%v\n", protojson.Format(f))
		}
		c.recordTx(f)
		return server.Handle(ctx, f)
	})
	if o.C2S != nil {
		c2s = o.C2S(c2s)
	}

	pump := func(ch chan []byte, h drpc.FrameHandler) func() {
		return func() {
			for {
				select {
				case <-ctx.Done():
					return
				case data := <-ch:
					e := &drpc.Envelop{}
					if err := proto.Unmarshal(data, e); err != nil {
						panic(err)
					}
					if err := drpc.Unpack(ctx, e, h); err != nil && err != ctx.Err() {
						panic(err)
					}
				}
			}
		}
	}

	var wg sync.WaitGroup
	wg.Go(pump(ca, s2c))
	wg.Go(pump(cb, c2s))

	return c, func() {
		server.GracefulStop()
		cancel()
		wg.Wait()
	}
}

func (o PipeOption) Use(t *testing.T) (*Client, func()) {
	c, stop := o.Build(t)
	echo.RegisterEchoServiceServer(c.server, c.service)

	return c, stop
}

type Client struct {
	echo.EchoServiceClient

	conn    *drpc.Conn
	server  *drpc.Server
	service *echo.EchoServer

	mu sync.Mutex
	tx []*drpc.Frame
	rx []*drpc.Frame
}

func (c *Client) recordTx(f *drpc.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tx = append(c.tx, proto.CloneOf(f))
}

func (c *Client) recordRx(f *drpc.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rx = append(c.rx, proto.CloneOf(f))
}

// firstTxPayload returns the first recorded client->server frame that carries
// a payload (e.g. skips the eager OPEN of client-streaming calls).
func (c *Client) firstTxPayload(t *testing.T) *drpc.Frame {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.tx {
		if f.HasPayload() {
			return f
		}
	}
	t.Fatal("no client->server frame with payload")
	return nil
}

// firstRxPayload returns the first recorded server->client frame that carries
// a payload (e.g. skips creation-ack and header frames).
func (c *Client) firstRxPayload(t *testing.T) *drpc.Frame {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.rx {
		if f.HasPayload() {
			return f
		}
	}
	t.Fatal("no server->client frame with payload")
	return nil
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

			client.service.Err = st.Err()
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

			client.service.Err = st.Err()
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

			client.service.Err = st.Err()
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

			client.service.Err = st.Err()
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

			client, stop := PipeOption{}.Build(t)
			defer stop()

			_, err := client.Once(ctx, &echo.EchoRequest{})
			x.Error(t, err)

			code := status.Code(err)
			x.Equal(t, codes.Unimplemented, code)
		})
		t.Run("Server Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := PipeOption{}.Build(t)
			defer stop()

			stream, err := client.Many(ctx, &echo.EchoRequest{})
			x.NoError(t, err)

			_, err = stream.Recv()
			x.Error(t, err)

			code := status.Code(err)
			x.Equal(t, codes.Unimplemented, code)
		})
		t.Run("Client Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := PipeOption{}.Build(t)
			defer stop()

			stream, err := client.Buff(ctx)
			x.NoError(t, err)

			// The eager OPEN is rejected as soon as it arrives; a Send racing
			// the rejection may or may not observe it yet.
			if err := stream.Send(&echo.EchoRequest{}); err != nil {
				x.ErrorIs(t, err, io.EOF)
			}

			// Wait for the client to receive the rejection.
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

			client, stop := PipeOption{}.Build(t)
			defer stop()

			stream, err := client.Live(ctx)
			x.NoError(t, err)

			if err := stream.Send(&echo.EchoRequest{}); err != nil {
				x.ErrorIs(t, err, io.EOF)
			}

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
			x.Equal(t, md, client.service.MD)
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

			// Repeat is 0: the stream ends without a message.
			_, err = stream.Recv()
			x.ErrorIs(t, err, io.EOF)
			x.Equal(t, md, client.service.MD)

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
			x.Equal(t, md, client.service.MD)

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
			x.ErrorIs(t, err, io.EOF)
			x.Equal(t, md, client.service.MD)

			header, err := stream.Header()
			x.NoError(t, err)
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "header"), header)

			trailer := stream.Trailer()
			x.Equal(t, metadata.Pairs("foo", "bar", "timing", "trailer"), trailer)
		})
	})
	t.Run("codec", func(t *testing.T) {
		pipe := PipeOption{
			ConnOpts: []drpc.ConnOption{
				drpc.WithDefaultCallOptions(grpc.ForceCodecV2(&x.JsonCodecV2{})),
			},
		}.Use

		t.Run("Unary", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			_, err := client.Once(ctx, echo.EchoRequest_builder{
				Message:       "abc",
				CircularShift: 1,
			}.Build())
			x.NoError(t, err)

			req := &echo.EchoRequest{}
			err = protojson.Unmarshal(client.firstTxPayload(t).GetPayload(), req)
			x.NoError(t, err)
			x.Equal(t, "abc", req.GetMessage())

			res := &echo.EchoResponse{}
			err = protojson.Unmarshal(client.firstRxPayload(t).GetPayload(), res)
			x.NoError(t, err)
			x.Equal(t, "bca", res.GetMessage())
		})
		t.Run("Server Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			stream, err := client.Many(ctx, echo.EchoRequest_builder{
				Message:       "abc",
				CircularShift: 1,
				Repeat:        1,
			}.Build())
			x.NoError(t, err)

			_, err = stream.Recv()
			x.NoError(t, err)

			req := &echo.EchoRequest{}
			err = protojson.Unmarshal(client.firstTxPayload(t).GetPayload(), req)
			x.NoError(t, err)
			x.Equal(t, "abc", req.GetMessage())

			res := &echo.EchoResponse{}
			err = protojson.Unmarshal(client.firstRxPayload(t).GetPayload(), res)
			x.NoError(t, err)
			x.Equal(t, "bca", res.GetMessage())
		})
		t.Run("Client Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			stream, err := client.Buff(ctx)
			x.NoError(t, err)
			defer stream.CloseSend()

			err = stream.Send(echo.EchoRequest_builder{
				Message:       "abc",
				Repeat:        1,
				CircularShift: 1,
			}.Build())
			x.NoError(t, err)

			_, err = stream.CloseAndRecv()
			x.NoError(t, err)

			req := &echo.EchoRequest{}
			err = protojson.Unmarshal(client.firstTxPayload(t).GetPayload(), req)
			x.NoError(t, err)
			x.Equal(t, "abc", req.GetMessage())

			res := &echo.EchoBatchResponse{}
			err = protojson.Unmarshal(client.firstRxPayload(t).GetPayload(), res)
			x.NoError(t, err)

			items := res.GetItems()
			x.NotEmpty(t, items)
			x.Equal(t, "bca", items[0].GetMessage())
		})
		t.Run("Bidi Streaming", func(t *testing.T) {
			ctx := t.Context()

			client, stop := pipe(t)
			defer stop()

			stream, err := client.Live(ctx)
			x.NoError(t, err)
			defer stream.CloseSend()

			err = stream.Send(echo.EchoRequest_builder{
				Message:       "abc",
				Repeat:        1,
				CircularShift: 1,
			}.Build())
			x.NoError(t, err)

			err = stream.CloseSend()
			x.NoError(t, err)

			_, err = stream.Recv()
			x.NoError(t, err)

			req := &echo.EchoRequest{}
			err = protojson.Unmarshal(client.firstTxPayload(t).GetPayload(), req)
			x.NoError(t, err)
			x.Equal(t, "abc", req.GetMessage())

			res := &echo.EchoResponse{}
			err = protojson.Unmarshal(client.firstRxPayload(t).GetPayload(), res)
			x.NoError(t, err)
			x.Equal(t, "bca", res.GetMessage())
		})
	})
	t.Run("cancel", func(t *testing.T) {
		pipe := func(t *testing.T) (*Client, context.Context, func()) {
			client, stop := pipe(t)

			ctx, cancel := context.WithCancel(t.Context())
			client.service.Hit = cancel

			return client, ctx, func() {
				cancel()
				stop()
			}
		}

		t.Run("Unary", func(t *testing.T) {
			client, ctx, stop := pipe(t)
			defer stop()

			_, err := client.Once(ctx, echo.Void())
			x.Equal(t, codes.Canceled, status.Code(err))
		})
		t.Run("Server Streaming", func(t *testing.T) {
			client, ctx, stop := pipe(t)
			defer stop()

			stream, err := client.Many(ctx, echo.Void())
			x.NoError(t, err)

			_, err = stream.Recv()
			x.Equal(t, codes.Canceled, status.Code(err))
		})
		t.Run("Client Streaming", func(t *testing.T) {
			client, ctx, stop := pipe(t)
			defer stop()

			stream, err := client.Buff(ctx)
			x.NoError(t, err)

			err = stream.Send(echo.Void())
			x.NoError(t, err)

			_, err = stream.CloseAndRecv()
			x.Equal(t, codes.Canceled, status.Code(err))
		})
		t.Run("Bidi Streaming", func(t *testing.T) {
			client, ctx, stop := pipe(t)
			defer stop()

			stream, err := client.Live(ctx)
			x.NoError(t, err)

			err = stream.Send(echo.Void())
			x.NoError(t, err)

			_, err = stream.Recv()
			x.Equal(t, codes.Canceled, status.Code(err))
		})
	})
	t.Run("concurrent", func(t *testing.T) {
		ctx := t.Context()

		client, stop := pipe(t)
		defer stop()

		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for i := range 8 {
			wg.Go(func() {
				msg := fmt.Sprintf("unary-%d", i)
				res, err := client.Once(ctx, echo.EchoRequest_builder{
					Message:       msg,
					CircularShift: 1,
				}.Build())
				if err != nil {
					errs <- fmt.Errorf("unary-%d: %w", i, err)
					return
				}
				if want := echo.CircularShift(msg, 1); res.GetMessage() != want {
					errs <- fmt.Errorf("unary-%d: got %q, want %q", i, res.GetMessage(), want)
				}
			})
		}
		for i := range 4 {
			wg.Go(func() {
				stream, err := client.Many(ctx, echo.EchoRequest_builder{
					Message:       fmt.Sprintf("ss-%d", i),
					CircularShift: 1,
					Repeat:        3,
				}.Build())
				if err != nil {
					errs <- fmt.Errorf("ss-%d: %w", i, err)
					return
				}
				n := 0
				for {
					_, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						errs <- fmt.Errorf("ss-%d: %w", i, err)
						return
					}
					n++
				}
				if n != 3 {
					errs <- fmt.Errorf("ss-%d: got %d messages, want 3", i, n)
				}
			})
		}
		for i := range 4 {
			wg.Go(func() {
				stream, err := client.Live(ctx)
				if err != nil {
					errs <- fmt.Errorf("bidi-%d: %w", i, err)
					return
				}
				for j := range 3 {
					err := stream.Send(echo.EchoRequest_builder{
						Message:       fmt.Sprintf("bidi-%d-%d", i, j),
						CircularShift: 1,
						Repeat:        1,
					}.Build())
					if err != nil {
						errs <- fmt.Errorf("bidi-%d send: %w", i, err)
						return
					}
					if _, err := stream.Recv(); err != nil {
						errs <- fmt.Errorf("bidi-%d recv: %w", i, err)
						return
					}
				}
				if err := stream.CloseSend(); err != nil {
					errs <- fmt.Errorf("bidi-%d close: %w", i, err)
					return
				}
				if _, err := stream.Recv(); err != io.EOF {
					errs <- fmt.Errorf("bidi-%d: want EOF, got %v", i, err)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			x.NoError(t, err)
		}
	})
}
