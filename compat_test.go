package drpc_test

// compat_test.go pins the gRPC API-parity surface: the call options, endpoint
// options and callbacks that generated code and gRPC users already rely on
// must behave here exactly as they do on grpc-go, because G2 (PROTOCOL.md §1)
// promises that protoc-gen-go-grpc output works unchanged against drpc.
//
// What each group pins:
//   - per-call size caps (Appendix B, §12.1): recv 4 MiB / send unlimited by
//     default, ResourceExhausted past them, the recv cap measured on the
//     DECOMPRESSED message and the send cap on the COMPRESSED bytes;
//   - grpc.OnFinish: exactly once, carrying the call's final error (§14);
//   - grpc.Peer / peer.FromContext: the transport names the remote end, never
//     the frame contents (§6.4);
//   - PerRPCCredentials: a metadata producer riding the OPEN (§11, §15) whose
//     transport-security demand drpc cannot attest;
//   - grpc.CallContentSubtype: names a registered codec on the OPEN (§12);
//   - Server.GetServiceInfo: the registry grpc.Server exposes (§13);
//   - SendHeader: one flush per call, INTERNAL on a second, and the core's own
//     creation ack is not a flush (§8, §11);
//   - Header(): never a context error, whatever the caller's ctx does (§11);
//   - metadata: "-bin" values are raw octets on the wire, and an illegal key or
//     a non-printable text value fails the call locally with INTERNAL (§11).

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"github.com/lesomnus/grpc-dgram/transport/udp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/gzip" // registers the §12.1 interop baseline
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// cpShapeMask is PROTOCOL.md §7.1's SHAPE_MASK: every routing decision reads
// flags & SHAPE_MASK, never the whole bitmask — a COMPRESSED data frame is
// still a data frame, a COMPRESSED terminal still a terminal. The tests below
// classify frames the same way, so a compressed call is not silently exempted
// from any assertion.
const cpShapeMask = drpc.FlagOpen | drpc.FlagClose | drpc.FlagReset | drpc.FlagPing | drpc.FlagWindow

func cpShape(f *drpc.Frame) uint32       { return f.GetFlags() & cpShapeMask }
func cpIsTerminal(f *drpc.Frame) bool    { return cpShape(f) == drpc.FlagClose && f.HasCode() }
func cpIsHeaderFrame(f *drpc.Frame) bool { return cpShape(f) == 0 && !f.HasPayload() }

// cpTxFrames snapshots the recorded client->server frames.
func cpTxFrames(c *Client) []*drpc.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*drpc.Frame(nil), c.tx...)
}

// cpTerminal returns the first terminal frame of fs, by shape (§7.1).
func cpTerminal(t *testing.T, fs []*drpc.Frame) *drpc.Frame {
	t.Helper()
	for _, f := range fs {
		if cpIsTerminal(f) {
			return f
		}
	}
	t.Fatal("no terminal frame recorded")
	return nil
}

// cpOpen returns the OPEN frame of fs (§8).
func cpOpen(t *testing.T, fs []*drpc.Frame) *drpc.Frame {
	t.Helper()
	for _, f := range fs {
		if cpShape(f)&drpc.FlagOpen != 0 {
			return f
		}
	}
	t.Fatal("no OPEN frame recorded")
	return nil
}

// cpRecv takes one value from ch within a generous real-time bound; a test
// that has to wait longer than this is broken, not slow.
func cpRecv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// cpSilent asserts nothing more arrives on ch. The window is short because
// the event it guards against would already have happened: both fire on the
// call's own completion path.
func cpSilent[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("expected no further %s, got %v", what, v)
	case <-time.After(100 * time.Millisecond):
	}
}

// cpRandomText builds n bytes of high-entropy printable text: gzip cannot
// meaningfully shrink it, which is what separates "the cap looked at the
// compressed bytes" from "the cap looked at the message".
func cpRandomText(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	r := rand.New(rand.NewPCG(0x5eed, 0x1dea))
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

func cpMarshaledLen(t *testing.T, m proto.Message) int {
	t.Helper()
	b, err := proto.Marshal(m)
	x.NoError(t, err)
	return len(b)
}

// ---------------------------------------------------------------------------
// §12.1 / Appendix B — per-call size caps (grpc.MaxCallRecvMsgSize,
// grpc.MaxCallSendMsgSize, WithMaxRecvMsgSize, WithMaxSendMsgSize)
// ---------------------------------------------------------------------------

// TestCompat_MaxRecvMsgSize pins the receive cap of Appendix B ("recv 4 MiB",
// gRPC's own default): a message past the cap fails ITS CALL with
// ResourceExhausted — grpc-go's code and wording — and never tears the channel
// down. The cap is per call and settable on either role, from the call option
// or the endpoint option.
func TestCompat_MaxRecvMsgSize(t *testing.T) {
	const cap4k = 4096
	big := strings.Repeat("a", 100_000)

	t.Run("client call option", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: big}.Build(),
			grpc.MaxCallRecvMsgSize(cap4k))
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
		x.True(t, strings.Contains(status.Convert(err).Message(), "received message larger than max"),
			"grpc-go's wording: ", status.Convert(err).Message())
	})

	t.Run("client endpoint option", func(t *testing.T) {
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithMaxRecvMsgSize(cap4k)},
		}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: big}.Build())
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
	})

	t.Run("client call option overrides the endpoint default", func(t *testing.T) {
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithMaxRecvMsgSize(cap4k)},
		}.Use(t)
		defer stop()

		res, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: big}.Build(),
			grpc.MaxCallRecvMsgSize(1<<20))
		x.NoError(t, err)
		x.Equal(t, len(big), len(res.GetMessage()))
	})

	t.Run("server endpoint option", func(t *testing.T) {
		client, stop := PipeOption{
			ServerOpts: []drpc.ServerOption{drpc.WithMaxRecvMsgSize(cap4k)},
		}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: big}.Build())
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
		// It is the SERVER that refused: the status travelled as a terminal
		// frame (§8), it was not synthesized by the client.
		term := cpTerminal(t, client.rxFrames())
		x.Equal(t, codes.ResourceExhausted, codes.Code(term.GetCode()))
	})

	t.Run("gRPC's 4 MiB default", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		// Just past 4 MiB with nothing configured anywhere: the default cap
		// (Appendix B) has to be the one that fires.
		_, err := client.Once(t.Context(),
			echo.EchoRequest_builder{Message: strings.Repeat("a", 4*1024*1024+16)}.Build())
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
	})

	t.Run("the cap measures the decompressed message", func(t *testing.T) {
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithDefaultCallOptions(grpc.UseCompressor("gzip"))},
		}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: big}.Build(),
			grpc.MaxCallRecvMsgSize(cap4k))
		x.Equal(t, codes.ResourceExhausted, status.Code(err))

		// The wire payload was far UNDER the cap — only the decompressed
		// message is over it, and §12.1 says that is what the cap measures
		// (a compression bomb costs nothing).
		term := cpTerminal(t, client.rxFrames())
		x.True(t, term.GetFlags()&drpc.FlagCompressed != 0, "the response payload must be compressed")
		x.True(t, len(term.GetPayload()) < cap4k,
			"the compressed terminal payload must fit the cap: ", len(term.GetPayload()))
	})
}

// TestCompat_MaxSendMsgSize pins the send cap of Appendix B ("send
// unlimited" by default): a message past the cap fails its call with
// ResourceExhausted before anything reaches the wire, and — per §12.1 —
// the cap counts the COMPRESSED bytes, since those are what the transport
// carries.
func TestCompat_MaxSendMsgSize(t *testing.T) {
	const cap4k = 4096
	big := strings.Repeat("a", 100_000)

	t.Run("client call option", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: big}.Build(),
			grpc.MaxCallSendMsgSize(cap4k))
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
		x.True(t, strings.Contains(status.Convert(err).Message(), "trying to send message larger than max"),
			"grpc-go's wording: ", status.Convert(err).Message())

		// Refused locally: the OPEN never left, so the server never saw the
		// call (only the abandon-abort of §10.3 may follow).
		for _, f := range cpTxFrames(client) {
			x.True(t, cpShape(f)&drpc.FlagOpen == 0, "the oversize request must not reach the wire")
			x.False(t, f.HasPayload(), "no payload may reach the wire")
		}
	})

	t.Run("client endpoint option", func(t *testing.T) {
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithMaxSendMsgSize(cap4k)},
		}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: big}.Build())
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
	})

	t.Run("server endpoint option", func(t *testing.T) {
		client, stop := PipeOption{
			ServerOpts: []drpc.ServerOption{drpc.WithMaxSendMsgSize(cap4k)},
		}.Use(t)
		defer stop()

		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message: big,
			Repeat:  1,
		}.Build())
		x.NoError(t, err)

		_, err = stream.Recv()
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
		term := cpTerminal(t, client.rxFrames())
		x.Equal(t, codes.ResourceExhausted, codes.Code(term.GetCode()))
	})

	t.Run("the cap measures the compressed bytes", func(t *testing.T) {
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithDefaultCallOptions(grpc.UseCompressor("gzip"))},
		}.Use(t)
		defer stop()

		req := echo.EchoRequest_builder{Message: big}.Build()
		x.True(t, cpMarshaledLen(t, req) > cap4k, "the message itself must exceed the cap")

		res, err := client.Once(t.Context(), req, grpc.MaxCallSendMsgSize(cap4k))
		x.NoError(t, err, "a message that COMPRESSES under the cap must be sendable")
		x.Equal(t, len(big), len(res.GetMessage()))

		open := cpOpen(t, cpTxFrames(client))
		x.True(t, open.GetFlags()&drpc.FlagCompressed != 0, "the request payload must be compressed")
		x.True(t, len(open.GetPayload()) < cap4k,
			"what the cap measured is what the frame carried: ", len(open.GetPayload()))
	})

	t.Run("incompressible past the cap still fails", func(t *testing.T) {
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithDefaultCallOptions(grpc.UseCompressor("gzip"))},
		}.Use(t)
		defer stop()

		// High-entropy text: compression would EXPAND it, so §12.1 says send
		// it raw — and raw is over the cap.
		_, err := client.Once(t.Context(),
			echo.EchoRequest_builder{Message: cpRandomText(100_000)}.Build(),
			grpc.MaxCallSendMsgSize(cap4k))
		x.Equal(t, codes.ResourceExhausted, status.Code(err))
		for _, f := range cpTxFrames(client) {
			x.False(t, f.HasPayload(), "nothing may reach the wire")
		}
	})
}

// ---------------------------------------------------------------------------
// §14 — grpc.OnFinish
// ---------------------------------------------------------------------------

// TestCompat_OnFinish pins grpc.OnFinish: it fires exactly once per call,
// carrying the call's final error — nil on success, the call's status
// otherwise — which is the callback gRPC instrumentation hangs its
// per-call bookkeeping on.
func TestCompat_OnFinish(t *testing.T) {
	t.Run("success reports nil, once", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		fin := make(chan error, 4)
		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build(), grpc.OnFinish(func(err error) { fin <- err }))
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		x.NoError(t, cpRecv(t, fin, "OnFinish"))
		cpSilent(t, fin, "OnFinish call")
	})

	t.Run("failure reports the call's status, once", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		fin := make(chan error, 4)
		_, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Status: echo.Status_builder{Code: int32(codes.NotFound), Message: "no such thing"}.Build(),
		}.Build(), grpc.OnFinish(func(err error) { fin <- err }))
		x.Equal(t, codes.NotFound, status.Code(err))

		got := cpRecv(t, fin, "OnFinish")
		assertStatusEqual(t, status.Convert(err), status.Convert(got))
		cpSilent(t, fin, "OnFinish call")
	})

	t.Run("a cancelled stream reports its status, once", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		ctx, cancel := context.WithCancel(t.Context())
		fin := make(chan error, 4)
		stream, err := client.Live(ctx, grpc.OnFinish(func(err error) { fin <- err }))
		x.NoError(t, err)
		cancel()

		_, err = stream.Recv()
		x.Equal(t, codes.Canceled, status.Code(err))
		x.Equal(t, codes.Canceled, status.Code(cpRecv(t, fin, "OnFinish")))
		cpSilent(t, fin, "OnFinish call")
	})
}

// ---------------------------------------------------------------------------
// §6.4 — grpc.Peer and peer.FromContext
// ---------------------------------------------------------------------------

// cpUDPTiming keeps the real-socket test snappy. Real sockets cannot enter a
// synctest bubble, so these are real timers.
var cpUDPTiming = drpc.Timing{
	Call:       2 * time.Second,
	Liveness:   3 * time.Second,
	Retransmit: 100 * time.Millisecond,
}

// TestCompat_Peer pins PROTOCOL.md §6.4: the peer is supplied by the
// transport, never taken from frame contents. Over the UDP adapter that means
// real addresses on both sides — grpc.Peer(&p) names the server the client
// talks to, and peer.FromContext inside a handler names the datagram's source
// — so code written against gRPC's peer API works unchanged.
func TestCompat_Peer(t *testing.T) {
	t.Run("udp adapter names both ends", func(t *testing.T) {
		seen := make(chan *peer.Peer, 4)

		pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		x.NoError(t, err)
		gw := udp.NewGateway(pc)
		srv := drpc.NewServer(gw, drpc.WithTiming(cpUDPTiming),
			drpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				p, ok := peer.FromContext(ctx)
				if !ok {
					p = nil
				}
				seen <- p
				return h(ctx, req)
			}))
		echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

		ctx, cancel := context.WithCancel(t.Context())
		served := make(chan struct{})
		go func() { defer close(served); _ = gw.Serve(ctx, srv) }()

		sock, err := net.Dial("udp", pc.LocalAddr().String())
		x.NoError(t, err)
		conn := drpc.NewConn(udp.New(sock), drpc.WithTiming(cpUDPTiming))
		t.Cleanup(func() {
			conn.Close(nil)
			srv.Stop()
			cancel()
			pc.Close()
			<-served
		})
		client := echo.NewEchoServiceClient(conn)

		var p peer.Peer
		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build(), grpc.Peer(&p))
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		// Client side: the transport knew the remote end (drpc.TransportPeer).
		x.True(t, p.Addr != nil, "grpc.Peer must be populated")
		x.Equal(t, sock.RemoteAddr().String(), p.Addr.String())
		x.Equal(t, sock.LocalAddr().String(), p.LocalAddr.String())

		// Server side: the handler's ctx names the datagram's source.
		hp := cpRecv(t, seen, "the handler's peer")
		x.True(t, hp != nil, "peer.FromContext must work inside a handler")
		x.True(t, hp.Addr != nil && hp.LocalAddr != nil, "the adapter's peer must name both ends")
		x.Equal(t, sock.LocalAddr().String(), hp.Addr.String())
		x.Equal(t, pc.LocalAddr().String(), hp.LocalAddr.String())

		// Client side again: grpc-go's ClientStream.Context() names the peer,
		// so interceptors and application code read it with peer.FromContext.
		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        1,
		}.Build())
		x.NoError(t, err)
		sp, ok := peer.FromContext(stream.Context())
		x.True(t, ok, "the client stream's context must name the peer")
		x.Equal(t, sock.RemoteAddr().String(), sp.Addr.String())
	})

	t.Run("an address peer key alone names the peer", func(t *testing.T) {
		// A gateway that attaches only the drpc peer key (§6.4) still gets
		// gRPC's peer API, as long as the key is an address.
		seen := make(chan *peer.Peer, 4)
		srv := drpc.NewServer(
			drpc.FrameHandlerFunc(func(context.Context, *drpc.Frame) error { return nil }),
			drpc.WithReliable(true),
			drpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				p, _ := peer.FromContext(ctx)
				seen <- p
				return h(ctx, req)
			}))
		echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
		t.Cleanup(srv.Stop)

		addr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 4242}
		x.NoError(t, srv.Handle(drpc.NewPeerContext(context.Background(), addr), lcOnceOpen(0x11, 1, "abc")))

		p := cpRecv(t, seen, "the handler's peer")
		x.True(t, p != nil, "an address peer key must surface as a gRPC peer")
		x.Equal(t, addr.String(), p.Addr.String())
		x.Equal(t, addr.Network(), p.Addr.Network(),
			"the key IS an address: the handler must see it, not an opaque wrapper")
	})
}

// ---------------------------------------------------------------------------
// §11, §15 — PerRPCCredentials
// ---------------------------------------------------------------------------

// cpCreds is a PerRPCCredentials that records the audiences it was asked for.
type cpCreds struct {
	md     map[string]string
	secure bool

	mu   sync.Mutex
	uris []string
}

func (c *cpCreds) GetRequestMetadata(_ context.Context, uri ...string) (map[string]string, error) {
	c.mu.Lock()
	c.uris = append(c.uris, uri...)
	c.mu.Unlock()
	return c.md, nil
}

func (c *cpCreds) RequireTransportSecurity() bool { return c.secure }

func (c *cpCreds) audiences() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.uris...)
}

// TestCompat_PerRPCCredentials pins PROTOCOL.md §15 and §11: credentials are a
// metadata producer whose output rides the OPEN like any request header — drpc
// authenticates nothing itself. Credentials that demand transport security are
// refused with Unauthenticated, because drpc cannot attest a channel it does
// not own; WithAssumeTransportSecurity is the explicit, documented override.
func TestCompat_PerRPCCredentials(t *testing.T) {
	t.Run("metadata rides the OPEN", func(t *testing.T) {
		creds := &cpCreds{md: map[string]string{"Authorization": "Bearer t0ken"}}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{
				drpc.WithPerRPCCredentials(creds),
				drpc.WithAuthority("compat.test"),
			},
		}.Use(t)
		defer stop()

		md := metadata.Pairs("foo", "bar")
		_, err := client.Once(metadata.NewOutgoingContext(t.Context(), md),
			echo.EchoRequest_builder{Message: "abc", CircularShift: 1}.Build())
		x.NoError(t, err)

		// The handler saw the credential metadata alongside the caller's own,
		// with the key lowercased the way gRPC keys always are (§11).
		x.Equal(t, metadata.Pairs("foo", "bar", "authorization", "Bearer t0ken"), client.service.MD)

		// And it rode the OPEN, the only frame request MD travels on (§11).
		open := cpOpen(t, cpTxFrames(client))
		x.Equal(t, []string{"Bearer t0ken"}, open.GetHeader().MD()["authorization"])

		// The audience is grpc-go's createAudience string verbatim, scheme
		// included: providers mint their token's "aud" claim from it, so a
		// drpc-specific scheme would produce tokens no server accepts.
		x.Equal(t, []string{"https://compat.test/echo.EchoService"}, creds.audiences())
	})

	t.Run("per-call credentials", func(t *testing.T) {
		creds := &cpCreds{md: map[string]string{"authorization": "call-scoped"}}
		client, stop := PipeOption{}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(),
			echo.EchoRequest_builder{Message: "abc", CircularShift: 1}.Build(),
			grpc.PerRPCCredentials(creds))
		x.NoError(t, err)
		x.Equal(t, metadata.Pairs("authorization", "call-scoped"), client.service.MD)
	})

	t.Run("transport security is refused, not assumed", func(t *testing.T) {
		creds := &cpCreds{md: map[string]string{"authorization": "secret"}, secure: true}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{drpc.WithPerRPCCredentials(creds)},
		}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: "abc"}.Build())
		x.Equal(t, codes.Unauthenticated, status.Code(err))
		x.True(t, strings.Contains(status.Convert(err).Message(), "WithAssumeTransportSecurity"),
			"the refusal must name its own override: ", status.Convert(err).Message())

		// The call failed before it existed: no OPEN, and the credentials were
		// never even asked for their secret.
		x.Len(t, cpTxFrames(client), 0)
		x.Len(t, creds.audiences(), 0)
	})

	t.Run("WithAssumeTransportSecurity admits them", func(t *testing.T) {
		creds := &cpCreds{md: map[string]string{"authorization": "secret"}, secure: true}
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{
				drpc.WithPerRPCCredentials(creds),
				drpc.WithAssumeTransportSecurity(),
			},
		}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(),
			echo.EchoRequest_builder{Message: "abc", CircularShift: 1}.Build())
		x.NoError(t, err)
		x.Equal(t, metadata.Pairs("authorization", "secret"), client.service.MD)
	})
}

// ---------------------------------------------------------------------------
// §12 — grpc.CallContentSubtype
// ---------------------------------------------------------------------------

const cpCodecName = "compat-json"

// cpCountingCodec is a registered codec that counts its use, so a test can
// prove the codec the OPEN named is the one both ends actually ran (§12).
type cpCountingCodec struct {
	x.JsonCodecV2
	marshals   atomic.Int64
	unmarshals atomic.Int64
}

func (c *cpCountingCodec) Name() string { return cpCodecName }

func (c *cpCountingCodec) Marshal(v any) (mem.BufferSlice, error) {
	c.marshals.Add(1)
	return c.JsonCodecV2.Marshal(v)
}

func (c *cpCountingCodec) Unmarshal(data mem.BufferSlice, v any) error {
	c.unmarshals.Add(1)
	return c.JsonCodecV2.Unmarshal(data, v)
}

var cpCodec = &cpCountingCodec{}

func init() { encoding.RegisterCodecV2(cpCodec) }

// TestCompat_CallContentSubtype pins PROTOCOL.md §12: the codec is named on
// the OPEN and governs the whole call in both directions. grpc.CallContentSubtype
// selects a codec from the process registry — the same lookup grpc-go does —
// and an unregistered name fails the call locally rather than on the wire.
func TestCompat_CallContentSubtype(t *testing.T) {
	t.Run("a registered subtype reaches the server", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		marshals := cpCodec.marshals.Load()
		unmarshals := cpCodec.unmarshals.Load()

		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build(), grpc.CallContentSubtype(cpCodecName))
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		// Named on the OPEN, and only there (§12).
		open := cpOpen(t, cpTxFrames(client))
		x.Equal(t, cpCodecName, open.GetCodec())

		// Both directions ran through it: the request the server decoded and
		// the response it produced are both this codec's JSON.
		req := &echo.EchoRequest{}
		x.NoError(t, protojson.Unmarshal(open.GetPayload(), req))
		x.Equal(t, "abc", req.GetMessage())

		term := cpTerminal(t, client.rxFrames())
		out := &echo.EchoResponse{}
		x.NoError(t, protojson.Unmarshal(term.GetPayload(), out))
		x.Equal(t, "bca", out.GetMessage())

		x.True(t, cpCodec.marshals.Load() > marshals, "the registered codec must have marshaled")
		x.True(t, cpCodec.unmarshals.Load() > unmarshals, "the registered codec must have unmarshaled")
	})

	t.Run("an unregistered subtype fails the call locally", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		_, err := client.Once(t.Context(), echo.EchoRequest_builder{Message: "abc"}.Build(),
			grpc.CallContentSubtype("no-such-codec"))
		x.Equal(t, codes.Internal, status.Code(err))
		x.Len(t, cpTxFrames(client), 0, "an unresolvable codec must never reach the wire")
	})
}

// ---------------------------------------------------------------------------
// §13 — Server.GetServiceInfo
// ---------------------------------------------------------------------------

// TestCompat_GetServiceInfo pins the registry view grpc.Server exposes: every
// registered method under its service, with the streaming flags that made it a
// stream — what google.golang.org/grpc/reflection and health tooling read
// (PROTOCOL.md §13: methods are addressed by name, always).
func TestCompat_GetServiceInfo(t *testing.T) {
	srv := drpc.NewServer(drpc.FrameHandlerFunc(func(context.Context, *drpc.Frame) error { return nil }))
	x.Equal(t, 0, len(srv.GetServiceInfo()), "a bare server registers nothing")

	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
	t.Cleanup(srv.Stop)

	info := srv.GetServiceInfo()
	x.Equal(t, 1, len(info))
	svc, ok := info["echo.EchoService"]
	x.True(t, ok, "the service must be registered under its proto name")
	x.Equal(t, "echo/echo.proto", svc.Metadata)

	methods := map[string]grpc.MethodInfo{}
	for _, m := range svc.Methods {
		methods[m.Name] = m
	}
	x.Equal(t, map[string]grpc.MethodInfo{
		"Noop": {Name: "Noop"},
		"Once": {Name: "Once"},
		"Many": {Name: "Many", IsServerStream: true},
		"Buff": {Name: "Buff", IsClientStream: true},
		"Live": {Name: "Live", IsClientStream: true, IsServerStream: true},
	}, methods)
}

// ---------------------------------------------------------------------------
// §8, §11 — SendHeader
// ---------------------------------------------------------------------------

// TestCompat_SendHeaderTwice pins PROTOCOL.md §11: a header may be flushed
// once. The second SendHeader — and a SetHeader after the flush — is INTERNAL,
// grpc-go's ErrIllegalHeaderWrite, and the losing metadata never reaches the
// client. The streaming case additionally pins that the core's own creation
// ack (§8) is NOT a flush: the handler's first SendHeader must still succeed.
func TestCompat_SendHeaderTwice(t *testing.T) {
	first := metadata.Pairs("flush", "first")
	second := metadata.Pairs("flush", "second")
	late := metadata.Pairs("late", "set")

	t.Run("unary", func(t *testing.T) {
		errs := make(chan error, 8)
		client, stop := PipeOption{
			ServerOpts: []drpc.ServerOption{
				drpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
					errs <- grpc.SendHeader(ctx, first)
					errs <- grpc.SendHeader(ctx, second)
					errs <- grpc.SetHeader(ctx, late)
					return h(ctx, req)
				}),
			},
		}.Use(t)
		defer stop()

		header := metadata.MD{}
		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build(), grpc.Header(&header))
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		x.NoError(t, cpRecv(t, errs, "the first SendHeader"))
		x.Equal(t, codes.Internal, status.Code(cpRecv(t, errs, "the second SendHeader")))
		x.Equal(t, codes.Internal, status.Code(cpRecv(t, errs, "the post-flush SetHeader")))
		x.Equal(t, first, header, "only the flushed metadata may reach the client")
	})

	t.Run("streaming: the creation ack is not a flush", func(t *testing.T) {
		errs := make(chan error, 8)
		client, stop := PipeOption{
			ServerOpts: []drpc.ServerOption{
				drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
					// The core already sent the creation-ack H for this call
					// (§8) — if that counted as a flush, this would fail.
					errs <- ss.SendHeader(first)
					errs <- ss.SendHeader(second)
					errs <- ss.SetHeader(late)
					return h(srv, ss)
				}),
			},
		}.Use(t)
		defer stop()

		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        1,
		}.Build())
		x.NoError(t, err)
		_, err = stream.Recv()
		x.NoError(t, err)
		_, err = stream.Recv()
		x.ErrorIs(t, err, io.EOF)

		x.NoError(t, cpRecv(t, errs, "the first SendHeader"))
		x.Equal(t, codes.Internal, status.Code(cpRecv(t, errs, "the second SendHeader")))
		x.Equal(t, codes.Internal, status.Code(cpRecv(t, errs, "the post-flush SetHeader")))

		header, err := stream.Header()
		x.NoError(t, err)
		x.Equal(t, first, header)

		// On the wire: the ack H carries no header field (§8 — an ack must not
		// pin the header to nil), the flush H that follows carries it.
		var hs []*drpc.Frame
		for _, f := range client.rxFrames() {
			if cpIsHeaderFrame(f) {
				hs = append(hs, f)
			}
		}
		x.True(t, len(hs) >= 2, "expected the creation ack and the flushed H")
		x.False(t, hs[0].HasHeader(), "the creation ack must not carry a header")
		x.True(t, hs[1].HasHeader(), "the flush must carry one")
		x.Equal(t, first, hs[1].GetHeader().MD())
	})
}

// TestCompat_UnarySendHeaderReleasesHeaderEarly pins PROTOCOL.md §11's v1.1
// rule: on a unary call SendHeader flushes an H at once, so a client blocked
// in Header() is released BEFORE the response exists — gRPC's behavior, which
// its separate HEADERS frame gives for free. The handler here is still parked
// when Header() returns, so nothing but the flush could have released it.
func TestCompat_UnarySendHeaderReleasesHeaderEarly(t *testing.T) {
	release := make(chan struct{})
	want := metadata.Pairs("early", "yes")
	client, stop := PipeOption{
		ServerOpts: []drpc.ServerOption{
			drpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
				if err := grpc.SendHeader(ctx, want); err != nil {
					return nil, err
				}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return h(ctx, req)
			}),
		},
	}.Use(t)
	defer stop()

	// The generated unary stub cannot observe a header mid-call, so drive the
	// unary shape (§8) through the ClientStream API directly.
	cs, err := client.conn.NewStream(t.Context(), &grpc.StreamDesc{StreamName: "Once"},
		echo.EchoService_Once_FullMethodName)
	x.NoError(t, err)
	x.NoError(t, cs.SendMsg(echo.EchoRequest_builder{Message: "abc", CircularShift: 1}.Build()))

	type hdr struct {
		md  metadata.MD
		err error
	}
	got := make(chan hdr, 1)
	go func() {
		md, err := cs.Header()
		got <- hdr{md, err}
	}()

	h := cpRecv(t, got, "Header() while the handler is still parked")
	x.NoError(t, h.err)
	x.Equal(t, want, h.md)

	close(release)
	res := &echo.EchoResponse{}
	x.NoError(t, cs.RecvMsg(res))
	x.Equal(t, "bca", res.GetMessage())
}

// TestCompat_HeaderOnCallerCancel pins PROTOCOL.md §11: Header() returns the
// latched MD or nothing — never the call's status, and never a context error.
// A caller-ctx cancellation races the abort path that closes both of the
// channels Header() waits on, so the outcome must not depend on which one the
// scheduler observes first; the loop makes the race real.
func TestCompat_HeaderOnCallerCancel(t *testing.T) {
	client, stop := PipeOption{}.Use(t)
	defer stop()

	for i := range 30 {
		ctx, cancel := context.WithCancel(t.Context())
		stream, err := client.Live(ctx)
		x.NoError(t, err)
		if i%2 == 0 {
			// Vary how far the call gets before the cancellation lands.
			_ = stream.Send(echo.EchoRequest_builder{Message: "m"}.Build())
		}
		cancel()

		md, err := stream.Header()
		x.NoError(t, err, "Header() must never surface the cancellation")
		x.True(t, md == nil, "a call that ended without a header latches nothing")

		// The status is RecvMsg's to deliver, exactly as in grpc-go.
		_, err = stream.Recv()
		x.Equal(t, codes.Canceled, status.Code(err))
	}
}

// ---------------------------------------------------------------------------
// §11 — metadata representation and validation
// ---------------------------------------------------------------------------

// TestCompat_BinaryMetadata pins PROTOCOL.md §11 and §5: metadata values are
// bytes on the wire, so a "-bin" key carries arbitrary octets — NUL bytes and
// invalid UTF-8 included — verbatim, in both directions. This is exactly what
// a proto string field could not hold, and why the v1.1 wire uses bytes.
func TestCompat_BinaryMetadata(t *testing.T) {
	client, stop := PipeOption{}.Use(t)
	defer stop()

	raw := string([]byte{0x00, 0x01, 0xff, 0xfe, 0x80, 'z', 0x7f})
	md := metadata.MD{
		"trace-bin": []string{raw, ""},
		"plain":     []string{"printable"},
	}

	header := metadata.MD{}
	trailer := metadata.MD{}
	res, err := client.Once(metadata.NewOutgoingContext(t.Context(), md), echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
	}.Build(), grpc.Header(&header), grpc.Trailer(&trailer))
	x.NoError(t, err)
	x.Equal(t, "bca", res.GetMessage())

	// c -> s: the handler sees the octets it was sent, including the empty
	// value (a zero-length value is a present value, §5).
	x.Equal(t, []string{raw, ""}, client.service.MD["trace-bin"])

	// ...and they were raw bytes on the wire: no base64, no UTF-8 coercion.
	open := cpOpen(t, cpTxFrames(client))
	x.Equal(t, [][]byte{[]byte(raw), {}}, open.GetHeader().GetEntries()["trace-bin"].GetValues())

	// s -> c: the echo handler mirrors the MD into header and trailer, so the
	// octets survive the return trip too (§11).
	x.Equal(t, []string{raw, ""}, header["trace-bin"])
	x.Equal(t, []string{raw, ""}, trailer["trace-bin"])
}

// TestCompat_MetadataValidation pins PROTOCOL.md §11's validation, which
// mirrors grpc-go's: an illegal key or a non-printable value in a text key is
// the sender's own bug and fails the call locally with INTERNAL, naming the
// key — never as a marshal failure deep inside an adapter, and never on the
// wire.
func TestCompat_MetadataValidation(t *testing.T) {
	bad := []struct {
		name string
		md   metadata.MD
		want string
	}{
		{"illegal key", metadata.MD{"bad key": []string{"v"}}, "illegal characters"},
		{"empty key", metadata.MD{"": []string{"v"}}, "empty key"},
		{"non-printable text value", metadata.MD{"text": []string{"a\x00b"}}, "non-printable"},
		{"high-bit text value", metadata.MD{"text": []string{"caf\xc3\xa9"}}, "non-printable"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			client, stop := PipeOption{}.Use(t)
			defer stop()

			ctx := metadata.NewOutgoingContext(t.Context(), tc.md)
			_, err := client.Once(ctx, echo.EchoRequest_builder{Message: "abc"}.Build())
			x.Equal(t, codes.Internal, status.Code(err))
			x.True(t, strings.Contains(status.Convert(err).Message(), tc.want),
				"the failure must name what is wrong: ", status.Convert(err).Message())
			x.Len(t, cpTxFrames(client), 0, "the call must never reach the wire")

			// Streaming calls are validated on the same path (§11: request MD
			// rides the OPEN, so it is checked before the call exists).
			_, err = client.Live(ctx)
			x.Equal(t, codes.Internal, status.Code(err))
			x.Len(t, cpTxFrames(client), 0)
		})
	}

	t.Run("the same octets under a -bin key are legal", func(t *testing.T) {
		client, stop := PipeOption{}.Use(t)
		defer stop()

		ctx := metadata.NewOutgoingContext(t.Context(),
			metadata.MD{"text-bin": []string{"a\x00b\xc3\xa9"}})
		_, err := client.Once(ctx, echo.EchoRequest_builder{Message: "abc"}.Build())
		x.NoError(t, err, "-bin values are unvalidated (§11)")
	})

	t.Run("an upper-case key is normalized, not rejected", func(t *testing.T) {
		// grpc-go lower-cases outgoing keys in FromOutgoingContext, so the
		// validation of §11 never sees the upper-case form — rejecting it here
		// would fail calls grpc-go accepts.
		client, stop := PipeOption{}.Use(t)
		defer stop()

		ctx := metadata.NewOutgoingContext(t.Context(), metadata.MD{"Mixed-Case": []string{"v"}})
		_, err := client.Once(ctx, echo.EchoRequest_builder{Message: "abc"}.Build())
		x.NoError(t, err)
		x.Equal(t, []string{"v"}, client.service.MD["mixed-case"])
	})
}
