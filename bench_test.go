package drpc_test

import (
	"context"
	"sync/atomic"
	"testing"

	drpc "github.com/lesomnu/grpc-dgram"
	"github.com/lesomnu/grpc-dgram/internal/echo"
	"google.golang.org/protobuf/proto"
)

func BenchmarkServerHandle(b *testing.B) {
	data, _ := proto.Marshal(echo.EchoRequest_builder{Message: "foo"}.Build())

	new_server := func() *drpc.Server {
		server := drpc.NewServer(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
			// discard.
			return nil
		}))
		echo.RegisterEchoServiceServer(server, &echo.EchoServer{})
		return server
	}
	new_frame := func(sid uint32) *drpc.Frame {
		return drpc.Frame_builder{
			Sid:     sid,
			Method:  echo.EchoService_Noop_FullMethodName,
			Payload: data,
		}.Build()
	}

	b.Run("single producer", func(b *testing.B) {
		ctx := b.Context()
		server := new_server()
		sid := uint32(0)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sid++
			frame := new_frame(sid)
			server.Handle(ctx, frame)
		}
	})
	b.Run("multiple producers", func(b *testing.B) {
		ctx := b.Context()
		server := new_server()
		var sid atomic.Uint32

		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				frame := new_frame(sid.Add(1))
				server.Handle(ctx, frame)
			}
		})
	})
}
