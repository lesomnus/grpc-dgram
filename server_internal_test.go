package drpc

// White-box: DisconnectPeer is the teardown that reclaims per-peer state
// (PROTOCOL.md §9.4, §10.6). Gateways issue a fresh peer key per connection,
// so no later traffic can address it and no cap or sweep would ever collect
// it — reliable containers in particular are never swept. And the one frame
// that must never create per-peer state: a connection grant (§4.2.1). Then
// two things only the internals can show about the connection window: a
// call that ends while its send holds credit refunds the Conn's window, and
// an evicted container's sender continues where it left off.

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
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

// Pins §4.2.1 Grants: a sid-0 WINDOW the receiver holds no connection sender
// for is dropped "never validated (§9.1), never answered with a RESET (§9.3),
// never creating state" — on the server no container and no ledger, on the
// Conn no lock and no credit. A frame no OPEN preceded must not cost a
// container: that is what makes the drop free on a datagram channel (§15).
func TestSid0GrantCreatesNoState(t *testing.T) {
	grant := func(epoch, peerEpoch uint32) *Frame {
		f := &Frame{}
		f.SetEpoch(epoch)
		f.SetPeerEpoch(peerEpoch)
		f.SetFlags(FlagWindow)
		f.SetWindow(100)
		return f
	}
	t.Run("server", func(t *testing.T) {
		var sent atomic.Int32
		srv := NewServer(FrameHandlerFunc(func(context.Context, *Frame) error {
			sent.Add(1)
			return nil
		}), WithReliable(true))
		defer srv.Stop()

		if err := srv.Handle(NewPeerContext(t.Context(), "peer"), grant(7, 0)); err != nil {
			t.Fatal(err)
		}
		srv.mu.Lock()
		containers, ledgers := len(srv.peers), len(srv.peerFlow)
		srv.mu.Unlock()
		if containers != 0 || ledgers != 0 {
			t.Fatalf("a sid-0 WINDOW before any OPEN created %d container(s) and %d ledger(s); want none", containers, ledgers)
		}
		if n := sent.Load(); n != 0 {
			t.Fatalf("answered with %d frame(s); want silence", n)
		}
	})
	t.Run("conn", func(t *testing.T) {
		var sent atomic.Int32
		c := NewConn(FrameHandlerFunc(func(context.Context, *Frame) error {
			sent.Add(1)
			return nil
		}), WithReliable(true))
		defer c.Close(nil)

		// Names this incarnation, but the Conn is locked to no server yet:
		// there is no sender counted against that epoch to credit.
		if err := c.Handle(t.Context(), grant(0xA11CE, c.epoch)); err != nil {
			t.Fatal(err)
		}
		on, granted, _ := c.connTx.snapshot()
		if !on || granted != int64(wConn) {
			t.Fatalf("connTx on/granted = %v/%d; want on at exactly W_conn: a grant from an unlocked epoch credits nothing", on, granted)
		}
		c.mu.Lock()
		locked := c.srvEpochSet
		c.mu.Unlock()
		if locked {
			t.Fatal("a sid-0 WINDOW must not lock the Conn to a server incarnation: only an accepted call frame does")
		}
		if n := sent.Load(); n != 0 {
			t.Fatalf("answered with %d frame(s); want silence", n)
		}
	})
}

// Pins §4.2.1 Sending: "a send that never reaches the wire — the adapter
// refused it (§4.4), or the call ended first — refunds both" windows. The
// call ends in the one window the sender's own checks cannot cover: after
// acquire2 handed back the credit and before the frame is built.
func TestClientSend_CallEndedAfterAcquireRefundsConnectionCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const srvEpoch = uint32(0xA11CE)
		var c *Conn
		var mu sync.Mutex
		data := 0
		tx := FrameHandlerFunc(func(ctx context.Context, f *Frame) error {
			mu.Lock()
			if f.isData() {
				data++
			}
			mu.Unlock()
			if !f.isOpen() {
				return nil
			}
			// The creation ack, with a window of 1: the second send parks on
			// the stream window.
			h := &Frame{}
			h.SetEpoch(srvEpoch)
			h.SetPeerEpoch(f.GetEpoch())
			h.SetSid(f.GetSid())
			h.SetSeq(1)
			h.SetWindow(1)
			return c.Handle(ctx, h)
		})
		var s *clientStream
		endUnderTheSend := ProtocolStatsFunc(func(ev ProtocolEvent) {
			if ev.Kind == EventFlowResume {
				// The grant woke the parked send and acquire2 handed it both
				// credits; the call ends now, before the frame is built —
				// a terminal, an abort or a ctx cancel racing the grant.
				s.finishLocal(status.Error(codes.Canceled, "ended under the send"))
			}
		})
		c = NewConn(tx, WithReliable(true), WithProtocolStats(endUnderTheSend))
		defer c.Close(nil)

		cs, err := c.NewStream(t.Context(), &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, "/t.T/Live")
		if err != nil {
			t.Fatal(err)
		}
		s = cs.(*clientStream)
		if err := s.SendMsg(&emptypb.Empty{}); err != nil {
			t.Fatal(err) // spends the stream's one credit and one of the Conn's
		}
		res := make(chan error, 1)
		go func() { res <- s.SendMsg(&emptypb.Empty{}) }()
		synctest.Wait() // parked on the stream window
		if _, _, sent := c.connTx.snapshot(); sent != 1 {
			t.Fatalf("connTx.sent = %d while parked, want 1: a park holds no connection credit", sent)
		}

		g := &Frame{}
		g.SetEpoch(srvEpoch)
		g.SetPeerEpoch(c.epoch)
		g.SetSid(s.sid)
		g.SetFlags(FlagWindow)
		g.SetWindow(1)
		if err := c.Handle(t.Context(), g); err != nil {
			t.Fatal(err)
		}
		if err := <-res; !errors.Is(err, io.EOF) {
			t.Fatalf("send on the ended call: err = %v, want io.EOF", err)
		}
		mu.Lock()
		got := data
		mu.Unlock()
		if got != 1 {
			t.Fatalf("%d data frames on the wire, want 1: the second never went out", got)
		}
		if _, _, sent := c.connTx.snapshot(); sent != 1 {
			t.Fatalf("connTx.sent = %d, want 1: the credit of a frame that never went out is refunded", sent)
		}
	})
}

// Pins §9.4 / §15: "the ledger keeps the evicted container's connection
// sender ... so that the incarnation's next OPEN continues it" — its credit,
// its settle and the raise it already got; a sid-0 grant addressed to it in
// the meantime still lands.
func TestEvictedContainerContinuesItsSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := NewServer(FrameHandlerFunc(func(context.Context, *Frame) error {
			return nil
		}), WithReliable(true), WithLimits(Limits{MaxDeadPeers: 2}))
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
		const peer = "peer"
		ctx := NewPeerContext(t.Context(), peer)
		open := func(epoch, sid uint32) {
			f := &Frame{}
			f.SetEpoch(epoch)
			f.SetSid(sid)
			f.SetSeq(1)
			f.SetFlags(FlagOpen | FlagClose)
			f.SetMethod("/t.T/M")
			f.SetPayload([]byte{})
			f.SetWindow(32)
			if err := srv.Handle(ctx, f); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()         // the call ran to completion: the container is idle
			time.Sleep(time.Second) // containers are evicted oldest first
		}
		grant := func(epoch, n uint32) {
			f := &Frame{}
			f.SetEpoch(epoch)
			f.SetFlags(FlagWindow)
			f.SetWindow(n)
			if err := srv.Handle(ctx, f); err != nil {
				t.Fatal(err)
			}
		}
		container := func(epoch uint32) *peerState {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			return srv.peers[epochKey{peer: peer, epoch: epoch}]
		}

		// Incarnation 1: settled by its OPEN, raised by this side (by hand:
		// the unary service sends no H), lifted by the client to 4096.
		open(1, 1)
		ps := container(1)
		if ps == nil {
			t.Fatal("no container for epoch 1")
		}
		ps.raised.Store(true)
		grant(1, 3*wConn)
		if on, granted, sent := ps.connTx.snapshot(); !on || granted != int64(4*wConn) || sent != 0 {
			t.Fatalf("epoch 1 before eviction: on %v, granted %d, sent %d", on, granted, sent)
		}

		// Two more idle incarnations: the third OPEN evicts 1, the oldest.
		open(2, 1)
		open(3, 1)
		if container(1) != nil {
			t.Fatal("epoch 1 must have been evicted by the cap")
		}
		if container(2) == nil || container(3) == nil {
			t.Fatal("epochs 2 and 3 must be there")
		}

		// The client returns credit to the evicted incarnation: it lands on
		// the held position, not on the floor.
		grant(1, 5)

		// A fourth idle incarnation evicts 2: the ledger now holds as many
		// positions as the cap holds containers — 1 and 2, oldest first —
		// and epoch 1 is its oldest entry. Exactly 2 × MaxDeadPeers idle
		// incarnations on the key: the bound §16 states holds exactly.
		open(4, 1)
		if container(2) != nil {
			t.Fatal("epoch 2 must have been evicted by the cap")
		}

		// Its next OPEN recreates the container from that position — not at
		// W_conn, not owed a second raise. That OPEN itself evicts 3 into a
		// full ledger: the position of the one coming back must not be what
		// the trim drops.
		open(1, 2)
		ps = container(1)
		if ps == nil {
			t.Fatal("epoch 1 must be back")
		}
		if on, granted, sent := ps.connTx.snapshot(); !on || granted != int64(4*wConn+5) || sent != 0 {
			t.Fatalf("epoch 1 recreated: on %v, granted %d, sent %d; want on, 4096+5, 0", on, granted, sent)
		}
		if !ps.raised.Load() {
			t.Fatal("the raise it already got must not be repeated")
		}
		srv.mu.Lock()
		pf := srv.peerFlow[peer]
		srv.mu.Unlock()
		if _, _, held := pf.unstash(1); held {
			t.Fatal("the position went back into the container: the ledger holds it no more")
		}
		// The ledger holds the two the cap evicted and nothing was trimmed:
		// 2, then 3 (evicted by epoch 1's return).
		for _, epoch := range []uint32{2, 3} {
			if _, _, held := pf.unstash(epoch); !held {
				t.Fatalf("epoch %d's position must still be held", epoch)
			}
		}
	})
}

// The server twin of the test above: a handler's Send whose call ends after
// acquire2 handed back the credit and before the frame is built refunds the
// connection window toward that client incarnation.
// Pins §4.2.1 Sending: "a send that never reaches the wire — the adapter
// refused it (§4.4), or the call ended first — refunds both".
func TestServerSend_CallEndedAfterAcquireRefundsConnectionCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const peer = "peer"
		var srv *Server
		var mu sync.Mutex
		data := 0
		tx := FrameHandlerFunc(func(_ context.Context, f *Frame) error {
			mu.Lock()
			defer mu.Unlock()
			if f.isData() {
				data++
			}
			return nil
		})
		key := callKey{peer: peer, epoch: 7, sid: 1}
		endUnderTheSend := ProtocolStatsFunc(func(ev ProtocolEvent) {
			if ev.Kind != EventFlowResume {
				return
			}
			// The grant woke the parked Send and acquire2 handed it both
			// credits; the client's abort lands now, before the frame is
			// built.
			srv.mu.Lock()
			st := srv.calls[key]
			srv.mu.Unlock()
			st.cancel(status.Error(codes.Canceled, "ended under the send"))
		})
		srv = NewServer(tx, WithReliable(true), WithProtocolStats(endUnderTheSend))
		defer srv.Stop()
		sent := make(chan error, 2)
		srv.RegisterService(&grpc.ServiceDesc{
			ServiceName: "t.T",
			Streams: []grpc.StreamDesc{{
				StreamName:    "S",
				ServerStreams: true,
				Handler: func(_ any, ss grpc.ServerStream) error {
					sent <- ss.SendMsg(&emptypb.Empty{}) // the stream's one credit
					sent <- ss.SendMsg(&emptypb.Empty{}) // parks on the stream window
					return nil
				},
			}},
		}, struct{}{})

		open := &Frame{}
		open.SetEpoch(key.epoch)
		open.SetSid(key.sid)
		open.SetSeq(1)
		open.SetFlags(FlagOpen | FlagClose)
		open.SetMethod("/t.T/S")
		open.SetPayload([]byte{})
		open.SetWindow(1) // a stream window of one
		ctx := NewPeerContext(t.Context(), peer)
		if err := srv.Handle(ctx, open); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the first Send went out, the second is parked
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
		srv.mu.Lock()
		ps := srv.peers[epochKey{peer: peer, epoch: key.epoch}]
		srv.mu.Unlock()
		if _, _, n := ps.connTx.snapshot(); n != 1 {
			t.Fatalf("connTx.sent = %d while parked, want 1: a park holds no connection credit", n)
		}

		g := &Frame{}
		g.SetEpoch(key.epoch)
		g.SetSid(key.sid)
		g.SetFlags(FlagWindow)
		g.SetWindow(1)
		if err := srv.Handle(ctx, g); err != nil {
			t.Fatal(err)
		}
		if err := <-sent; status.Code(err) != codes.Canceled {
			t.Fatalf("send on the ended call: err = %v, want CANCELED", err)
		}
		synctest.Wait()
		mu.Lock()
		got := data
		mu.Unlock()
		if got != 1 {
			t.Fatalf("%d data frames on the wire, want 1: the second never went out", got)
		}
		if _, _, n := ps.connTx.snapshot(); n != 1 {
			t.Fatalf("connTx.sent = %d, want 1: the credit of a frame that never went out is refunded", n)
		}
	})
}
