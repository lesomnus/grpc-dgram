// Command udpserver is a cross-language conformance fixture: it serves the
// echo service over the drpc UDP transport so a TypeScript client can drive a
// real Go drpc.Server across the wire. It binds two ephemeral 127.0.0.1 UDP
// ports and announces both on stdout, one per line:
//
//	PORT <n>           the unreliable endpoint — drpc's default mode
//	PORT_RELIABLE <n>  the same service, served in RELIABLE mode
//
// then runs until stdin closes (the parent test process going away) — see
// ts/test/conformance.test.ts.
//
// Why two endpoints: half of wire v1.1 is reliable-mode only (per-stream flow
// control, PROTOCOL.md §4.2.1 — the `window` field and the WINDOW flag), and
// the unreliable endpoint must be able to prove it advertises NO window. UDP
// is of course not a reliable channel; the second endpoint overrides the
// gateway's per-frame mode annotation (drpc.NewReliableContext) so the core
// runs its reliable-mode machinery over loopback, where loss and reordering
// are not a practical concern for the handful of small datagrams the suite
// sends. Nothing else about that endpoint differs.
//
// Service behaviour beyond internal/echo lives in conformanceServer: a request
// whose message is "conf/<directive>" selects one of the v1.1 surfaces (binary
// metadata, status details, an eagerly flushed header); anything else falls
// through to the plain echo handler the older cases use.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/udp"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // registers "gzip", the §12.1 interop baseline
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ---------------------------------------------------------------------------
// the exact bytes the two implementations pin, independently
// ---------------------------------------------------------------------------
//
// Metadata values are `repeated bytes` on the wire (v1.1). grpc-go keeps the
// octets of a "-bin" value inside a string; the TS port cannot (a JS string
// holds no arbitrary octets) and keeps their base64 instead. Both stacks
// therefore put IDENTICAL bytes on the wire from DIFFERENT local
// representations — which is exactly the divergence a round-trip assertion
// cannot see: if one side base64'd onto the wire and the other base64-decoded
// off it, an echo would still match itself.
//
// So nothing below round-trips. The server hard-codes what it expects to
// receive and fails the call when it differs, and hard-codes what it sends;
// the TS test hard-codes the same octets on its own side. Either
// implementation getting the boundary wrong breaks the call.
var (
	// Deliberately not valid UTF-8: 0xff/0xfe never occur in UTF-8 at all and
	// 0x80/0xc0 are a lone continuation byte and a truncated lead byte. A
	// lossy UTF-8 decode replaces each with U+FFFD and changes the length, so
	// a peer that treated a "-bin" value as text cannot reproduce these.
	hdrBinValue = []byte{0x00, 0xff, 0xfe, 0x80, 0xc0, 'd', 'r', 'p', 'c', 0x7f, 0x0a}
	trlBinValue = []byte{0xff, 0xd8, 0x00, 0x1b, 0x80, 't', 'r', 'l', 'r', 0xfe, 0x00}
	// reqBinValue is what the TS client must put on the wire for keyReqBin.
	reqBinValue = []byte{0x00, 0x01, 0x02, 0x80, 0xfe, 0xff, 'r', 'e', 'q', 0xc2, 0x00}
)

const (
	// Printable ASCII (0x20..0x7E) only: that is all a non-"-bin" value may
	// hold, and both stacks reject the rest with INTERNAL before the call
	// starts (§11). Round-tripping these unchanged is what proves a text key
	// is NOT base64'd anywhere.
	hdrTextValue = `conformance header !"#$%&'()*+,-./09:;<=>?@AZ[\]^_az{|}~ `
	trlTextValue = `conformance trailer 0x20..0x7E`
	reqTextValue = `conformance request !"#$%&'()*+,-./09:;<=>?@AZ[\]^_az{|}~ `
)

const (
	keyReqBin  = "x-conf-req-bin"  // TS -> Go, binary
	keyReqText = "x-conf-req-text" // TS -> Go, text
	keyHdrBin  = "x-conf-hdr-bin"  // Go -> TS, binary, on the header
	keyHdrText = "x-conf-hdr-text" // Go -> TS, text, on the header
	keyEchoBin = "x-conf-echo-bin" // Go -> TS, the received binary value verbatim
	keyTrlBin  = "x-conf-trl-bin"  // Go -> TS, binary, on the trailer
	keyTrlText = "x-conf-trl-text" // Go -> TS, text, on the trailer
	keyPhase   = "x-conf-phase"    // Go -> TS, marks the eagerly flushed header
)

// The google.rpc.ErrorInfo the details case attaches; the TS test pins the
// marshaled Any byte for byte, so these strings are load-bearing.
const (
	errorInfoReason = "CONFORMANCE"
	errorInfoDomain = "drpc.conformance"
)

// slowHeaderDelay is how long the "slow-header" case sits between flushing its
// header and producing the response, so a client can prove Header() returned
// first (§8, §11: a unary SendHeader flushes an H frame at once).
const slowHeaderDelay = 300 * time.Millisecond

// directivePrefix marks a conformance request; see conformanceServer.
const directivePrefix = "conf/"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "udpserver:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The unreliable endpoint: drpc's default mode, and the one every
	// pre-v1.1 case runs on.
	unreliable, err := listen()
	if err != nil {
		return err
	}
	gw := udp.NewGateway(unreliable)
	srv := drpc.NewServer(gw)
	// Registration must precede the first received frame (§13); done here,
	// before Serve starts.
	echo.RegisterEchoServiceServer(srv, &conformanceServer{EchoServer: &echo.EchoServer{}})
	go func() { _ = gw.Serve(ctx, srv) }()

	// The reliable endpoint. The gateway annotates every frame it delivers as
	// unreliable (udp.Gateway.Serve) and that per-frame annotation wins over
	// the server's own mode — so the annotation is what has to be replaced,
	// frame by frame. WithReliable keeps the server-wide default in step for
	// the state that is not decided per frame.
	reliable, err := listen()
	if err != nil {
		return err
	}
	relGw := udp.NewGateway(reliable)
	relSrv := drpc.NewServer(relGw, drpc.WithReliable(true))
	echo.RegisterEchoServiceServer(relSrv, &conformanceServer{EchoServer: &echo.EchoServer{}})
	asReliable := drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		return relSrv.Handle(drpc.NewReliableContext(ctx, true), f)
	})
	go func() { _ = relGw.Serve(ctx, asReliable) }()

	fmt.Printf("PORT %d\n", port(unreliable))
	fmt.Printf("PORT_RELIABLE %d\n", port(reliable))
	if f, ok := any(os.Stdout).(interface{ Sync() error }); ok {
		_ = f.Sync()
	}

	// Block until the parent closes our stdin, then tear down cleanly.
	_, _ = io.Copy(io.Discard, os.Stdin)
	cancel()
	_ = unreliable.Close()
	_ = reliable.Close()
	srv.Stop()
	relSrv.Stop()
	return nil
}

func listen() (*net.UDPConn, error) {
	return net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
}

func port(c *net.UDPConn) int {
	return c.LocalAddr().(*net.UDPAddr).Port
}

// ---------------------------------------------------------------------------
// the service
// ---------------------------------------------------------------------------

// conformanceServer serves internal/echo unchanged, plus the wire v1.1
// surfaces the cross-language suite pins. A request selects one with the
// message "conf/<directive>"; every other message reaches the plain echo
// handler, so the older cases (CircularShift, sequences, status codes,
// timestamps, streaming shapes) behave exactly as before.
type conformanceServer struct {
	*echo.EchoServer
}

func (s *conformanceServer) Once(ctx context.Context, req *echo.EchoRequest) (*echo.EchoResponse, error) {
	d, ok := strings.CutPrefix(req.GetMessage(), directivePrefix)
	if !ok {
		return s.EchoServer.Once(ctx, req)
	}
	switch d {
	case "md":
		return metadataCase(ctx)
	case "details":
		return nil, detailsErr(req)
	case "slow-header":
		return slowHeaderCase(ctx)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "conformance: unknown directive %q", d)
	}
}

// metadataCase is the binary-metadata contract (§11, wire v1.1): it verifies
// the octets the client sent against the hard-coded expectations above, then
// answers with header and trailer metadata whose bytes the client verifies the
// same way. keyEchoBin additionally returns the received value verbatim, which
// pins that the octets survived the server's own wire -> MD -> wire round.
func metadataCase(ctx context.Context) (*echo.EchoResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "conformance: request carried no metadata")
	}
	if err := wantValue(md, keyReqBin, string(reqBinValue)); err != nil {
		return nil, err
	}
	if err := wantValue(md, keyReqText, reqTextValue); err != nil {
		return nil, err
	}

	stream := grpc.ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return nil, status.Error(codes.Internal, "conformance: no server transport stream")
	}
	// SendHeader, not SetHeader: the header is flushed as its own H frame even
	// on this unary call (§8, §11).
	if err := stream.SendHeader(metadata.MD{
		keyHdrBin:  []string{string(hdrBinValue)},
		keyHdrText: []string{hdrTextValue},
		keyEchoBin: md.Get(keyReqBin),
	}); err != nil {
		return nil, err
	}
	if err := stream.SetTrailer(metadata.MD{
		keyTrlBin:  []string{string(trlBinValue)},
		keyTrlText: []string{trlTextValue},
	}); err != nil {
		return nil, err
	}
	return echo.EchoResponse_builder{
		Message:     "md-ok",
		DateCreated: timestamppb.Now(),
	}.Build(), nil
}

// wantValue fails the call unless md carries exactly one value for key and it
// is byte-identical to want. The message reports both sides in hex so a
// mismatch names the bytes rather than "metadata differed" — the two stacks
// hold these values in different representations, and hex is the only form
// that reads the same on both sides of that boundary.
func wantValue(md metadata.MD, key, want string) error {
	got := md.Get(key)
	if len(got) == 1 && got[0] == want {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition,
		"conformance: %s: want 1 value %x (%d bytes), got %d value(s) %x",
		key, want, len(want), len(got), got)
}

// detailsErr is the rich-status contract (§5): a non-OK status carrying
// google.rpc.Status.details, which ride the terminal frame as repeated
// google.protobuf.Any.
//
// Two details, of two kinds: a google.rpc.ErrorInfo (a type the TS side has no
// schema for, so it pins the marshaled Any bytes exactly) and the request
// message itself (a type the TS side DOES have, so it decodes it and reads the
// fields back). The order is significant and is pinned.
func detailsErr(req *echo.EchoRequest) error {
	st, err := status.New(codes.FailedPrecondition, "conformance: rich status details").WithDetails(
		&errdetails.ErrorInfo{Reason: errorInfoReason, Domain: errorInfoDomain},
		req,
	)
	if err != nil {
		return status.Errorf(codes.Internal, "conformance: attaching details: %v", err)
	}
	return st.Err()
}

// slowHeaderCase flushes the header, then stalls before answering: a client's
// Header() must return while the response is still being produced, which is
// what a unary SendHeader flushing its own H frame buys (§8, §11). The second
// flush must be refused, as in grpc-go — reporting that here keeps the rule
// pinned cross-language instead of only inside the Go suite.
func slowHeaderCase(ctx context.Context) (*echo.EchoResponse, error) {
	stream := grpc.ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return nil, status.Error(codes.Internal, "conformance: no server transport stream")
	}
	if err := stream.SendHeader(metadata.MD{keyPhase: []string{"header"}}); err != nil {
		return nil, err
	}
	if err := stream.SendHeader(metadata.MD{keyPhase: []string{"again"}}); err == nil {
		return nil, status.Error(codes.Internal, "conformance: a second SendHeader was accepted")
	}
	select {
	case <-time.After(slowHeaderDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return echo.EchoResponse_builder{
		Message:     "late",
		DateCreated: timestamppb.Now(),
	}.Build(), nil
}
