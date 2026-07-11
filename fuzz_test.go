package drpc_test

import (
	"context"
	"testing"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"google.golang.org/protobuf/proto"
)

// sink discards every frame; the fuzz targets only care that Handle never
// panics on adversarial input.
type sink struct{}

func (sink) Handle(context.Context, *drpc.Frame) error { return nil }

// FuzzServerHandle feeds arbitrary bytes as a Frame straight into a registered
// server. Handle must never panic — it is the entry point an adapter exposes
// to the (untrusted, on raw UDP) network.
func FuzzServerHandle(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		frame := &drpc.Frame{}
		if proto.Unmarshal(data, frame) != nil {
			return // only well-formed protobuf reaches Handle via an adapter
		}
		srv := drpc.NewServer(sink{})
		echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
		defer srv.Stop()
		// Deliver twice: exercise the tombstone / duplicate paths too.
		_ = srv.Handle(context.Background(), proto.CloneOf(frame))
		_ = srv.Handle(context.Background(), frame)
	})
}

// FuzzConnHandle feeds arbitrary bytes as a Frame into a client Conn's receive
// path (the frames a server, or a spoofer, could send back).
func FuzzConnHandle(f *testing.F) {
	seed(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		frame := &drpc.Frame{}
		if proto.Unmarshal(data, frame) != nil {
			return
		}
		conn := drpc.NewConn(sink{})
		_ = conn.Handle(context.Background(), proto.CloneOf(frame))
		_ = conn.Handle(context.Background(), frame)
	})
}

// seed adds a handful of structurally valid frames so the corpus starts from
// meaningful shapes rather than pure noise.
func seed(f *testing.F) {
	mk := func(build func(b *drpc.Frame)) []byte {
		b := &drpc.Frame{}
		build(b)
		data, _ := proto.Marshal(b)
		return data
	}
	f.Add(mk(func(b *drpc.Frame) { // unary OPEN|CLOSE
		b.SetEpoch(1)
		b.SetSid(1)
		b.SetSeq(1)
		b.SetFlags(drpc.FlagOpen | drpc.FlagClose)
		b.SetMethod(echo.EchoService_Once_FullMethodName)
	}))
	f.Add(mk(func(b *drpc.Frame) { // OPEN by out-of-range index
		b.SetEpoch(2)
		b.SetSid(7)
		b.SetSeq(1)
		b.SetFlags(drpc.FlagOpen)
		b.SetMethodIndex(1 << 20)
	}))
	f.Add(mk(func(b *drpc.Frame) { // stray RESET
		b.SetEpoch(3)
		b.SetSid(9)
		b.SetFlags(drpc.FlagReset)
	}))
	f.Add(mk(func(b *drpc.Frame) { // stream probe for unknown sid
		b.SetEpoch(4)
		b.SetSid(11)
		b.SetFlags(drpc.FlagPing)
	}))
	f.Add(mk(func(b *drpc.Frame) { // huge seq (poisoning attempt)
		b.SetEpoch(5)
		b.SetSid(13)
		b.SetSeq(1 << 30)
		b.SetPayload([]byte{})
	}))
}
