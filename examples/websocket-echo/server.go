package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/examples/websocket-echo/echopb"
	"github.com/lesomnus/grpc-dgram/transport/gorilla"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// echoService is an ordinary gRPC service implementation.
type echoService struct {
	echopb.UnimplementedEchoServiceServer
}

func (echoService) Echo(_ context.Context, req *echopb.EchoRequest) (*echopb.EchoResponse, error) {
	return &echopb.EchoResponse{Message: req.GetMessage()}, nil
}

func (echoService) Count(req *echopb.CountRequest, stream grpc.ServerStreamingServer[echopb.EchoResponse]) error {
	ctx := stream.Context()
	interval := time.Duration(req.GetIntervalMs()) * time.Millisecond
	for i := uint32(1); i <= req.GetCount(); i++ {
		if interval > 0 {
			select {
			case <-ctx.Done():
				// A graceful stop does not reach here: it waits for this
				// handler. Cancellation means the client left or the socket
				// died (§4.5) — the adapter's teardown cancelled us.
				return status.FromContextError(ctx.Err()).Err()
			case <-time.After(interval):
			}
		}
		err := stream.Send(&echopb.EchoResponse{
			Message:  fmt.Sprintf("tick %d", i),
			Sequence: i,
		})
		if err != nil {
			return err
		}
	}
	// Returning nil sends the terminal frame; the client's Recv reports
	// io.EOF. End-of-stream is always a frame, never inferred from silence.
	return nil
}

type server struct {
	url string

	closer sync.Once
	shut   func()
}

func startServer(ctx context.Context, addr string) (*server, error) {
	// One Gateway serves every WebSocket, one drpc peer each. It advertises
	// Reliable() == true, so NewServer turns the timer machinery off and
	// requires the exact sequence on the wire (PROTOCOL.md §10.6).
	gw := gorilla.NewGateway()
	srv := drpc.NewServer(gw)
	echopb.RegisterEchoServiceServer(srv, echoService{})

	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade already wrote the error response
		}
		// Blocks until the socket dies, then deregisters the peer and calls
		// srv.DisconnectPeer. With protocol timers off, that teardown is the
		// only thing that can unblock this peer's live calls (§4.5).
		_ = gw.ServePeer(r.Context(), srv, c)
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	hs := &http.Server{Handler: mux}
	go func() { _ = hs.Serve(ln) }()

	return &server{
		url: fmt.Sprintf("ws://%s/rpc", ln.Addr()),
		shut: func() {
			// Order matters: drain the RPC layer first (GracefulStop waits
			// for live handlers), then stop accepting HTTP. Upgraded
			// connections are hijacked, so http.Server.Shutdown neither
			// waits for them nor closes them — the drpc gateway owns them
			// from the Upgrade on, and they die with the process or when
			// their peer goes away.
			srv.GracefulStop()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = hs.Shutdown(sctx)
		},
	}, nil
}

// gracefulStop drains in-flight calls and stops serving; safe to call more
// than once.
func (s *server) gracefulStop() { s.closer.Do(s.shut) }
