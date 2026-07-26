package drpc_test

import (
	"context"
	"io"
	"testing"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/lossy"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// These tests pin the server-header metadata rules of PROTOCOL.md §11 against
// the wire shapes of §8: the SendHeader immediate-H flush (on streaming calls
// and, since v1.1, on unary ones too — gRPC parity, so Header() returns before
// the response), the SetHeader defer-to-next-frame path, the T header re-carry
// that survives first-frame loss (§10.3 recovers the call, T recovers the
// header), and the first-wins latch shared with trailers (§7).

// rxFrames snapshots the recorded server->client frames.
func (c *Client) rxFrames() []*drpc.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*drpc.Frame(nil), c.rx...)
}

// hdrBearing selects non-terminal frames carrying the server header MD — the
// H of a SendHeader flush, or the first data frame of the SetHeader path.
func hdrBearing(f *drpc.Frame) bool { return f.GetFlags() == 0 && f.HasHeader() }

// dropEvery drops every frame matching match; everything else passes.
func dropEvery(match func(f *drpc.Frame) bool) func(drpc.FrameHandler) drpc.FrameHandler {
	return func(next drpc.FrameHandler) drpc.FrameHandler {
		return lossy.New(next, lossy.Options{Drop: 1, Filter: match})
	}
}

// wireMd builds the wire Metadata message from md (test-side twin of the
// core's newMd).
func wireMd(md metadata.MD) *drpc.Metadata {
	es := map[string]*drpc.Metadata_Entry{}
	for k, v := range md {
		bs := make([][]byte, len(v))
		for i, s := range v {
			bs[i] = []byte(s)
		}
		es[k] = drpc.Metadata_Entry_builder{Values: bs}.Build()
	}
	return drpc.Metadata_builder{Entries: es}.Build()
}

// findTerminal returns the first terminal frame of frames, or fails.
func findTerminal(t *testing.T, frames []*drpc.Frame) *drpc.Frame {
	t.Helper()
	for _, f := range frames {
		if isTerminal(f) {
			return f
		}
	}
	t.Fatal("no terminal frame recorded")
	return nil
}

// TestStreamingSendHeaderFlushesH pins PROTOCOL.md §11: SendHeader flushes
// immediately as an H frame on streaming calls — the header MD rides a
// standalone H (flags 0, no payload, §7) sent before the first data frame —
// and §11's "T always carries it again once set".
func TestStreamingSendHeaderFlushesH(t *testing.T) {
	client, stop := PipeOption{}.Use(t)
	defer stop()

	md := metadata.Pairs("foo", "bar")
	ctx := metadata.NewOutgoingContext(t.Context(), md)
	wantHdr := metadata.Pairs("foo", "bar", "timing", "header")

	// The request MD triggers handleMd, which calls SendHeader
	// (LazyHeader=false) before the first Send.
	stream, err := client.Many(ctx, echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
		Repeat:        1,
	}.Build())
	x.NoError(t, err)

	res, err := stream.Recv()
	x.NoError(t, err)
	x.Equal(t, "bca", res.GetMessage())
	_, err = stream.Recv()
	x.ErrorIs(t, err, io.EOF)

	header, err := stream.Header()
	x.NoError(t, err)
	x.Equal(t, wantHdr, header)

	frames := client.rxFrames()
	hIdx, dIdx := -1, -1
	for i, f := range frames {
		if hIdx < 0 && f.HasHeader() {
			hIdx = i
		}
		if dIdx < 0 && f.GetFlags() == 0 && f.HasPayload() {
			dIdx = i
		}
	}
	x.True(t, hIdx >= 0, "a server frame must carry the header MD")
	x.True(t, dIdx >= 0, "expected a data frame")
	x.True(t, hIdx < dIdx, "SendHeader must flush an H before the first data frame")

	h := frames[hIdx]
	x.Equal(t, uint32(0), h.GetFlags(), "the flush is a header frame H (§7)")
	x.False(t, h.HasPayload(), "the flush carries no payload (§7)")
	x.Equal(t, wantHdr, h.GetHeader().MD())

	// The header rides a non-terminal frame once: data frames after the flush
	// carry the field absent (§11).
	x.False(t, frames[dIdx].HasHeader(), "data after the H flush must not re-carry the header")

	// ...but T always carries it again once set (§11, §8).
	term := findTerminal(t, frames)
	x.True(t, term.HasHeader(), "T must re-carry the header")
	x.Equal(t, wantHdr, term.GetHeader().MD())
}

// TestSetHeaderDefersToNextFrame pins PROTOCOL.md §11: SetHeader defers to
// the next outgoing frame. The creation ack of §8 is emitted before the
// handler runs, so its header field is absent; the deferred MD rides the
// first data frame (the SS fast path of §11), once — and T re-carries it.
func TestSetHeaderDefersToNextFrame(t *testing.T) {
	client, stop := PipeOption{}.Use(t)
	defer stop()
	client.service.LazyHeader = true // handleMd uses SetHeader instead of SendHeader

	md := metadata.Pairs("foo", "bar")
	ctx := metadata.NewOutgoingContext(t.Context(), md)
	wantHdr := metadata.Pairs("foo", "bar", "timing", "header")

	stream, err := client.Many(ctx, echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
		Repeat:        2,
	}.Build())
	x.NoError(t, err)

	res, err := stream.Recv()
	x.NoError(t, err)
	x.Equal(t, "bca", res.GetMessage())
	res, err = stream.Recv()
	x.NoError(t, err)
	x.Equal(t, "cab", res.GetMessage())
	_, err = stream.Recv()
	x.ErrorIs(t, err, io.EOF)

	// Header() still resolves: the first header-present frame is the data
	// frame (§11).
	header, err := stream.Header()
	x.NoError(t, err)
	x.Equal(t, wantHdr, header)

	frames := client.rxFrames()
	var data []*drpc.Frame
	for _, f := range frames {
		if f.GetFlags() == 0 && !f.HasPayload() {
			// Every standalone H here is the creation ack, sent before the
			// handler set anything: header field absent (§8, §11).
			x.False(t, f.HasHeader(), "SetHeader must not flush a standalone H")
		}
		if f.GetFlags() == 0 && f.HasPayload() {
			data = append(data, f)
		}
	}
	x.Len(t, data, 2)
	x.True(t, data[0].HasHeader(), "the deferred header rides the next outgoing frame — the first data frame")
	x.Equal(t, wantHdr, data[0].GetHeader().MD())
	x.False(t, data[1].HasHeader(), "the header rides a non-terminal frame once")

	term := findTerminal(t, frames)
	x.True(t, term.HasHeader(), "T must re-carry the header")
	x.Equal(t, wantHdr, term.GetHeader().MD())
}

// TestUnarySendHeaderFlushesH pins PROTOCOL.md §11/§8: SendHeader flushes an H
// at once on a unary call too, so a client blocked in Header() is released
// before the handler produces its response — what gRPC does with its separate
// HEADERS frame. T still re-carries the header (§11).
func TestUnarySendHeaderFlushesH(t *testing.T) {
	client, stop := PipeOption{}.Use(t)
	defer stop()

	md := metadata.Pairs("foo", "bar")
	ctx := metadata.NewOutgoingContext(t.Context(), md)
	wantHdr := metadata.Pairs("foo", "bar", "timing", "header")
	wantTrl := metadata.Pairs("foo", "bar", "timing", "trailer")

	header := metadata.MD{}
	trailer := metadata.MD{}
	// handleMd calls SendHeader (LazyHeader=false): on a unary call it flushes
	// an H at once, exactly as gRPC does — a client's Header() must not have
	// to wait for the response (PROTOCOL.md §8, §11).
	res, err := client.Once(ctx, echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
	}.Build(), grpc.Header(&header), grpc.Trailer(&trailer))
	x.NoError(t, err)
	x.Equal(t, "bca", res.GetMessage())
	x.Equal(t, wantHdr, header)
	x.Equal(t, wantTrl, trailer)

	frames := client.rxFrames()
	x.Len(t, frames, 2, "SendHeader flushes an H, then the T (§8)")
	h := frames[0]
	x.True(t, isAckH(h), "the flushed header frame carries no payload")
	x.True(t, h.HasHeader(), "and carries the header MD")
	x.Equal(t, wantHdr, h.GetHeader().MD())
	x.Equal(t, uint32(1), h.GetSeq())

	term := frames[1]
	x.True(t, isTerminal(term))
	x.Equal(t, uint32(2), term.GetSeq())
	x.True(t, term.HasHeader(), "the T re-carries the header (§11)")
	x.Equal(t, wantHdr, term.GetHeader().MD())
	x.Equal(t, wantTrl, term.GetTrailer().MD())
	x.True(t, term.HasPayload(), "the unary response rides the T")
}

// The lazy twin: SetHeader alone defers to the next outgoing frame, so a
// unary call still answers with a single T carrying both header and response.
func TestUnarySetHeaderRidesTerminal(t *testing.T) {
	client, stop := PipeOption{}.Use(t)
	defer stop()
	client.service.LazyHeader = true

	md := metadata.Pairs("foo", "bar")
	ctx := metadata.NewOutgoingContext(t.Context(), md)
	wantHdr := metadata.Pairs("foo", "bar", "timing", "header")

	header := metadata.MD{}
	res, err := client.Once(ctx, echo.EchoRequest_builder{
		Message:       "abc",
		CircularShift: 1,
	}.Build(), grpc.Header(&header))
	x.NoError(t, err)
	x.Equal(t, "bca", res.GetMessage())
	x.Equal(t, wantHdr, header)

	frames := client.rxFrames()
	x.Len(t, frames, 1, "unary shape without a flush is a single server frame (§8)")
	term := frames[0]
	x.True(t, isTerminal(term))
	x.True(t, term.HasHeader(), "the header rides the T")
	x.True(t, term.HasPayload(), "the unary response rides the same T")
}

// TestTerminalRecarriesHeader pins the loss-resilience rule of PROTOCOL.md
// §11: "T always carries it again once set". When the frame that first
// carried the header MD is lost, the call still completes (§10.3) and
// Header() still resolves — from the T re-carry.
func TestTerminalRecarriesHeader(t *testing.T) {
	md := metadata.Pairs("foo", "bar")
	wantHdr := metadata.Pairs("foo", "bar", "timing", "header")
	wantTrl := metadata.Pairs("foo", "bar", "timing", "trailer")

	t.Run("flushed H dropped once", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			// SendHeader path: the flushed H is the first header-bearing
			// frame; drop it once on the wire.
			drop := dropFirst(hdrBearing)
			client, stop := unreliablePipe(nil, drop).Use(t)
			defer stop()

			ctx := metadata.NewOutgoingContext(t.Context(), md)
			stream, err := client.Many(ctx, echo.EchoRequest_builder{
				Message:       "abc",
				CircularShift: 1,
				Repeat:        1,
			}.Build())
			x.NoError(t, err)

			res, err := stream.Recv()
			x.NoError(t, err)
			x.Equal(t, "bca", res.GetMessage())
			_, err = stream.Recv()
			x.ErrorIs(t, err, io.EOF)

			header, err := stream.Header()
			x.NoError(t, err)
			x.Equal(t, wantHdr, header, "the header must survive the loss of its H via the T re-carry")
			x.Equal(t, wantTrl, stream.Trailer(), "the trailer rides the same T (§11)")
		})
	})
	t.Run("every non-terminal header carrier dropped", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			// Sharper: drop EVERY non-terminal header-bearing frame — replays
			// included — so the T is the only frame that can deliver the MD.
			drop := dropEvery(hdrBearing)
			client, stop := unreliablePipe(nil, drop).Use(t)
			defer stop()
			// SetHeader path: the deferred header rides the first data frame,
			// which the filter eats.
			client.service.LazyHeader = true

			ctx := metadata.NewOutgoingContext(t.Context(), md)
			stream, err := client.Many(ctx, echo.EchoRequest_builder{
				Message:       "abc",
				CircularShift: 1,
				Repeat:        2,
			}.Build())
			x.NoError(t, err)

			// The first data frame (the header carrier) is lost; unreliable-
			// mode SS delivery is best-effort (§14), so the stream resumes at
			// the second message.
			res, err := stream.Recv()
			x.NoError(t, err)
			x.Equal(t, "cab", res.GetMessage())
			x.Equal(t, 1, res.GetSequence())
			_, err = stream.Recv()
			x.ErrorIs(t, err, io.EOF)

			header, err := stream.Header()
			x.NoError(t, err)
			x.Equal(t, wantHdr, header, "only the T could deliver the header")
			x.Equal(t, wantTrl, stream.Trailer())
		})
	})
}

// TestHeaderFirstWins pins the first-wins rule of PROTOCOL.md §7/§11: the
// first accepted frame with header MD present latches it; a later frame
// disagreeing about the header — the T included — must not rewrite it. The
// server frames are crafted so a second carrier can actually disagree.
func TestHeaderFirstWins(t *testing.T) {
	ctx := t.Context()

	frames := make(chan *drpc.Frame, 8)
	// Reliable mode: no timers, no retransmission — the crafted exchange is
	// the whole conversation.
	conn := drpc.NewConn(drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		frames <- proto.CloneOf(f)
		return nil
	}), drpc.WithReliable(true))
	defer conn.Close(nil)

	client := echo.NewEchoServiceClient(conn)
	stream, err := client.Many(ctx, &echo.EchoRequest{})
	x.NoError(t, err)

	open := <-frames // the OPEN|CLOSE of the server-streaming call

	const srvEpoch = 7
	craft := func(seq uint32) *drpc.Frame {
		f := &drpc.Frame{}
		f.SetEpoch(srvEpoch)
		f.SetSid(open.GetSid())
		f.SetSeq(seq)
		// Every server frame must echo the client incarnation it addresses
		// (PROTOCOL.md §6.1) or the Conn refuses it with a RESET.
		f.SetPeerEpoch(open.GetEpoch())
		return f
	}

	payload, err := proto.Marshal(echo.EchoResponse_builder{Message: "crafted"}.Build())
	x.NoError(t, err)

	h := craft(1) // H: flags 0, no payload (§7), header {k: v1}
	h.SetHeader(wireMd(metadata.Pairs("k", "v1")))

	d := craft(2) // data frame disagreeing about the header
	d.SetPayload(payload)
	d.SetHeader(wireMd(metadata.Pairs("k", "v2")))

	term := craft(3) // T disagreeing too, and carrying the trailer
	term.SetFlags(drpc.FlagClose)
	term.SetCode(uint32(codes.OK))
	term.SetHeader(wireMd(metadata.Pairs("k", "v3")))
	term.SetTrailer(wireMd(metadata.Pairs("t", "tv")))

	x.NoError(t, conn.Handle(ctx, h))
	x.NoError(t, conn.Handle(ctx, d))
	x.NoError(t, conn.Handle(ctx, term))

	header, err := stream.Header()
	x.NoError(t, err)
	x.Equal(t, metadata.Pairs("k", "v1"), header, "the first accepted header wins")

	res, err := stream.Recv()
	x.NoError(t, err)
	x.Equal(t, "crafted", res.GetMessage())
	_, err = stream.Recv()
	x.ErrorIs(t, err, io.EOF)

	x.Equal(t, metadata.Pairs("t", "tv"), stream.Trailer(), "the trailer rides only T (§11)")
	header, err = stream.Header()
	x.NoError(t, err)
	x.Equal(t, metadata.Pairs("k", "v1"), header, "the terminal must not rewrite the latched header")
}
