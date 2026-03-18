package drpc_test

import (
	"context"
	"io"
	"sync"
	"testing"

	drpc "github.com/lesomnu/grpc-dgram"
	"github.com/lesomnu/grpc-dgram/internal/echo"
	"github.com/lesomnu/grpc-dgram/internal/x"
	"google.golang.org/protobuf/proto"
)

func TestConn(t *testing.T) {
	t.Run("Invoke", func(t *testing.T) {
		ctx := t.Context()
		msg := "Royale with Cheese"

		data, err := proto.Marshal(echo.EchoResponse_builder{Message: msg}.Build())
		x.NoError(t, err)

		frame_out := &drpc.Frame{}
		frame_out.SetPayload(data)

		var wg sync.WaitGroup
		defer wg.Wait()

		var conn *drpc.Conn
		conn = drpc.NewConn(func(ctx context.Context, f *drpc.Frame) error {
			wg.Go(func() {
				frame_out.SetSid(f.GetSid())
				frame_out.SetSeq(1)
				err := conn.Handle(t.Context(), frame_out)
				x.NoError(t, err)
			})
			return nil
		})

		req := &echo.EchoRequest{}
		res := &echo.EchoResponse{}
		err = conn.Invoke(ctx, echo.EchoService_Once_FullMethodName, req, res)
		x.NoError(t, err)
		x.Equal(t, msg, res.GetMessage())
	})
	t.Run("index learning", func(t *testing.T) {
		ctx := t.Context()

		data, err := proto.Marshal(&echo.EchoResponse{})
		x.NoError(t, err)

		var frame_in *drpc.Frame
		frame_out := &drpc.Frame{}
		frame_out.SetPayload(data)
		frame_out.SetMethodIndex(42)

		var wg sync.WaitGroup
		defer wg.Wait()

		var conn *drpc.Conn
		conn = drpc.NewConn(func(ctx context.Context, f *drpc.Frame) error {
			frame_in = f
			wg.Go(func() {
				frame_out.SetSid(f.GetSid())
				frame_out.SetSeq(1)
				err := conn.Handle(t.Context(), frame_out)
				x.NoError(t, err)
			})
			return nil
		})

		req := &echo.EchoRequest{}
		res := &echo.EchoResponse{}
		err = conn.Invoke(ctx, echo.EchoService_Once_FullMethodName, req, res)
		x.NoError(t, err)
		x.Equal(t, echo.EchoService_Once_FullMethodName, frame_in.GetMethod())

		err = conn.Invoke(ctx, echo.EchoService_Once_FullMethodName, req, res)
		x.NoError(t, err)
		x.Equal(t, "", frame_in.GetMethod())
		x.Equal(t, 42, frame_in.GetMethodIndex())
	})
	t.Run("delayed response", func(t *testing.T) {
		ctx := t.Context()

		var wg sync.WaitGroup
		defer wg.Wait()

		var frame_in *drpc.Frame
		frame_out := &drpc.Frame{}

		var conn *drpc.Conn
		conn = drpc.NewConn(func(ctx context.Context, f *drpc.Frame) error {
			frame_in = f
			return nil
		})

		ctx_, cancel := context.WithCancel(ctx)
		cancel()

		req := &echo.EchoRequest{}
		res := &echo.EchoResponse{}
		err := conn.Invoke(ctx_, echo.EchoService_Once_FullMethodName, req, res)
		x.Error(t, err)

		frame_out.SetSid(frame_in.GetSid())
		frame_out.SetSeq(1)
		err = conn.Handle(ctx, frame_out)
		x.ErrorIs(t, err, io.EOF)
	})
	// t.Run("Stream", func(t *testing.T) {
	// 	ctx := t.Context()
	// 	msg := "Royale with Cheese"

	// 	var wg sync.WaitGroup
	// 	defer wg.Wait()

	// 	var conn *drpc.Conn
	// 	conn = drpc.NewConn(func(ctx context.Context, f *drpc.Frame) error {
	// 		wg.Go(func() {
	// 			frame_out.SetSid(f.GetSid())
	// 			err := conn.Handle(t.Context(), frame_out)
	// 			x.NoError(t, err)
	// 		})
	// 		return nil
	// 	})

	// 	conn.NewStream()
	// })
}
