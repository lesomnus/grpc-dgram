package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/lesomnus/grpc-dgram/examples/browser-webrtc/echopb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// echoService is an ordinary gRPC service implementation. Nothing in it knows
// that its client is a browser tab on the other end of a DataChannel.
type echoService struct {
	echopb.UnimplementedEchoServiceServer
	name string
}

func newEchoService() *echoService {
	host, _ := os.Hostname()
	return &echoService{name: fmt.Sprintf("%s/pid-%d", host, os.Getpid())}
}

func (s *echoService) Echo(ctx context.Context, req *echopb.EchoRequest) (*echopb.EchoResponse, error) {
	if strings.TrimSpace(req.GetMessage()) == "" {
		// A plain gRPC status: it travels as the call's terminal frame and
		// surfaces in the page as a StatusError with code 3.
		return nil, status.Error(codes.InvalidArgument, "message must not be empty")
	}
	// The pion adapter names the DataChannel as the peer, so the standard
	// peer.FromContext works here as it would over HTTP/2.
	from := "unknown"
	if p, ok := peer.FromContext(ctx); ok {
		from = p.Addr.String()
	}
	log.Printf("Echo from %s: %q", from, req.GetMessage())

	return &echopb.EchoResponse{
		Message:  strings.ToUpper(req.GetMessage()),
		ServedBy: s.name,
	}, nil
}
