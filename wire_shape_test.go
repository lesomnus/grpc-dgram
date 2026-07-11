package drpc_test

// wire_shape_test.go pins the on-wire shape rules two implementations (the Go
// core and the planned TS port) must agree on: envelop framing and in-order
// processing (PROTOCOL.md §4.1), the exact Frame/Envelop encoding (§5), codec
// addressing (§12), the registration freeze (§13), and the rx drop policies
// (§4.2).

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// liveOpen builds the eager OPEN of a bidi call (PROTOCOL.md §8): OPEN flag
// only, no payload, seq 1.
func liveOpen(epoch, sid uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(1)
	f.SetFlags(drpc.FlagOpen)
	f.SetMethod(echo.EchoService_Live_FullMethodName)
	return f
}

// echoData builds a client data frame carrying one EchoRequest.
func echoData(t *testing.T, epoch, sid, seq uint32, msg string) *drpc.Frame {
	t.Helper()
	payload, err := proto.Marshal(echo.EchoRequest_builder{
		Message:       msg,
		Repeat:        1,
		CircularShift: 1,
	}.Build())
	x.NoError(t, err)
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	f.SetPayload(payload)
	return f
}

// drainFor collects every frame the server emits within d.
func drainFor(is *injectServer, d time.Duration) []*drpc.Frame {
	var out []*drpc.Frame
	deadline := time.After(d)
	for {
		select {
		case f := <-is.out:
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
}

// ---------------------------------------------------------------------------
// §4.1: one envelop, many frames — receivers process them in order, and a
// frame later in the envelop lands on the call created earlier in the SAME
// envelop (mid-envelop creation).
// ---------------------------------------------------------------------------

func TestWireShape_MultiFrameEnvelop(t *testing.T) {
	is := newInjectServerMode(t, false) // unreliable: the datagram framing mode

	const cEpoch, sid = uint32(1), uint32(11)
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{
		liveOpen(cEpoch, sid),
		echoData(t, cEpoch, sid, 2, "abc"),
	})
	x.NoError(t, drpc.Unpack(context.Background(), e, is.srv))

	// The OPEN took effect: the creation ack H (flags 0, no payload, §8) is
	// emitted synchronously at call creation, naming the client incarnation.
	h := is.recv(t)
	x.True(t, h != nil, "expected the creation ack H")
	x.Equal(t, uint32(0), h.GetFlags())
	x.False(t, h.HasPayload(), "H carries no payload (§7)")
	x.Equal(t, sid, h.GetSid())
	x.Equal(t, cEpoch, h.GetPeerEpoch(), "server frames echo the client epoch (§6.1)")

	// The data frame took effect TOO: it routed into the call created
	// mid-envelop, and the Live handler echoed it back.
	res := is.recv(t)
	x.True(t, res != nil, "expected the echoed data frame")
	x.Equal(t, uint32(0), res.GetFlags())
	x.True(t, res.HasPayload(), "expected a data frame")
	got := &echo.EchoResponse{}
	x.NoError(t, proto.Unmarshal(res.GetPayload(), got))
	x.Equal(t, "bca", got.GetMessage())

	t.Run("empty envelop is a no-op", func(t *testing.T) {
		// §4.1: an empty envelop is dropped — no panic, no reply.
		x.NoError(t, drpc.Unpack(context.Background(), &drpc.Envelop{}, is.srv))
		x.True(t, is.recv(t) == nil, "an empty envelop must draw no reply")
	})
}

// ---------------------------------------------------------------------------
// §4.1 + §9.3: frames are processed in the order they appear in the envelop.
// [data(seq2), OPEN(seq1)] — the data frame precedes creation, so it is
// dropped (delayed-RESET path; its loss is within the §14 contract) and the
// OPEN then creates the call. The pending RESET is cancelled by the OPEN, so
// no RESET ever fires.
// ---------------------------------------------------------------------------

func TestWireShape_EnvelopInOrderProcessing(t *testing.T) {
	// Short T_hold so a leaked delayed RESET would fire inside the assertion
	// window; every other timer keeps its (long) default.
	is := newInjectServerMode(t, false, drpc.WithTiming(drpc.Timing{Hold: 50 * time.Millisecond}))

	const cEpoch, sid = uint32(1), uint32(12)
	e := &drpc.Envelop{}
	e.SetFrames([]*drpc.Frame{
		echoData(t, cEpoch, sid, 2, "early"), // before its OPEN: dropped, held
		liveOpen(cEpoch, sid),
	})
	x.NoError(t, drpc.Unpack(context.Background(), e, is.srv))

	// The OPEN created the call: creation ack H arrives (§8). No crash from
	// the mis-ordered data frame.
	h := is.recv(t)
	x.True(t, h != nil, "the OPEN later in the envelop must still create the call")
	x.Equal(t, uint32(0), h.GetFlags())
	x.False(t, h.HasPayload())
	x.Equal(t, cEpoch, h.GetPeerEpoch())

	// The call is live: a fresh data frame is delivered and echoed.
	is.handle(echoData(t, cEpoch, sid, 2, "xyz"))
	res := is.recv(t)
	x.True(t, res != nil && res.HasPayload(), "the created call must be usable")
	got := &echo.EchoResponse{}
	x.NoError(t, proto.Unmarshal(res.GetPayload(), got))
	x.Equal(t, "yzx", got.GetMessage())

	// The delayed RESET scheduled for the early data frame was cancelled by
	// the OPEN (§9.3): well past T_hold, no RESET is emitted.
	for _, f := range drainFor(is, 300*time.Millisecond) {
		x.True(t, f.GetFlags()&drpc.FlagReset == 0,
			"no RESET may fire once the OPEN admitted the call, got ", f)
	}
}

// ---------------------------------------------------------------------------
// §5: the exact wire bytes. These vectors are the cross-implementation
// contract — a TS port must produce and accept byte-identical encodings.
// Protobuf marshals scalar fields in field-number order, so the encoding is
// deterministic (verified below by double-marshal).
// ---------------------------------------------------------------------------

// golden vector for cross-implementation agreement (TS port).
// Frame{epoch:0x01020304 sid:5 seq:6 flags:OPEN|CLOSE method:"/a.B/C"
// codec:"json" timeout:1.5s payload:[0xAA] code:0(present) desc:"d"
// peer_epoch:0x0A0B0C0D} — header/trailer absent to keep the vector stable.
// Layout (§5): 0d=epoch(1,fixed32) 15=sid(2,fixed32) 1d=seq(3,fixed32)
// 20=flags(4,varint) 2a=method(5,len) 3a=codec(7,len) 42=timeout(8,msg)
// 4a=payload(9,len) 50=code(10,varint,explicit) 5a=desc(11,len)
// 75=peer_epoch(14,fixed32).
const goldenFrameHex = "0d0403020115050000001d0600000020032a062f612e422f433a046a736f6e420808011080cab5ee014a01aa50005a0164750d0c0b0a"

// golden vector for cross-implementation agreement (TS port).
// Envelop{frames:[OPEN{epoch:1 sid:2 seq:1 flags:1 method:"/a.B/C"},
// data{epoch:1 sid:2 seq:2 payload:[0xAA]}]} — frames is field 1 (§4.1, §5;
// the old frames=8 / payload=8 collision is gone, Appendix A).
const goldenEnvelopHex = "0a190d0100000015020000001d0100000020012a062f612e422f430a120d0100000015020000001d020000004a01aa"

func goldenFrame() *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(0x01020304)
	f.SetSid(5)
	f.SetSeq(6)
	f.SetFlags(drpc.FlagOpen | drpc.FlagClose)
	f.SetMethod("/a.B/C")
	f.SetCodec("json")
	f.SetTimeout(durationpb.New(1500 * time.Millisecond))
	f.SetPayload([]byte{0xAA})
	f.SetCode(0) // presence is load-bearing: terminal CLOSE vs half-close (§5)
	f.SetDesc("d")
	f.SetPeerEpoch(0x0A0B0C0D)
	return f
}

func TestWireShape_GoldenBytes(t *testing.T) {
	t.Run("Frame", func(t *testing.T) {
		f := goldenFrame()
		a, err := proto.Marshal(f)
		x.NoError(t, err)
		b, err := proto.Marshal(f)
		x.NoError(t, err)
		x.Equal(t, hex.EncodeToString(a), hex.EncodeToString(b), "marshal must be deterministic")

		if got := hex.EncodeToString(a); got != goldenFrameHex {
			t.Fatalf("Frame wire bytes drifted (field number / wire type change?):\n got  %s\n want %s", got, goldenFrameHex)
		}

		// The golden bytes round-trip every field.
		raw, err := hex.DecodeString(goldenFrameHex)
		x.NoError(t, err)
		g := &drpc.Frame{}
		x.NoError(t, proto.Unmarshal(raw, g))
		x.Equal(t, 0x01020304, g.GetEpoch())
		x.Equal(t, 5, g.GetSid())
		x.Equal(t, 6, g.GetSeq())
		x.Equal(t, drpc.FlagOpen|drpc.FlagClose, g.GetFlags())
		x.Equal(t, "/a.B/C", g.GetMethod())
		x.Equal(t, "json", g.GetCodec())
		x.True(t, g.HasTimeout(), "timeout must survive")
		x.Equal(t, 1500*time.Millisecond, g.GetTimeout().AsDuration())
		x.True(t, g.HasPayload(), "payload presence is explicit (§5)")
		x.Equal(t, []byte{0xAA}, g.GetPayload())
		x.True(t, g.HasCode(), "code presence is explicit (§5): 0 must survive as present")
		x.Equal(t, 0, g.GetCode())
		x.Equal(t, "d", g.GetDesc())
		x.False(t, g.HasHeader())
		x.False(t, g.HasTrailer())
		x.Equal(t, 0x0A0B0C0D, g.GetPeerEpoch())
	})
	t.Run("Envelop", func(t *testing.T) {
		open := &drpc.Frame{}
		open.SetEpoch(1)
		open.SetSid(2)
		open.SetSeq(1)
		open.SetFlags(drpc.FlagOpen)
		open.SetMethod("/a.B/C")
		data := &drpc.Frame{}
		data.SetEpoch(1)
		data.SetSid(2)
		data.SetSeq(2)
		data.SetPayload([]byte{0xAA})
		e := &drpc.Envelop{}
		e.SetFrames([]*drpc.Frame{open, data})

		a, err := proto.Marshal(e)
		x.NoError(t, err)
		b, err := proto.Marshal(e)
		x.NoError(t, err)
		x.Equal(t, hex.EncodeToString(a), hex.EncodeToString(b), "marshal must be deterministic")

		if got := hex.EncodeToString(a); got != goldenEnvelopHex {
			t.Fatalf("Envelop wire bytes drifted (frames must stay field 1):\n got  %s\n want %s", got, goldenEnvelopHex)
		}

		raw, err := hex.DecodeString(goldenEnvelopHex)
		x.NoError(t, err)
		g := &drpc.Envelop{}
		x.NoError(t, proto.Unmarshal(raw, g))
		fs := g.GetFrames()
		x.Len(t, fs, 2)
		x.Equal(t, 1, fs[0].GetEpoch())
		x.Equal(t, 2, fs[0].GetSid())
		x.Equal(t, 1, fs[0].GetSeq())
		x.Equal(t, drpc.FlagOpen, fs[0].GetFlags())
		x.Equal(t, "/a.B/C", fs[0].GetMethod())
		x.False(t, fs[0].HasPayload())
		x.Equal(t, 1, fs[1].GetEpoch())
		x.Equal(t, 2, fs[1].GetSid())
		x.Equal(t, 2, fs[1].GetSeq())
		x.Equal(t, uint32(0), fs[1].GetFlags())
		x.True(t, fs[1].HasPayload())
		x.Equal(t, []byte{0xAA}, fs[1].GetPayload())
	})
}

// ---------------------------------------------------------------------------
// §12: the codec is named on OPEN only. An unknown codec on OPEN rejects the
// call with T{UNIMPLEMENTED} (§9.4); the codec field on any later frame
// addresses nothing and is ignored.
// ---------------------------------------------------------------------------

func TestWireShape_UnknownCodec(t *testing.T) {
	t.Run("unknown codec on OPEN -> T{UNIMPLEMENTED}", func(t *testing.T) {
		is := newInjectServer(t)
		f := openFrame(1, 21, 1, echo.EchoService_Once_FullMethodName)
		f.SetCodec("nope")
		is.handle(f)

		r := is.recv(t)
		x.True(t, r != nil, "expected a terminal")
		x.Equal(t, drpc.FlagClose, r.GetFlags())
		x.True(t, r.HasCode(), "rejection is a terminal CLOSE, not a RESET (§9.4)")
		x.Equal(t, codes.Unimplemented, codes.Code(r.GetCode()))
		x.Equal(t, uint32(1), r.GetPeerEpoch(), "the rejection names the client incarnation (§6.1)")
	})
	t.Run("codec on a later frame is ignored", func(t *testing.T) {
		is := newInjectServer(t)
		const cEpoch, sid = uint32(1), uint32(22)
		is.handle(liveOpen(cEpoch, sid)) // codec "" = proto (§12)
		h := is.recv(t)
		x.True(t, h != nil && h.GetFlags() == 0 && !h.HasPayload(), "expected the creation ack H")

		// A data frame that also names a bogus codec: the field addresses
		// nothing after OPEN — the call keeps its proto codec and echoes.
		data := echoData(t, cEpoch, sid, 2, "abc")
		data.SetCodec("nope")
		is.handle(data)

		res := is.recv(t)
		x.True(t, res != nil, "the call must keep working")
		x.Equal(t, uint32(0), res.GetFlags(), "expected a data frame, not an error frame")
		x.True(t, res.HasPayload())
		got := &echo.EchoResponse{}
		x.NoError(t, proto.Unmarshal(res.GetPayload(), got))
		x.Equal(t, "bca", got.GetMessage())
	})
}

// ---------------------------------------------------------------------------
// §13: the registry freezes when serving starts — RegisterService after the
// first Handle panics.
// ---------------------------------------------------------------------------

func TestWireShape_RegistrationFreeze(t *testing.T) {
	srv := drpc.NewServer(drpc.FrameHandlerFunc(func(context.Context, *drpc.Frame) error {
		return nil
	}), drpc.WithReliable(true))
	defer srv.Stop()

	// Any received frame flips the server to serving.
	ping := &drpc.Frame{}
	ping.SetEpoch(1)
	ping.SetFlags(drpc.FlagPing)
	x.NoError(t, srv.Handle(context.Background(), ping))

	panicked := func() (v any) {
		defer func() { v = recover() }()
		echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})
		return nil
	}()
	x.True(t, panicked != nil, "RegisterService after the first Handle must panic (§13)")
}

// ---------------------------------------------------------------------------
// §4.2: the two drop policies observably differ. With a 2-frame rx buffer and
// four data frames injected before the app Recvs, DropNewest keeps the oldest
// two payloads and DropOldest keeps the newest two. The terminal T is
// processed via the seq window and finishTerm, never the buffer, so it lands
// under either policy.
// ---------------------------------------------------------------------------

func runDropPolicy(t *testing.T, policy drpc.DropPolicy) []string {
	t.Helper()

	frames := make(chan *drpc.Frame, 64)
	conn := drpc.NewConn(drpc.FrameHandlerFunc(func(_ context.Context, f *drpc.Frame) error {
		frames <- proto.CloneOf(f)
		return nil
	}), drpc.WithReliable(false), drpc.WithRxBuffer(2, policy))
	defer conn.Close(nil)

	client := echo.NewEchoServiceClient(conn)
	stream, err := client.Many(t.Context(), echo.EchoRequest_builder{Message: "abc"}.Build())
	x.NoError(t, err)

	open := <-frames // the recorded OPEN|CLOSE of the server-streaming call
	x.True(t, open.GetFlags()&drpc.FlagOpen != 0, "first client frame must be the OPEN")
	cEpoch := open.GetEpoch()
	sid := open.GetSid()

	// A fake server incarnation streams four responses. Every frame MUST echo
	// the client epoch (peer_epoch, §6.1) or the Conn refuses it outright.
	const srvEpoch = uint32(7)
	ctx := context.Background()
	for seq := uint32(1); seq <= 4; seq++ {
		payload, err := proto.Marshal(echo.EchoResponse_builder{
			Message: fmt.Sprintf("m%d", seq),
		}.Build())
		x.NoError(t, err)
		f := &drpc.Frame{}
		f.SetEpoch(srvEpoch)
		f.SetSid(sid)
		f.SetSeq(seq)
		f.SetPeerEpoch(cEpoch)
		f.SetPayload(payload)
		x.NoError(t, conn.Handle(ctx, f))
	}

	// Terminal T at seq 5: the window accepted all four data frames (buffer
	// drops do not regress L), so one forward step admits it.
	term := &drpc.Frame{}
	term.SetEpoch(srvEpoch)
	term.SetSid(sid)
	term.SetSeq(5)
	term.SetFlags(drpc.FlagClose)
	term.SetCode(uint32(codes.OK))
	term.SetPeerEpoch(cEpoch)
	x.NoError(t, conn.Handle(ctx, term))

	var got []string
	for {
		res, err := stream.Recv()
		if err == io.EOF {
			break
		}
		x.NoError(t, err)
		got = append(got, res.GetMessage())
	}
	return got
}

func TestWireShape_DropPolicies(t *testing.T) {
	t.Run("DropNewest keeps the buffered prefix", func(t *testing.T) {
		x.Equal(t, []string{"m1", "m2"}, runDropPolicy(t, drpc.DropNewest))
	})
	t.Run("DropOldest keeps the freshest", func(t *testing.T) {
		x.Equal(t, []string{"m3", "m4"}, runDropPolicy(t, drpc.DropOldest))
	})
}
