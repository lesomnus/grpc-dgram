package drpc

// White-box: DisconnectPeer is the teardown that reclaims per-peer state
// (PROTOCOL.md §9.4, §10.6). Gateways issue a fresh peer key per connection,
// so no later traffic can address it and no cap or sweep would ever collect
// it — reliable containers in particular are never swept.

import (
	"context"
	"testing"

	"google.golang.org/grpc"
)

func TestDisconnectPeerReclaimsContainers(t *testing.T) {
	srv := NewServer(FrameHandlerFunc(func(context.Context, *Frame) error {
		return nil
	}))
	defer srv.Stop()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "t.T",
		Methods: []grpc.MethodDesc{{
			MethodName: "M",
			Handler: func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
				return nil, nil
			},
		}},
	}, struct{}{})

	for i, reliable := range []bool{true, false} {
		peer := i // distinct comparable key per iteration
		open := &Frame{}
		open.SetEpoch(7)
		open.SetSid(1)
		open.SetSeq(1)
		open.SetFlags(FlagOpen | FlagClose)
		open.SetMethod("/t.T/M")
		open.SetPayload([]byte{})

		ctx := NewReliableContext(NewPeerContext(t.Context(), peer), reliable)
		if err := srv.Handle(ctx, open); err != nil {
			t.Fatal(err)
		}

		srv.mu.Lock()
		n := 0
		for k := range srv.peers {
			if k.peer == peer {
				n++
			}
		}
		srv.mu.Unlock()
		if n == 0 {
			t.Fatalf("reliable=%v: expected a peer container after OPEN", reliable)
		}

		srv.DisconnectPeer(peer, nil)

		srv.mu.Lock()
		n = 0
		for k := range srv.peers {
			if k.peer == peer {
				n++
			}
		}
		srv.mu.Unlock()
		if n != 0 {
			t.Fatalf("reliable=%v: %d container(s) leaked past DisconnectPeer", reliable, n)
		}
	}
}
