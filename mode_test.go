package drpc_test

// mode_test.go pins per-peer reliability (PROTOCOL.md §4.3): one Server
// serving a reliable channel and an unreliable channel at once, each peer
// running in its channel's mode. The reliable peer's idle stream surviving
// past T_live is the regression this feature fixes — a single-mode server
// forced to unreliable would kill it (reliable clients send no keepalive).

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/protobuf/proto"
)

// mixedPipe wires one Server to two clients over in-memory channels: peer
// "R" annotated reliable (its Conn in reliable mode), peer "U" unannotated
// (unreliable, the server default). Server tx routes by the peer key —
// the first in-core exercise of multi-peer routing.
type mixedPipe struct {
	srv      *drpc.Server
	relConn  *drpc.Conn
	unrlConn *drpc.Conn
	unrlDead atomic.Bool // the unreliable client's process dies
}

func newMixedPipe(t *testing.T) *mixedPipe {
	p := &mixedPipe{}

	send := func(conn **drpc.Conn) drpc.FrameHandler {
		return drpc.Wrap1(drpc.EnvelopHandlerFunc(func(ctx context.Context, e *drpc.Envelop) error {
			data, err := proto.Marshal(e)
			if err != nil {
				return err
			}
			de := &drpc.Envelop{}
			if err := proto.Unmarshal(data, de); err != nil {
				return err
			}
			for _, f := range de.GetFrames() {
				(*conn).Handle(ctx, f)
			}
			return nil
		}))
	}
	p.srv = drpc.NewServer(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		key, _ := drpc.PeerFromContext(ctx)
		switch key {
		case "R":
			return send(&p.relConn).Handle(ctx, f)
		case "U":
			return send(&p.unrlConn).Handle(ctx, f)
		}
		return nil
	}), drpc.WithTiming(fastTiming)) // server default: unreliable
	echo.RegisterEchoServiceServer(p.srv, &echo.EchoServer{})

	// Peer R: a reliable channel — annotated per frame, client timers off.
	p.relConn = drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		ctx = drpc.NewPeerContext(ctx, "R")
		ctx = drpc.NewReliableContext(ctx, true)
		return p.srv.Handle(ctx, f)
	}), drpc.WithReliable(true))

	// Peer U: an unreliable channel — no annotation, server default applies.
	p.unrlConn = drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		if p.unrlDead.Load() {
			return nil
		}
		return p.srv.Handle(drpc.NewPeerContext(ctx, "U"), f)
	}), drpc.WithReliable(false), drpc.WithTiming(fastTiming))

	t.Cleanup(func() {
		p.srv.Stop()
		p.relConn.Close(nil)
		p.unrlConn.Close(nil)
	})
	return p
}

func TestPerPeerReliability(t *testing.T) {
	bubble(t, func(t *testing.T) {
		p := newMixedPipe(t)
		relClient := echo.NewEchoServiceClient(p.relConn)
		unrlClient := echo.NewEchoServiceClient(p.unrlConn)

		// Both peers work against the one server.
		relStream, err := relClient.Live(t.Context())
		x.NoError(t, err)
		x.NoError(t, relStream.Send(echo.EchoRequest_builder{Message: "r", Repeat: 1}.Build()))
		_, err = relStream.Recv()
		x.NoError(t, err)

		unrlStream, err := unrlClient.Live(t.Context())
		x.NoError(t, err)
		x.NoError(t, unrlStream.Send(echo.EchoRequest_builder{Message: "u", Repeat: 1}.Build()))
		_, err = unrlStream.Recv()
		x.NoError(t, err)

		// The unreliable client vanishes: ITS handler is reclaimed by
		// T_live...
		p.unrlDead.Store(true)

		// ...while the reliable peer's idle stream — no keepalive, no
		// traffic, way past T_live — must NOT be touched by that same
		// liveness machinery. This is the regression a single-mode server
		// cannot pass.
		time.Sleep(4 * fastTiming.Liveness)

		x.NoError(t, relStream.Send(echo.EchoRequest_builder{Message: "still", Repeat: 1}.Build()))
		res, err := relStream.Recv()
		x.NoError(t, err)
		x.Equal(t, "still", res.GetMessage())

		// Clean end of the reliable stream.
		x.NoError(t, relStream.CloseSend())
		if _, err = relStream.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("want io.EOF, got %v", err)
		}

		// The vanished unreliable peer cannot wedge shutdown: its handler
		// died with liveness, so GracefulStop returns.
		done := make(chan struct{})
		go func() {
			p.srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(4 * fastTiming.Liveness):
			t.Fatal("GracefulStop wedged: the vanished unreliable peer's handler leaked")
		}
	})
}
