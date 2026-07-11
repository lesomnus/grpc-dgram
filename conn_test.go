package drpc_test

import (
	"context"
	"sync"
	"testing"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// terminalFor builds a server terminal frame answering the request frame,
// echoing the client incarnation as a conforming server does (PROTOCOL.md
// §6.1).
func terminalFor(req *drpc.Frame, epoch uint32, payload []byte) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(req.GetSid())
	f.SetSeq(1)
	f.SetFlags(drpc.FlagClose)
	f.SetCode(uint32(codes.OK))
	f.SetPayload(payload)
	f.SetPeerEpoch(req.GetEpoch())
	return f
}

func TestConn(t *testing.T) {
	t.Run("closed conn refuses new calls", func(t *testing.T) {
		// The closed latch: after Close the pump is gone and the sweeper is
		// stopped, so a call admitted now could never terminate — it must be
		// refused at the door instead (racing calls are caught by failAll).
		conn := drpc.NewConn(drpc.FrameHandlerFunc(func(context.Context, *drpc.Frame) error {
			return nil
		}))
		conn.Close(nil)

		err := conn.Invoke(t.Context(), echo.EchoService_Once_FullMethodName,
			&echo.EchoRequest{}, &echo.EchoResponse{})
		x.Equal(t, codes.Unavailable, status.Code(err))
	})
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
	t.Run("every OPEN carries the method string", func(t *testing.T) {
		// Methods are addressed by string, always (PROTOCOL.md §13): repeat
		// calls must not switch to any learned shorthand — a restarted,
		// differently-built server must never mis-dispatch an OPEN.
		ctx := t.Context()

		data, err := proto.Marshal(&echo.EchoResponse{})
		x.NoError(t, err)

		var frame_in *drpc.Frame

		var wg sync.WaitGroup
		defer wg.Wait()

		var conn *drpc.Conn
		conn = drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			frame_in = f
			res := terminalFor(f, 7, data)
			wg.Go(func() {
				err := conn.Handle(t.Context(), res)
				x.NoError(t, err)
			})
			return nil
		}))

		req := &echo.EchoRequest{}
		res := &echo.EchoResponse{}
		for range 2 {
			err = conn.Invoke(ctx, echo.EchoService_Once_FullMethodName, req, res)
			x.NoError(t, err)
			x.Equal(t, echo.EchoService_Once_FullMethodName, frame_in.GetMethod())
		}
	})
	t.Run("delayed response", func(t *testing.T) {
		ctx := t.Context()

		frames := make(chan *drpc.Frame, 8)
		// Reliable mode: no tombstones, so the late frame is answered with a
		// RESET at once (in unreliable mode it would be silently dropped by
		// the call's tombstone, PROTOCOL.md §9.2).
		conn := drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			frames <- f
			return nil
		}), drpc.WithReliable(true))

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
