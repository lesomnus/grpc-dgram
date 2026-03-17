package drpc_test

import (
	"context"
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
}
