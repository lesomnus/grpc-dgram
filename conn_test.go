package drpc_test

import (
	"context"
	"sync"
	"testing"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// terminalFor builds a server terminal frame answering the request frame.
func terminalFor(req *drpc.Frame, epoch uint32, payload []byte) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(req.GetSid())
	f.SetSeq(1)
	f.SetFlags(drpc.FlagClose)
	f.SetCode(uint32(codes.OK))
	f.SetPayload(payload)
	return f
}

func TestConn(t *testing.T) {
	t.Run("Invoke", func(t *testing.T) {
		ctx := t.Context()
		msg := "Royale with Cheese"

		data, err := proto.Marshal(echo.EchoResponse_builder{Message: msg}.Build())
		x.NoError(t, err)

		var wg sync.WaitGroup
		defer wg.Wait()

		var conn *drpc.Conn
		conn = drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			res := terminalFor(f, 7, data)
			wg.Go(func() {
				err := conn.Handle(t.Context(), res)
				x.NoError(t, err)
			})
			return nil
		}))

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

		var wg sync.WaitGroup
		defer wg.Wait()

		var conn *drpc.Conn
		conn = drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			frame_in = f
			// The server teaches its method index on every frame it sends
			// (PROTOCOL.md §13); indices are valid per server epoch.
			res := terminalFor(f, 7, data)
			res.SetMethodIndex(42)
			wg.Go(func() {
				err := conn.Handle(t.Context(), res)
				x.NoError(t, err)
			})
			return nil
		}))

		req := &echo.EchoRequest{}
		res := &echo.EchoResponse{}
		err = conn.Invoke(ctx, echo.EchoService_Once_FullMethodName, req, res)
		x.NoError(t, err)
		x.Equal(t, echo.EchoService_Once_FullMethodName, frame_in.GetMethod())
		x.Equal(t, 0, frame_in.GetMethodIndex())

		err = conn.Invoke(ctx, echo.EchoService_Once_FullMethodName, req, res)
		x.NoError(t, err)
		x.Equal(t, "", frame_in.GetMethod())
		x.Equal(t, 42, frame_in.GetMethodIndex())
	})
	t.Run("delayed response", func(t *testing.T) {
		ctx := t.Context()

		frames := make(chan *drpc.Frame, 8)
		conn := drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			frames <- f
			return nil
		}))

		ctx_, cancel := context.WithCancel(ctx)
		cancel()

		req := &echo.EchoRequest{}
		res := &echo.EchoResponse{}
		err := conn.Invoke(ctx_, echo.EchoService_Once_FullMethodName, req, res)
		x.Error(t, err)

		// Drain whatever the aborted call emitted (OPEN and/or abort CLOSE).
		for len(frames) > 0 {
			<-frames
		}

		// The call is gone from the stream map; a late response for its sid
		// (the first sid is always 1) is answered with a RESET echoing the
		// offender's epoch (PROTOCOL.md §9.3).
		late := &drpc.Frame{}
		late.SetEpoch(7)
		late.SetSid(1)
		late.SetSeq(1)
		late.SetFlags(drpc.FlagClose)
		late.SetCode(uint32(codes.OK))
		err = conn.Handle(ctx, late)
		x.NoError(t, err)

		reset := <-frames
		x.Equal(t, drpc.FlagReset, reset.GetFlags())
		x.Equal(t, 7, reset.GetEpoch())
		x.Equal(t, 1, reset.GetSid())
	})
}
