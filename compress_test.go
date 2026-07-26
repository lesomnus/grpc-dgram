package drpc_test

// compress_test.go pins message compression (PROTOCOL.md §12.1) — the
// per-call `compressor` named on the OPEN, and the COMPRESSED modifier bit
// that says a given frame's payload actually is compressed:
//
//   - the compressor governs the WHOLE call, both directions, like the codec
//     (§12, §12.1): it is named on the OPEN only, and every message frame of
//     all four RPC types rides compressed;
//   - a payload that would GROW — tiny, empty, or already high-entropy — is
//     sent raw, without the flag and without expansion (§12.1);
//   - an unknown compressor at the server draws T{UNIMPLEMENTED}, like an
//     unknown codec (§12.1); the client's own registry guard fails the call
//     locally before anything reaches the wire;
//   - decompression is bounded by the receive cap and fails
//     RESOURCE_EXHAUSTED past it, so a compression bomb costs nothing
//     (§12.1);
//   - COMPRESSED is a MODIFIER, not a shape (§7.1): a compressed unary /
//     SendAndClose response rides the terminal frame, whose shape is still
//     CLOSE — this is the regression the SHAPE_MASK exists for;
//   - the terminal's other passengers (google.rpc.Status.details, §5) travel
//     on a compressed call unchanged.

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // the §12.1 interop baseline
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// czShapeMask is PROTOCOL.md §7.1's SHAPE_MASK: a frame's shape is
// flags & SHAPE_MASK, and COMPRESSED (32) lives outside it.
const czShapeMask = drpc.FlagOpen | drpc.FlagClose | drpc.FlagReset | drpc.FlagPing | drpc.FlagWindow

// czBig is a highly compressible message: gzip takes it from ~450 B to ~50 B,
// so "was this frame compressed?" is unambiguous on the wire.
var czBig = strings.Repeat("Royale with Cheese ", 24)

// czPipe builds a reliable-mode pipe. Compression is orthogonal to the
// datagram machinery (§7.1), so these tests want a lossless, timer-free
// channel: every recorded frame is one the core deliberately sent.
func czPipe(t *testing.T, serverOpts ...drpc.ServerOption) (*Client, func()) {
	t.Helper()
	return PipeOption{
		ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true)},
		ServerOpts: append([]drpc.ServerOption{drpc.WithReliable(true)}, serverOpts...),
	}.Use(t)
}

// czCtx bounds every call in this file. A routing regression — a compressed
// frame no receiver accepts — must surface as a prompt failure rather than a
// hung test, so the deadline is the test's own safety net; a healthy call
// never comes near it.
func czCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// czTxFrames / czRxFrames copy the recorded frames of one direction.
func czTxFrames(c *Client) []*drpc.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*drpc.Frame(nil), c.tx...)
}

func czRxFrames(c *Client) []*drpc.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*drpc.Frame(nil), c.rx...)
}

// czMessages keeps the frames that carry a message. Payload presence is what
// makes a frame a message (§7.1): creation acks, half-closes, WINDOW grants
// and an error terminal carry none.
func czMessages(fs []*drpc.Frame) []*drpc.Frame {
	var out []*drpc.Frame
	for _, f := range fs {
		if f.HasPayload() {
			out = append(out, f)
		}
	}
	return out
}

// czTerminal returns the recorded server terminal T: shape CLOSE with a code
// (§7), found under the modifier bit rather than by whole-bitmask equality.
func czTerminal(t *testing.T, c *Client) *drpc.Frame {
	t.Helper()
	for _, f := range czRxFrames(c) {
		if f.GetFlags()&czShapeMask == drpc.FlagClose && f.HasCode() {
			return f
		}
	}
	t.Fatal("no terminal frame recorded")
	return nil
}

// czOpen returns the recorded OPEN (shape OPEN or OPEN|CLOSE, §8).
func czOpen(t *testing.T, c *Client) *drpc.Frame {
	t.Helper()
	for _, f := range czTxFrames(c) {
		if f.GetFlags()&drpc.FlagOpen != 0 {
			return f
		}
	}
	t.Fatal("no OPEN frame recorded")
	return nil
}

// czAssertCompressed asserts that fs is exactly want message frames, each
// carrying COMPRESSED over a legal shape (§7.1): the modifier never replaces
// the shape it rides on.
func czAssertCompressed(t *testing.T, fs []*drpc.Frame, want int, what string) {
	t.Helper()
	x.Len(t, fs, want, what)
	for _, f := range fs {
		x.True(t, f.GetFlags()&drpc.FlagCompressed != 0,
			what, ": message frame must carry COMPRESSED, flags=", f.GetFlags())
		switch shape := f.GetFlags() & czShapeMask; shape {
		case 0, drpc.FlagOpen | drpc.FlagClose, drpc.FlagClose:
		default:
			t.Fatalf("%s: compressed message frame has shape %#x, which carries no payload in §8", what, shape)
		}
	}
}

// czAssertCompressorOnOpenOnly pins §12/§12.1's "named on OPEN only": the
// compressor addresses the call, not the frame, so no later client frame and
// no server frame repeats it.
func czAssertCompressorOnOpenOnly(t *testing.T, c *Client, name string) {
	t.Helper()
	x.Equal(t, name, czOpen(t, c).GetCompressor(), "the OPEN names the compressor")
	for _, f := range czTxFrames(c) {
		if f.GetFlags()&drpc.FlagOpen != 0 {
			continue
		}
		x.Equal(t, "", f.GetCompressor(), "only the OPEN names the compressor (§12.1)")
	}
	for _, f := range czRxFrames(c) {
		x.Equal(t, "", f.GetCompressor(), "the server never re-names the compressor (§12.1)")
	}
}

func czMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	x.NoError(t, err)
	return b
}

// czIncompressible builds a deterministic high-entropy printable string.
// gzip's own framing (a ~20-byte header plus a trailer) costs more than
// Huffman coding saves on it, so §12.1's "MUST NOT compress a payload that
// would grow" applies. The fixture's precondition is asserted, not assumed
// (czAssertWouldGrow).
func czIncompressible(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ!#$%&()*+,-./:;<=>?@[]^_{|}~"
	r := rand.New(rand.NewPCG(0x5EED, 0xD1CE))
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// czGzipBomb returns a gzip stream that expands to n zero bytes (~1000:1), so
// a receiver that does not bound its read materializes n bytes out of a frame
// of a few kilobytes.
func czGzipBomb(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	chunk := make([]byte, 64<<10)
	for written := 0; written < n; written += len(chunk) {
		_, err := w.Write(chunk)
		x.NoError(t, err)
	}
	x.NoError(t, w.Close())
	return buf.Bytes()
}

// czAssertWouldGrow checks the FIXTURE, not the core: gzipping raw really
// does produce at least as many bytes, so a raw frame below is the "would
// grow" rule of §12.1 and not an accident of the data.
func czAssertWouldGrow(t *testing.T, raw []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(raw)
	x.NoError(t, err)
	x.NoError(t, w.Close())
	x.True(t, buf.Len() >= len(raw),
		"fixture must not be compressible: raw=", len(raw), " gzip=", buf.Len())
}

// ---------------------------------------------------------------------------
// §12.1: the compressor is named on the OPEN and governs the whole call in
// both directions, like the codec — for every RPC type of §8. The messages
// decode back exactly; the wire shows COMPRESSED on every message frame.
// ---------------------------------------------------------------------------

func TestCompress_RoundTripAllRPCTypes(t *testing.T) {
	t.Run("Unary", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		req := echo.EchoRequest_builder{Message: czBig, CircularShift: 3}.Build()
		res, err := client.Once(czCtx(t), req, grpc.UseCompressor("gzip"))
		x.NoError(t, err)
		x.Equal(t, echo.CircularShift(czBig, 3), res.GetMessage())

		// C→S: the request rides OPEN|CLOSE (§8), compressed.
		tx := czMessages(czTxFrames(client))
		czAssertCompressed(t, tx, 1, "unary request")
		x.Equal(t, drpc.FlagOpen|drpc.FlagClose|drpc.FlagCompressed, tx[0].GetFlags())
		x.True(t, len(tx[0].GetPayload()) < len(czMarshal(t, req)),
			"the wire payload must be the compressed one")

		// S→C: the response rides T, compressed — a modifier on a terminal.
		rx := czMessages(czRxFrames(client))
		czAssertCompressed(t, rx, 1, "unary response")
		x.Equal(t, drpc.FlagClose|drpc.FlagCompressed, rx[0].GetFlags())

		czAssertCompressorOnOpenOnly(t, client, "gzip")
	})

	t.Run("ServerStreaming", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		stream, err := client.Many(czCtx(t), echo.EchoRequest_builder{
			Message:       czBig,
			CircularShift: 1,
			Repeat:        3,
		}.Build(), grpc.UseCompressor("gzip"))
		x.NoError(t, err)

		want := czBig
		for i := range 3 {
			want = echo.CircularShift(want, 1)
			res, err := stream.Recv()
			x.NoError(t, err)
			x.Equal(t, want, res.GetMessage())
			x.Equal(t, uint32(i), res.GetSequence())
		}
		_, err = stream.Recv()
		x.ErrorIs(t, err, io.EOF)

		czAssertCompressed(t, czMessages(czTxFrames(client)), 1, "streaming request")
		rx := czMessages(czRxFrames(client))
		czAssertCompressed(t, rx, 3, "streamed responses")
		for _, f := range rx {
			x.Equal(t, drpc.FlagCompressed, f.GetFlags(), "a compressed data frame has shape 0")
		}
		// The payload-less terminal is never compressed: there is nothing to
		// compress, and the flag would lie about the frame (§12.1).
		x.Equal(t, drpc.FlagClose, czTerminal(t, client).GetFlags())

		czAssertCompressorOnOpenOnly(t, client, "gzip")
	})

	t.Run("ClientStreaming", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		stream, err := client.Buff(czCtx(t), grpc.UseCompressor("gzip"))
		x.NoError(t, err)

		for range 2 {
			x.NoError(t, stream.Send(echo.EchoRequest_builder{
				Message:       czBig,
				CircularShift: 1,
				Repeat:        1,
			}.Build()))
		}
		res, err := stream.CloseAndRecv()
		x.NoError(t, err)
		x.Len(t, res.GetItems(), 2)
		// Each request is answered from its own message, so both items are
		// the same one-step shift; the sequence numbers run across the call.
		for i, item := range res.GetItems() {
			x.Equal(t, echo.CircularShift(czBig, 1), item.GetMessage())
			x.Equal(t, uint32(i), item.GetSequence())
		}

		// The eager OPEN carries no payload (§8): the messages are data
		// frames, and the response rides the terminal.
		tx := czMessages(czTxFrames(client))
		czAssertCompressed(t, tx, 2, "streamed requests")
		for _, f := range tx {
			x.Equal(t, drpc.FlagCompressed, f.GetFlags(), "a compressed data frame has shape 0")
		}
		rx := czMessages(czRxFrames(client))
		czAssertCompressed(t, rx, 1, "SendAndClose response")
		x.Equal(t, drpc.FlagClose|drpc.FlagCompressed, rx[0].GetFlags())

		czAssertCompressorOnOpenOnly(t, client, "gzip")
	})

	t.Run("BidiStreaming", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		stream, err := client.Live(czCtx(t), grpc.UseCompressor("gzip"))
		x.NoError(t, err)

		for i := range 2 {
			x.NoError(t, stream.Send(echo.EchoRequest_builder{
				Message:       czBig,
				CircularShift: 1,
				Repeat:        1,
			}.Build()))
			res, err := stream.Recv()
			x.NoError(t, err)
			x.Equal(t, echo.CircularShift(czBig, 1), res.GetMessage())
			x.Equal(t, uint32(i), res.GetSequence())
		}
		x.NoError(t, stream.CloseSend())
		_, err = stream.Recv()
		x.ErrorIs(t, err, io.EOF)

		czAssertCompressed(t, czMessages(czTxFrames(client)), 2, "bidi requests")
		czAssertCompressed(t, czMessages(czRxFrames(client)), 2, "bidi responses")

		czAssertCompressorOnOpenOnly(t, client, "gzip")
	})
}

// ---------------------------------------------------------------------------
// §12.1: the decision is PER FRAME. A payload that compression would grow —
// empty, tiny, or high-entropy — travels raw: no COMPRESSED flag, and the
// bytes on the wire are the message itself, never a larger encoding of it.
// ---------------------------------------------------------------------------

func TestCompress_PayloadThatWouldGrowStaysRaw(t *testing.T) {
	t.Run("EmptyMessage", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		// A 0-byte message is meaningful (§5, §7.1): it must stay a present,
		// empty, uncompressed payload — a gzip header here would turn "no
		// bytes" into "some bytes".
		res, err := client.Noop(czCtx(t), &echo.EchoRequest{}, grpc.UseCompressor("gzip"))
		x.NoError(t, err)
		x.Equal(t, "", res.GetMessage())

		tx := czMessages(czTxFrames(client))
		x.Len(t, tx, 1, "the request rides OPEN|CLOSE")
		x.Equal(t, drpc.FlagOpen|drpc.FlagClose, tx[0].GetFlags(), "an empty payload is never compressed")
		x.Len(t, tx[0].GetPayload(), 0)

		rx := czMessages(czRxFrames(client))
		x.Len(t, rx, 1, "the response rides T")
		x.Equal(t, drpc.FlagClose, rx[0].GetFlags(), "an empty payload is never compressed")
		x.Len(t, rx[0].GetPayload(), 0)
	})

	t.Run("TinyMessage", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		req := echo.EchoRequest_builder{Message: "hi"}.Build()
		raw := czMarshal(t, req)
		czAssertWouldGrow(t, raw)

		res, err := client.Once(czCtx(t), req, grpc.UseCompressor("gzip"))
		x.NoError(t, err)
		x.Equal(t, "hi", res.GetMessage())

		tx := czMessages(czTxFrames(client))
		x.Len(t, tx, 1)
		x.Equal(t, drpc.FlagOpen|drpc.FlagClose, tx[0].GetFlags(), "a tiny payload is sent raw")
		x.Equal(t, raw, tx[0].GetPayload(), "the raw message travels byte-identically")

		// The response is tiny too, so the server made the same decision.
		rx := czMessages(czRxFrames(client))
		x.Len(t, rx, 1)
		x.Equal(t, drpc.FlagClose, rx[0].GetFlags(), "a tiny payload is sent raw")
	})

	t.Run("IncompressibleMessage", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		msg := czIncompressible(192)
		req := echo.EchoRequest_builder{Message: msg}.Build()
		raw := czMarshal(t, req)
		czAssertWouldGrow(t, raw)

		res, err := client.Once(czCtx(t), req, grpc.UseCompressor("gzip"))
		x.NoError(t, err)
		x.Equal(t, msg, res.GetMessage())

		tx := czMessages(czTxFrames(client))
		x.Len(t, tx, 1)
		x.Equal(t, drpc.FlagOpen|drpc.FlagClose, tx[0].GetFlags(),
			"compression that would expand the payload is skipped")
		x.Equal(t, raw, tx[0].GetPayload(), "the raw message travels byte-identically")
		x.True(t, len(tx[0].GetPayload()) <= len(raw), "a raw frame never grows the message")
	})
}

// ---------------------------------------------------------------------------
// §12.1: an unknown compressor at the server draws T{UNIMPLEMENTED}, exactly
// like an unknown codec (§12) — nothing silently degrades, and no handler
// runs. The client's own registry is checked locally first, so a compressor
// it cannot use never reaches the wire at all.
// ---------------------------------------------------------------------------

func TestCompress_UnknownCompressorIsUnimplemented(t *testing.T) {
	t.Run("at the server", func(t *testing.T) {
		var execs atomic.Int32
		is := newInjectServer(t, countExecs(&execs))

		// A unary OPEN|CLOSE (§8) naming a compressor this build does not
		// have. Its payload is plain — the name alone must be refused,
		// before the request is ever decoded.
		f := &drpc.Frame{}
		f.SetEpoch(0xC0FFEE)
		f.SetSid(1)
		f.SetSeq(1)
		f.SetFlags(drpc.FlagOpen | drpc.FlagClose)
		f.SetMethod(echo.EchoService_Once_FullMethodName)
		f.SetCompressor("brotli-9")
		f.SetPayload(czMarshal(t, echo.EchoRequest_builder{Message: "hi"}.Build()))
		is.handle(f)

		got := is.recv(t)
		x.True(t, got != nil, "an unknown compressor must be answered, not ignored")
		x.Equal(t, drpc.FlagClose, got.GetFlags(), "the answer is a terminal T")
		x.Equal(t, codes.Unimplemented, codes.Code(got.GetCode()))
		x.True(t, strings.Contains(got.GetDesc(), "brotli-9"), "T names the compressor: ", got.GetDesc())
		x.Equal(t, uint32(1), got.GetSid())
		x.Equal(t, int32(0), execs.Load(), "the handler must never run")
	})

	t.Run("at the client", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		_, err := client.Once(czCtx(t), echo.EchoRequest_builder{Message: czBig}.Build(),
			grpc.UseCompressor("brotli-9"))
		x.Equal(t, codes.Internal, status.Code(err), "a compressor we lack fails the call locally")
		x.Len(t, czTxFrames(client), 0, "nothing reached the wire")
	})
}

// ---------------------------------------------------------------------------
// §12.1: a receiver MUST bound decompression by its receive size cap and fail
// RESOURCE_EXHAUSTED past it — the expansion is read one byte past the cap,
// never materialized, so a compression bomb costs nothing. Both roles.
// ---------------------------------------------------------------------------

func TestCompress_DecompressionIsBoundedByRecvCap(t *testing.T) {
	const recvCap = 4096
	// 1 MiB that gzips to ~1 kB: the frame on the wire is tiny, the message
	// behind it is 256x the cap.
	bomb := strings.Repeat("a", 1<<20)

	t.Run("client receiving", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		_, err := client.Once(czCtx(t), echo.EchoRequest_builder{Message: bomb}.Build(),
			grpc.UseCompressor("gzip"), grpc.MaxCallRecvMsgSize(recvCap))
		st := status.Convert(err)
		x.Equal(t, codes.ResourceExhausted, st.Code())
		x.True(t, strings.Contains(st.Message(), "after decompression"),
			"the cap must stop the DECOMPRESSION, not the decompressed message: ", st.Message())
	})

	t.Run("server receiving", func(t *testing.T) {
		client, stop := czPipe(t, drpc.WithMaxRecvMsgSize(recvCap))
		defer stop()

		_, err := client.Once(czCtx(t), echo.EchoRequest_builder{Message: bomb}.Build(),
			grpc.UseCompressor("gzip"))
		st := status.Convert(err)
		x.Equal(t, codes.ResourceExhausted, st.Code())
		x.True(t, strings.Contains(st.Message(), "after decompression"),
			"the cap must stop the DECOMPRESSION, not the decompressed message: ", st.Message())
	})

	t.Run("the bomb costs nothing", func(t *testing.T) {
		// The claim of §12.1 is not just "it fails" but "it costs nothing":
		// the receiver reads one byte past the cap and stops. Injected as a
		// frame, because no client would ever send this: 16 MiB of zeros in a
		// ~16 kB payload, against a 4 kB cap. Bounded, the whole exchange
		// allocates ~120 kB; unbounded, it allocates the bomb.
		const bombSize = 16 << 20
		is := newInjectServer(t, drpc.WithMaxRecvMsgSize(recvCap))

		f := &drpc.Frame{}
		f.SetEpoch(0xB0B0)
		f.SetSid(1)
		f.SetSeq(1)
		f.SetFlags(drpc.FlagOpen | drpc.FlagClose | drpc.FlagCompressed)
		f.SetMethod(echo.EchoService_Once_FullMethodName)
		f.SetCompressor("gzip")
		f.SetPayload(czGzipBomb(t, bombSize))

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		is.handle(f)
		got := is.recv(t)
		runtime.ReadMemStats(&after)

		x.True(t, got != nil, "the bomb must be answered, not swallowed")
		x.Equal(t, codes.ResourceExhausted, codes.Code(got.GetCode()))
		x.True(t, strings.Contains(got.GetDesc(), "after decompression"), got.GetDesc())
		alloc := after.TotalAlloc - before.TotalAlloc
		x.True(t, alloc < bombSize/8,
			"decompression must stop at the cap, not materialize the bomb: allocated ", alloc)
	})
}

// ---------------------------------------------------------------------------
// §7.1: COMPRESSED is a MODIFIER, not a shape. A compressed response rides
// the terminal frame (§8: unary and SendAndClose results do), whose flags are
// then CLOSE|COMPRESSED = 34 — and it must still be routed as a terminal and
// delivered. A receiver that compares the whole bitmask instead of masking
// with SHAPE_MASK drops it and the call hangs to its deadline; this is the
// regression the mask exists for.
// ---------------------------------------------------------------------------

func TestCompress_CompressedTerminalIsStillATerminal(t *testing.T) {
	t.Run("Unary", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		res, err := client.Once(czCtx(t), echo.EchoRequest_builder{
			Message:       czBig,
			CircularShift: 5,
		}.Build(), grpc.UseCompressor("gzip"))
		x.NoError(t, err, "a compressed terminal must be delivered, not dropped")
		x.Equal(t, echo.CircularShift(czBig, 5), res.GetMessage())

		term := czTerminal(t, client)
		x.Equal(t, drpc.FlagClose|drpc.FlagCompressed, term.GetFlags())
		x.Equal(t, drpc.FlagClose, term.GetFlags()&czShapeMask, "its SHAPE is still CLOSE")
		x.True(t, term.HasPayload(), "the response rides the terminal (§8)")
		x.Equal(t, codes.OK, codes.Code(term.GetCode()))
	})

	t.Run("ClientStreaming", func(t *testing.T) {
		client, stop := czPipe(t)
		defer stop()

		stream, err := client.Buff(czCtx(t), grpc.UseCompressor("gzip"))
		x.NoError(t, err)
		x.NoError(t, stream.Send(echo.EchoRequest_builder{
			Message:       czBig,
			CircularShift: 2,
			Repeat:        1,
		}.Build()))
		res, err := stream.CloseAndRecv()
		x.NoError(t, err, "a compressed terminal must be delivered, not dropped")
		x.Len(t, res.GetItems(), 1)
		x.Equal(t, echo.CircularShift(czBig, 2), res.GetItems()[0].GetMessage())

		term := czTerminal(t, client)
		x.Equal(t, drpc.FlagClose|drpc.FlagCompressed, term.GetFlags())
		x.Equal(t, drpc.FlagClose, term.GetFlags()&czShapeMask, "its SHAPE is still CLOSE")
		x.Equal(t, codes.OK, codes.Code(term.GetCode()))
	})
}

// ---------------------------------------------------------------------------
// §5 / §12.1: the terminal's passengers are independent of compression. A
// call that names a compressor still carries google.rpc.Status.details on its
// terminal — the details are proto fields of the frame, never part of the
// compressed payload.
// ---------------------------------------------------------------------------

func TestCompress_StatusDetailsSurviveCompressedCall(t *testing.T) {
	client, stop := czPipe(t)
	defer stop()

	st := status.New(codes.FailedPrecondition, "nope")
	withDetails, derr := st.WithDetails(echo.EchoRequest_builder{Message: "detail"}.Build())
	x.NoError(t, derr)
	client.service.Err = withDetails.Err()

	_, err := client.Once(czCtx(t), echo.EchoRequest_builder{Message: czBig}.Build(),
		grpc.UseCompressor("gzip"))
	got, ok := status.FromError(err)
	x.True(t, ok, "must be a status")
	x.Equal(t, codes.FailedPrecondition, got.Code())
	x.Equal(t, "nope", got.Message())
	details := got.Details()
	x.Len(t, details, 1, "status details must survive a compressed call")
	req, ok := details[0].(*echo.EchoRequest)
	x.True(t, ok, "the detail decodes back to its own type")
	x.Equal(t, "detail", req.GetMessage())

	// The call really was compressed: the request rode COMPRESSED...
	tx := czMessages(czTxFrames(client))
	czAssertCompressed(t, tx, 1, "compressed request")
	// ...and the failing terminal carries details with no payload to compress.
	term := czTerminal(t, client)
	x.Equal(t, drpc.FlagClose, term.GetFlags(), "an error terminal has no payload to compress")
	x.Len(t, term.GetDetails(), 1, "the details ride the frame, not the payload")
}
