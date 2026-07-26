package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/transport/pion"
	"github.com/pion/webrtc/v4"
)

// signaler is the whole signaling story: the page POSTs an offer, this answers
// once, and every byte after that travels on the DataChannel. There is no
// trickle ICE and no STUN server — browser and server share a host, so the
// candidates in the first exchange are enough. A real deployment would keep
// its own signaling channel; drpc does not care how the channel is negotiated.
type signaler struct {
	ctx context.Context
	gw  *pion.Gateway
	srv *drpc.Server

	mu    sync.Mutex
	peers []*webrtc.PeerConnection
}

func (s *signaler) offer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&offer); err != nil {
		http.Error(w, "bad offer: "+err.Error(), http.StatusBadRequest)
		return
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The page creates the channel, so this side receives it.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		// Bind MUST run synchronously here: pion holds the channel's read
		// loop until this callback returns, and messages that arrive before a
		// handler is registered are dropped.
		s.gw.Bind(dc)
		log.Printf("data channel %q accepted", dc.Label())
		go func() {
			// ServePeer blocks — hence the goroutine — and on every exit
			// performs the §4.5 teardown, srv.DisconnectPeer, which fails
			// that peer's live calls. In reliable mode nothing else would.
			err := s.gw.ServePeer(s.ctx, s.srv, dc)
			log.Printf("data channel %q gone: %v", dc.Label(), err)
		}()
	})
	// A severed peer (page closed, network gone) may never surface OnClose on
	// the channel: the SCTP shutdown needs a live transport to travel over.
	// Watching the connection state is the adapter's documented companion duty.
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("peer connection: %s", st)
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateDisconnected:
			_ = pc.Close() // closes the channel, which tears the peer down
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, "bad offer: "+err.Error(), http.StatusBadRequest)
		_ = pc.Close()
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		_ = pc.Close()
		return
	}
	// Answer only once every candidate is in the SDP: one request, one reply.
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		_ = pc.Close()
		return
	}
	select {
	case <-gathered:
	case <-time.After(10 * time.Second):
		http.Error(w, "ICE gathering timed out", http.StatusGatewayTimeout)
		_ = pc.Close()
		return
	case <-r.Context().Done():
		_ = pc.Close()
		return
	}

	s.mu.Lock()
	s.peers = append(s.peers, pc)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(pc.LocalDescription()); err != nil {
		log.Printf("answer: %v", err)
	}
}

// closeAll drops every peer connection; the gateway's ServePeer loops then
// exit and disconnect their peers.
func (s *signaler) closeAll() {
	s.mu.Lock()
	peers := s.peers
	s.peers = nil
	s.mu.Unlock()
	for _, pc := range peers {
		_ = pc.Close()
	}
}
