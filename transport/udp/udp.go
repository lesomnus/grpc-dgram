// Package udp runs drpc over UDP datagrams: one datagram carries one
// marshaled Envelop, the channel is unreliable (drpc's default mode), and
// nothing is ever fragmented — a message that does not fit MaxMessageSize is
// refused at send with drpc.ErrMessageTooLarge, which the core surfaces as
// ResourceExhausted on the owning call (PROTOCOL.md §4.4).
//
// UDP is connectionless: there is no transport-death signal to hook the §4.5
// teardown duty on. Vanished peers are handled by the core's own liveness
// machinery; tearing the endpoint down is the application's move — on the
// client one conn.Close(nil) suffices (it closes the Transport and the
// socket); on the server, close the socket, then Server.Stop.
// Consequently an ICMP unreachable —
// surfaced by the OS as ECONNREFUSED on a connected socket's reads and
// writes — is treated as datagram loss, not as an error: a momentarily
// absent peer (a restarting server) is exactly what the protocol rides out.
//
// The wire is plaintext. Deploy over an encrypted channel (e.g. DTLS) or on a
// trusted network — see PROTOCOL.md §15.
package udp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"google.golang.org/protobuf/proto"
)

// unblockNow is a read deadline in the past: setting it fails the pending
// blocking read so Serve can observe ctx cancellation.
var unblockNow = time.Unix(1, 0)

// transient reports errors that mean "this datagram (or a previous one) went
// nowhere", not "this socket is broken" — chiefly ICMP unreachable surfacing
// as ECONNREFUSED/EHOSTUNREACH/ENETUNREACH. The socket stays usable; the
// condition is indistinguishable from loss, which UDP already promises.
func transient(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH)
}

// DefaultMaxMessageSize keeps a datagram under the typical 1500-byte path
// MTU with room for IP/UDP headers and a tunnel or two.
const DefaultMaxMessageSize = 1200

// readBufferSize accepts anything a peer could legally send regardless of
// the local send limit (the largest UDP payload).
const readBufferSize = 65535

type options struct {
	maxMessageSize int
}

type Option func(*options)

// WithMaxMessageSize sets the largest marshaled Envelop this endpoint will
// send, in bytes. It bounds sends only; receives accept any datagram.
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

func marshal(e *drpc.Envelop, limit int) ([]byte, error) {
	data, err := proto.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("udp: %d-byte envelop over the %d-byte limit: %w",
			len(data), limit, drpc.ErrMessageTooLarge)
	}
	return data, nil
}

// serve pumps datagrams into h until ctx is done or the socket is closed.
// Malformed datagrams are dropped: garbage is normal on an open UDP port,
// and frame-level errors never tear down the channel (PROTOCOL.md §4.2).
func serve(ctx context.Context, h drpc.FrameHandler, read func([]byte) (int, context.Context, error)) error {
	buf := make([]byte, readBufferSize)
	for {
		n, rxCtx, err := read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			if transient(err) {
				continue
			}
			return err
		}
		e := &drpc.Envelop{}
		if err := proto.Unmarshal(buf[:n], e); err != nil {
			continue
		}
		drpc.Unpack(rxCtx, e, h)
	}
}

// Transport is the client-side endpoint: a connected datagram socket talking
// to one server. It is the tx handler for drpc.NewConn — implementing
// drpc.TransportInfo and drpc.ConnAttacher directly so neither is masked by
// a wrapper. drpc.NewConn attaches it and the receive pump starts by itself:
// no user goroutine, and conn.Close (or Close here) tears everything down,
// socket included.
type Transport struct {
	c   net.Conn
	max int

	attached atomic.Bool
	closer   sync.Once
}

// New wraps a connected datagram socket (e.g. net.Dial("udp", addr)). The
// Transport owns the socket from here on: Close closes it, and closing the
// attached Conn does too.
func New(c net.Conn, opts ...Option) *Transport {
	o := buildOptions(opts)
	return &Transport{c: c, max: o.maxMessageSize}
}

// AttachConn is called by drpc.NewConn: it starts the receive pump, which
// runs until the socket closes and performs the §4.5 teardown — conn.Close
// with the cause — on its way out. The single peer needs no peer key
// (PROTOCOL.md §6.4).
func (t *Transport) AttachConn(conn *drpc.Conn) {
	if !t.attached.CompareAndSwap(false, true) {
		panic("udp: transport already attached to a Conn")
	}
	go func() {
		err := serve(context.Background(), conn, func(buf []byte) (int, context.Context, error) {
			n, err := t.c.Read(buf)
			return n, context.Background(), err
		})
		conn.Close(err)
		t.Close()
	}()
}

// Close closes the socket, which stops the receive pump and, through its
// exit path, fails any live calls. Idempotent.
func (t *Transport) Close() error {
	t.closer.Do(func() { t.c.Close() })
	return nil
}

// Reliable reports false: UDP loses, duplicates, and reorders.
func (t *Transport) Reliable() bool { return false }

// Handle sends one frame as a single-frame envelop.
func (t *Transport) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return t.Send(ctx, e)
}

// Send transmits one envelop as one datagram. A transient unreachable
// condition is reported as success: the datagram is lost, which UDP already
// promises — failing the call instead would defeat the retransmission
// machinery that rides out a restarting peer.
func (t *Transport) Send(_ context.Context, e *drpc.Envelop) error {
	data, err := marshal(e, t.max)
	if err != nil {
		return err
	}
	if _, err := t.c.Write(data); err != nil && !transient(err) {
		return err
	}
	return nil
}

// Gateway is the server-side endpoint: one unconnected UDP socket serving
// many peers. The peer key handed to the core is the source netip.AddrPort.
// It is the tx handler for drpc.NewServer.
type Gateway struct {
	c   *net.UDPConn
	max int
}

// NewGateway wraps a listening UDP socket (net.ListenUDP).
func NewGateway(c *net.UDPConn, opts ...Option) *Gateway {
	o := buildOptions(opts)
	return &Gateway{c: c, max: o.maxMessageSize}
}

// Reliable reports false: UDP loses, duplicates, and reorders.
func (g *Gateway) Reliable() bool { return false }

// Handle sends one frame as a single-frame envelop to the peer named in ctx.
func (g *Gateway) Handle(ctx context.Context, f *drpc.Frame) error {
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{f})
	return g.Send(ctx, e)
}

// Send transmits one envelop as one datagram to the peer named in ctx.
func (g *Gateway) Send(ctx context.Context, e *drpc.Envelop) error {
	key, ok := drpc.PeerFromContext(ctx)
	if !ok {
		return errors.New("udp: no peer in context")
	}
	addr, ok := key.(netip.AddrPort)
	if !ok {
		return fmt.Errorf("udp: foreign peer key %T", key)
	}
	data, err := marshal(e, g.max)
	if err != nil {
		return err
	}
	if _, err := g.c.WriteToUDPAddrPort(data, addr); err != nil && !transient(err) {
		return err
	}
	return nil
}

// Serve delivers received frames to h — normally the *drpc.Server — with the
// source address attached as the peer key, until ctx is done or the socket
// is closed.
func (g *Gateway) Serve(ctx context.Context, h drpc.FrameHandler) error {
	stop := context.AfterFunc(ctx, func() { g.c.SetReadDeadline(unblockNow) })
	defer stop()
	return serve(ctx, h, func(buf []byte) (int, context.Context, error) {
		n, addr, err := g.c.ReadFromUDPAddrPort(buf)
		if err != nil {
			return n, ctx, err
		}
		return n, drpc.NewPeerContext(ctx, addr), nil
	})
}
