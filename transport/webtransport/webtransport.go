// Package webtransport runs drpc over WebTransport datagrams: one datagram
// carries one marshaled Envelope, the channel is unreliable (drpc's default
// mode), and nothing is ever fragmented — a message that does not fit
// MaxMessageSize is refused at send with drpc.ErrMessageTooLarge, which the
// core surfaces as ResourceExhausted on the owning call (PROTOCOL.md §4.4).
//
// The adapter takes an established *webtransport.Session
// (quic-go/webtransport-go); dialing, the HTTP/3 server, TLS and the CONNECT
// upgrade stay with the application. Only the session's datagram side is
// used — a reliable channel over one of its streams is a possible second
// step, on the pion precedent.
//
// A session is connection-oriented, so the §4.5 teardown duty applies. The
// death signal is the session's own closure — its Context, which ends on a
// WT_CLOSE_SESSION from either side, a QUIC error, or the idle timeout — and
// it fires whether or not the datagram pump is making progress. The attached
// client pump and ServePeer perform the teardown (Conn.Close /
// Server.DisconnectPeer with the cause) on every exit.
//
// The wire is QUIC, hence always TLS: there is no plaintext path. The
// protocol itself still authenticates nothing beyond the session — see
// PROTOCOL.md §15.
package webtransport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	wt "github.com/quic-go/webtransport-go"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
)

// DefaultMaxMessageSize matches transport/udp and stays under the datagram
// ceiling a session has right after its handshake: quic-go seeds it from
// the 1280-byte initial packet size — 1243 B of QUIC payload, one of which
// is the HTTP/3 quarter-stream-id prefix — and path-MTU discovery only
// raises it from there. The library exposes no getter for the live ceiling
// (only the refusal, see Send), so the default is a constant rather than
// "what the session reports".
const DefaultMaxMessageSize = 1200

type options struct {
	maxMessageSize int
}

type Option func(*options)

// WithMaxMessageSize sets the largest marshaled Envelope this endpoint will
// send, in bytes. It bounds sends only; receives accept any datagram. The
// QUIC stack keeps its own, path-dependent ceiling underneath: a limit
// raised past it does not fragment — the send is refused the same way
// (PROTOCOL.md §4.4).
func WithMaxMessageSize(n int) Option {
	return func(o *options) { o.maxMessageSize = n }
}

func buildOptions(opts []Option) options {
	o := options{maxMessageSize: DefaultMaxMessageSize}
	for _, f := range opts {
		f(&o)
	}
	return o
}

func marshal(e *drpc.Envelope, limit int) ([]byte, error) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("webtransport: %d-byte envelope over the %d-byte limit: %w",
			len(data), limit, drpc.ErrMessageTooLarge)
	}
	return data, nil
}

// send transmits one marshaled envelope as one datagram, waiting on the
// stack only as long as ctx and the session live. The stack checks its own
// ceiling — the peer's max_datagram_frame_size and the current path-MTU
// estimate — synchronously, before anything is queued; a refusal there is a
// size refusal like the adapter's own (PROTOCOL.md §4.4): the message is
// unsendable now, and reporting it as loss would leave the call to its
// deadline. Under the default limit this cannot fire (see
// DefaultMaxMessageSize); a raised limit can.
//
// Otherwise the payload is copied into the connection's datagram queue,
// which holds 32 frames and drains only as the packer sends them — under
// congestion control, so on a path that has stopped acknowledging it drains
// one frame per PTO probe, at intervals that double, until QUIC's idle
// timeout closes the connection (quic.Config.MaxIdleTimeout, 30 s by
// default). Nothing like a socket buffer, then, and the stack offers no
// non-blocking variant: the queueing runs on its own goroutine, and the
// caller waits for it only while ctx and the session live. The core hands
// Handle the call's ctx for message frames, which its teardown cancels, so
// a handler whose peer vanished is released with the peer's calls (§4.5)
// instead of parking until the idle timeout; terminals ride an
// uncancellable ctx and wait on the session alone. A send abandoned this
// way keeps its goroutine — and its datagram, which may still go out late —
// until a slot frees or the connection dies, both bounded by the idle
// timeout. The goroutine is the price of one send; the stack's own copy and
// packing dwarf it.
func send(ctx context.Context, sess *wt.Session, data []byte) error {
	done := make(chan error, 1)
	go func() { done <- sess.SendDatagram(data) }()
	select {
	case err := <-done:
		var tooLarge *quic.DatagramTooLargeError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("webtransport: %d-byte envelope over the session's %d-byte datagram ceiling (HTTP/3 prefix included): %w",
				len(data), tooLarge.MaxDatagramPayloadSize, drpc.ErrMessageTooLarge)
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.Context().Done():
		return errors.New("webtransport: session closed")
	}
}

// serve pumps datagrams from sess into h, each delivered under rxCtx, until
// ctx is done or the session ends; on death it flushes what was received
// first. Unreliable-mode Handle never blocks, so delivery is synchronous.
// Returns nil on ctx cancellation or a clean close, the death cause
// otherwise; exiting abandons the session either way, so the caller owes
// the §4.5 teardown on every return.
func serve(ctx context.Context, sess *wt.Session, rxCtx context.Context, h drpc.FrameHandler) error {
	// The session's own closure is the death signal (§4.5): it ends the
	// pending receive from outside this loop, so detection never depends on
	// the loop making progress. (The stack fails the receive itself too
	// once the session closes — after handing out what was queued before
	// the end, which is why nothing here drains explicitly.)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(sess.Context(), cancel)
	defer stop()
	for {
		data, err := sess.ReceiveDatagram(rctx)
		if err == nil {
			deliver(rxCtx, data, h)
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		// Dead. The receive can fail a moment before the Context ends (the
		// stack fails the CONNECT stream first, then finishes closing the
		// session); wait it out — the close reason is only on file once it
		// has. Then flush: a receive on the closed session hands out what
		// was queued before the end, and after that the receive error,
		// which carries the death typed where the close reason does not.
		<-sess.Context().Done()
		for {
			data, err = sess.ReceiveDatagram(sess.Context())
			if err != nil {
				break
			}
			deliver(rxCtx, data, h)
		}
		return deathCause(sess, err)
	}
}

// deliver unmarshals one datagram and hands its frames to h in order.
// Malformed datagrams are dropped — frame-level errors never tear down the
// channel (PROTOCOL.md §4.2).
func deliver(ctx context.Context, data []byte, h drpc.FrameHandler) {
	e := &drpc.Envelope{}
	if err := proto.Unmarshal(data, e); err != nil {
		return
	}
	drpc.Unpack(ctx, e, h)
}

// deathCause names why sess ended, given the closed session and the error
// its receive side reports. A goodbye — a WT_CLOSE_SESSION with code 0 from
// either side, or the peer closing its connection with H3_NO_ERROR, which
// is what reaches a server when a client's goodbye capsule loses the race
// with its connection close — is a clean end and reports nil; a coded
// close, a QUIC error, or the idle timeout is the cause.
//
// webtransport-go keeps the close reason private and surfaces it only
// through stream operations on the dead session, so ask one: OpenStream on
// a closed session returns the reason without touching the wire. That
// reason files a connection-level death as text; recvErr still has it
// typed, so it is preferred for the cause.
func deathCause(sess *wt.Session, recvErr error) error {
	_, closeErr := sess.OpenStream()
	if closeErr == nil {
		return errors.New("webtransport: session closed")
	}
	var se *wt.SessionError
	if errors.As(closeErr, &se) {
		if se.ErrorCode == 0 {
			return nil
		}
		side := "locally"
		if se.Remote {
			side = "by peer"
		}
		// SessionError prints its message alone, which may be empty.
		sep := ""
		if se.Message != "" {
			sep = ": "
		}
		return fmt.Errorf("webtransport: session closed %s with code %d%s%w", side, se.ErrorCode, sep, se)
	}
	var ae *quic.ApplicationError
	if errors.As(recvErr, &ae) && ae.Remote && ae.ErrorCode == quic.ApplicationErrorCode(http3.ErrCodeNoError) {
		return nil
	}
	cause := recvErr
	if cause == nil || errors.Is(cause, context.Canceled) {
		cause = closeErr
	}
	return fmt.Errorf("webtransport: session died: %w", cause)
}

// Transport is the client-side endpoint: one session talking to one server.
// It is the tx handler for drpc.NewConn — implementing drpc.TransportInfo
// and drpc.ConnAttacher directly so neither is masked by a wrapper.
// drpc.NewConn attaches it and the receive pump starts by itself: no user
// goroutine, and conn.Close (or Close here) tears everything down, session
// included.
type Transport struct {
	sess *wt.Session
	max  int

	attached atomic.Bool
	closer   sync.Once
}

// New wraps an established session (e.g. from webtransport.Transport.Dial).
// The Transport owns the session from here on: Close closes it, and closing
// the attached Conn does too.
func New(sess *wt.Session, opts ...Option) *Transport {
	o := buildOptions(opts)
	return &Transport{sess: sess, max: o.maxMessageSize}
}

// AttachConn is called by drpc.NewConn: it starts the receive pump, which
// runs until the session ends — its own closure being the death signal,
// independent of the pump (§4.5) — and performs the teardown, conn.Close
// with the cause, on its way out. The single peer needs no peer key
// (PROTOCOL.md §6.4).
func (t *Transport) AttachConn(conn *drpc.Conn) {
	if !t.attached.CompareAndSwap(false, true) {
		panic("webtransport: transport already attached to a Conn")
	}
	go func() {
		err := serve(context.Background(), t.sess, context.Background(), conn)
		conn.Close(err)
		t.Close()
	}()
}

// Close closes the session with code 0 — the goodbye the peer reports as a
// clean end — which stops the receive pump and, through its exit path,
// fails any live calls. On a dialed session this also closes the QUIC
// connection (webtransport.Transport ties the two). Idempotent.
func (t *Transport) Close() error {
	t.closer.Do(func() { t.sess.CloseWithError(0, "") })
	return nil
}

// Reliable reports false: QUIC datagrams are neither retransmitted nor
// ordered, so they are lost and reordered.
func (t *Transport) Reliable() bool { return false }

// Peer names the remote end for grpc.Peer and peer.FromContext
// (drpc.TransportPeer): the session's UDP addresses.
func (t *Transport) Peer() *peer.Peer {
	return &peer.Peer{Addr: t.sess.RemoteAddr(), LocalAddr: t.sess.LocalAddr()}
}

// Handle sends one frame as a single-frame envelope.
func (t *Transport) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelope{}
	e.SetFrames([]*drpc.Frame{f})
	return t.Send(ctx, e)
}

// Send transmits one envelope as one datagram. An envelope over the size
// limit — the adapter's or the stack's — is refused synchronously with an
// error wrapping drpc.ErrMessageTooLarge (PROTOCOL.md §4.4). A send the
// stack cannot take at once — its queue is full because the path has
// stopped acknowledging — waits only as long as ctx and the session live,
// and returns ctx's error when ctx ends first (see send).
func (t *Transport) Send(ctx context.Context, e *drpc.Envelope) error {
	data, err := marshal(e, t.max)
	if err != nil {
		return err
	}
	return send(ctx, t.sess, data)
}

// peerKey identifies one served session. Deliberately opaque and
// process-local: remote addresses collide behind proxies, so identity is a
// fresh counter per ServePeer, never reused — a peer that reconnects on a
// fresh session is a fresh peer.
type peerKey uint64

// Gateway is the server-side endpoint: one registered session per peer. It
// is the tx handler for drpc.NewServer — implementing drpc.TransportInfo
// directly so mode discovery is not masked by a wrapper: datagram-only means
// one mode, so the server may rely on TransportInfo alone (PROTOCOL.md
// §4.3); ServePeer annotates each peer anyway, as the UDP gateway does.
type Gateway struct {
	o    options
	next atomic.Uint64

	mu    sync.Mutex
	peers map[peerKey]*wt.Session
}

// NewGateway builds a Gateway; sessions join it via ServePeer.
func NewGateway(opts ...Option) *Gateway {
	return &Gateway{o: buildOptions(opts), peers: map[peerKey]*wt.Session{}}
}

// Reliable reports false: QUIC datagrams are neither retransmitted nor
// ordered, so they are lost and reordered.
func (g *Gateway) Reliable() bool { return false }

// Handle sends one frame as a single-frame envelope to the peer named in ctx.
func (g *Gateway) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelope{}
	e.SetFrames([]*drpc.Frame{f})
	return g.Send(ctx, e)
}

// Send transmits one envelope as one datagram to the peer named in ctx, with
// the same size refusal and the same ctx-bounded wait as Transport.Send.
func (g *Gateway) Send(ctx context.Context, e *drpc.Envelope) error {
	key, ok := drpc.PeerFromContext(ctx)
	if !ok {
		return errors.New("webtransport: no peer in context")
	}
	k, ok := key.(peerKey)
	if !ok {
		return fmt.Errorf("webtransport: foreign peer key %T", key)
	}
	g.mu.Lock()
	sess := g.peers[k]
	g.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("webtransport: peer %d is disconnected", k)
	}
	data, err := marshal(e, g.o.maxMessageSize)
	if err != nil {
		return err
	}
	return send(ctx, sess, data)
}

// ServePeer registers sess under a fresh peer key and delivers received
// frames to srv with the key attached (PROTOCOL.md §6.4) — annotated
// unreliable, and naming the session's addresses for peer.FromContext —
// until ctx is done or the session ends, its own closure being the death
// signal (§4.5). On EVERY exit it deregisters the peer and calls
// srv.DisconnectPeer with the cause: exiting abandons the session (the key
// is never reused), so the peer's live calls and state die with it whether
// the session died or the caller cancelled ctx. The session itself stays
// the caller's to close, as with every gateway — webtransport.Server.Close
// closes them all. Returns nil on ctx cancellation or a clean close, the
// death cause otherwise.
//
// The session outlives the HTTP handler that upgraded it, so ServePeer takes
// a server-lifetime ctx, not the request's:
//
//	sess, err := s.Upgrade(w, r)
//	if err != nil { return }
//	go gw.ServePeer(ctx, srv, sess)
func (g *Gateway) ServePeer(ctx context.Context, srv *drpc.Server, sess *wt.Session) error {
	key := peerKey(g.next.Add(1))
	g.mu.Lock()
	g.peers[key] = sess
	g.mu.Unlock()

	rxCtx := drpc.NewReliableContext(drpc.NewPeerContext(ctx, key), false)
	// The peer key is opaque (one session = one key), so the address the
	// server reports to handlers comes from the session itself.
	rxCtx = peer.NewContext(rxCtx, &peer.Peer{Addr: sess.RemoteAddr(), LocalAddr: sess.LocalAddr()})
	err := serve(ctx, sess, rxCtx, srv)

	g.mu.Lock()
	delete(g.peers, key)
	g.mu.Unlock()
	srv.DisconnectPeer(key, err)
	return err
}
