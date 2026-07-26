package drpc_test

// stats_test.go pins the observability surface of PROTOCOL.md §14 — the two
// halves an operator instruments a drpc endpoint with:
//
//   - google.golang.org/grpc/stats.Handler, so existing gRPC instrumentation
//     (OpenTelemetry, opencensus, custom handlers) sees the same event stream
//     it sees on a gRPC channel: Begin / OutHeader / OutPayload / InPayload /
//     InTrailer / End on the client, Begin / InHeader / InPayload /
//     OutPayload / OutTrailer / End on the server, with the Client flag and
//     End.Error set the way grpc-go sets them;
//
//   - drpc.ProtocolStats / drpc.Counters, for the datagram-specific events
//     gRPC has no concept of — the per-stream SKIPPED-message counter §14
//     promises, rx drops (§4.2), RESETs (§9.3), control retransmissions
//     (§10.3), keepalives and probes (§10.4, §10.5), liveness expiry,
//     tombstone replays (§9.2), window-overrun DATA_LOSS (§6.3) and
//     flow-control stalls (§4.2).
//
// Every counter test drives the event through the real machinery — a lossy
// filter, a killswitch, a crafted frame — never by calling the emitter.

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/lossy"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// stHandler is a recording stats.Handler: it keeps every RPC event of one
// endpoint in arrival order, plus the methods TagRPC was asked to tag (the
// per-call ctx the core must thread through every later event).
type stHandler struct {
	mu     sync.Mutex
	events []stats.RPCStats
	tagged []string
}

func (h *stHandler) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tagged = append(h.tagged, info.FullMethodName)
	return ctx
}

func (h *stHandler) HandleRPC(_ context.Context, s stats.RPCStats) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, s)
}

func (h *stHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (h *stHandler) HandleConn(context.Context, stats.ConnStats)                       {}

// stKind names an RPC event the way this file's expectations spell it.
func stKind(s stats.RPCStats) string {
	switch s.(type) {
	case *stats.Begin:
		return "Begin"
	case *stats.InHeader:
		return "InHeader"
	case *stats.InPayload:
		return "InPayload"
	case *stats.InTrailer:
		return "InTrailer"
	case *stats.OutHeader:
		return "OutHeader"
	case *stats.OutPayload:
		return "OutPayload"
	case *stats.OutTrailer:
		return "OutTrailer"
	case *stats.End:
		return "End"
	}
	return fmt.Sprintf("%T", s)
}

func (h *stHandler) kinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.events))
	for _, e := range h.events {
		out = append(out, stKind(e))
	}
	return out
}

func (h *stHandler) methods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.tagged...)
}

// find returns the n-th (0-based) event of the given kind, failing the test
// when there are not that many.
func (h *stHandler) find(t *testing.T, kind string, n int) stats.RPCStats {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := 0
	for _, e := range h.events {
		if stKind(e) != kind {
			continue
		}
		if seen == n {
			return e
		}
		seen++
	}
	t.Fatalf("no %s event #%d among %d recorded events", kind, n, len(h.events))
	return nil
}

func (h *stHandler) count(kind string) int {
	n := 0
	for _, k := range h.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

// stEventLog records drpc protocol events (the ProtocolStats side). It must
// not block: the core emits from receive and timer paths.
type stEventLog struct {
	mu  sync.Mutex
	evs []drpc.ProtocolEvent
}

func (l *stEventLog) ProtocolEvent(ev drpc.ProtocolEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
}

// of returns every recorded event of one kind.
func (l *stEventLog) of(k drpc.ProtocolEventKind) []drpc.ProtocolEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []drpc.ProtocolEvent
	for _, ev := range l.evs {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

// first returns the first event of one kind, failing the test if there is
// none.
func (l *stEventLog) first(t *testing.T, k drpc.ProtocolEventKind) drpc.ProtocolEvent {
	t.Helper()
	evs := l.of(k)
	if len(evs) == 0 {
		l.mu.Lock()
		defer l.mu.Unlock()
		t.Fatalf("no %v event in %v", k, l.evs)
	}
	return evs[0]
}

// stManyStream opens a server-streaming Many call on a fresh Conn wired to
// rec and returns the conn, the stream, and the identity every crafted server
// frame must echo: the conn epoch (§6.1) and the call's sid, learned from the
// emitted OPEN. It is dlManyStream (dataloss_test.go) with endpoint options,
// so a test can attach observers to the Conn under test.
func stManyStream(t *testing.T, rec *dlRecorder, opts ...drpc.ConnOption) (*drpc.Conn, grpc.ClientStream, uint32, uint32) {
	t.Helper()
	conn := drpc.NewConn(rec, opts...)
	t.Cleanup(func() { conn.Close(nil) })

	sd := &grpc.StreamDesc{StreamName: "Many", ServerStreams: true}
	stream, err := conn.NewStream(t.Context(), sd, echo.EchoService_Many_FullMethodName)
	x.NoError(t, err)
	// Server-streaming emits its OPEN|CLOSE on the first SendMsg; transmit is
	// synchronous, so the OPEN is recorded when SendMsg returns.
	x.NoError(t, stream.SendMsg(echo.EchoRequest_builder{Message: "req"}.Build()))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, f := range rec.frames {
		if f.GetFlags()&drpc.FlagOpen != 0 {
			return conn, stream, f.GetEpoch(), f.GetSid()
		}
	}
	t.Fatal("no OPEN emitted")
	return nil, nil, 0, 0
}

// stDataFrame builds a bare data frame — no shape flags, payload present.
// The shape test is a MASK, not an equality: a compressed data frame is still
// a data frame (PROTOCOL.md §7.1).
func stDataFrame(epoch, peerEpoch, sid, seq uint32) *drpc.Frame {
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetPeerEpoch(peerEpoch)
	f.SetSid(sid)
	f.SetSeq(seq)
	f.SetPayload([]byte{})
	return f
}

// stIsData reports a data/header frame by SHAPE (flags & SHAPE_MASK == 0),
// so the COMPRESSED modifier never hides a data frame from a filter (§7.1).
func stIsData(f *drpc.Frame) bool {
	const shapeMask = drpc.FlagOpen | drpc.FlagClose | drpc.FlagReset | drpc.FlagPing | drpc.FlagWindow
	return f.GetFlags()&shapeMask == 0 && f.HasPayload()
}

// ---------------------------------------------------------------------------
// §14 gRPC parity: the stats.Handler event stream of a unary call.
// ---------------------------------------------------------------------------

func TestStats_HandlerUnaryParity(t *testing.T) {
	bubble(t, func(t *testing.T) {
		hc, hs := &stHandler{}, &stHandler{}
		client, stop := PipeOption{
			ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true), drpc.WithStatsHandler(hc)},
			ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), drpc.WithStatsHandler(hs)},
		}.Use(t)
		defer stop()

		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "Royale with Cheese",
			CircularShift: 3,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "ale with CheeseRoy", res.GetMessage())

		synctest.Wait()

		// --- client ---------------------------------------------------------
		// End is the RPC's last event, after the response was delivered —
		// gRPC's own guarantee, which is why the terminal pair is emitted by
		// Invoke rather than by the receive path.
		x.Equal(t, []string{"Begin", "OutHeader", "OutPayload", "InPayload", "InTrailer", "End"},
			hc.kinds())
		x.Equal(t, []string{echo.EchoService_Once_FullMethodName}, hc.methods())

		begin := hc.find(t, "Begin", 0).(*stats.Begin)
		x.True(t, begin.Client, "client events must carry Client=true")
		x.False(t, begin.IsClientStream, "unary is not a client stream")
		x.False(t, begin.IsServerStream, "unary is not a server stream")
		x.False(t, begin.BeginTime.IsZero(), "Begin must be stamped")

		outHdr := hc.find(t, "OutHeader", 0).(*stats.OutHeader)
		x.True(t, outHdr.Client, "client events must carry Client=true")
		x.Equal(t, echo.EchoService_Once_FullMethodName, outHdr.FullMethod)

		outPay := hc.find(t, "OutPayload", 0).(*stats.OutPayload)
		x.True(t, outPay.Client, "client events must carry Client=true")
		req, err := proto.Marshal(echo.EchoRequest_builder{
			Message:       "Royale with Cheese",
			CircularShift: 3,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, len(req), outPay.Length)
		x.Equal(t, len(req), outPay.WireLength) // uncompressed call: equal (§12.1)

		inPay := hc.find(t, "InPayload", 0).(*stats.InPayload)
		x.True(t, inPay.Client, "client events must carry Client=true")
		got, ok := inPay.Payload.(*echo.EchoResponse)
		x.True(t, ok, "InPayload carries the unmarshaled message")
		x.Equal(t, "ale with CheeseRoy", got.GetMessage())

		x.True(t, hc.find(t, "InTrailer", 0).(*stats.InTrailer).Client, "InTrailer must be a client event")

		end := hc.find(t, "End", 0).(*stats.End)
		x.True(t, end.Client, "client events must carry Client=true")
		x.NoError(t, end.Error, "a successful call ends without an error")
		x.False(t, end.EndTime.Before(end.BeginTime), "End must be stamped after Begin")

		// --- server ---------------------------------------------------------
		// Every server event is emitted from the open path and then the
		// handler goroutine, in order: exact sequence, no exceptions.
		x.Equal(t, []string{"Begin", "InHeader", "InPayload", "OutPayload", "OutTrailer", "End"}, hs.kinds())
		x.Equal(t, []string{echo.EchoService_Once_FullMethodName}, hs.methods())

		sBegin := hs.find(t, "Begin", 0).(*stats.Begin)
		x.False(t, sBegin.Client, "server events must carry Client=false")
		x.False(t, sBegin.IsClientStream, "unary is not a client stream")

		inHdr := hs.find(t, "InHeader", 0).(*stats.InHeader)
		x.Equal(t, echo.EchoService_Once_FullMethodName, inHdr.FullMethod)
		x.Equal(t, len(req), inHdr.WireLength) // the unary request rides the OPEN (§8)
		x.Equal(t, "", inHdr.Compression)      // no compressor named on the OPEN (§12.1)

		sIn := hs.find(t, "InPayload", 0).(*stats.InPayload)
		x.False(t, sIn.Client, "server events must carry Client=false")
		x.Equal(t, len(req), sIn.Length)

		sOut := hs.find(t, "OutPayload", 0).(*stats.OutPayload)
		x.False(t, sOut.Client, "server events must carry Client=false")

		sEnd := hs.find(t, "End", 0).(*stats.End)
		x.False(t, sEnd.Client, "server events must carry Client=false")
		x.NoError(t, sEnd.Error, "a successful call ends without an error")
	})
}

// ---------------------------------------------------------------------------
// §8 / §11 / §14: a unary SendHeader flushes an H frame at once, and the
// stats.Handler sees it as an OutHeader — between the request and the
// response, exactly where grpc-go reports it. That the core's OWN creation
// ack is not a flush is pinned by TestStats_HandlerBidiParity: its server
// sequence carries no OutHeader at all, though an ack H went on the wire.
// ---------------------------------------------------------------------------

func TestStats_HandlerUnarySendHeaderReportsOutHeader(t *testing.T) {
	bubble(t, func(t *testing.T) {
		hs := &stHandler{}
		client, stop := PipeOption{
			ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true)},
			ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), drpc.WithStatsHandler(hs)},
		}.Use(t)
		defer stop()

		// The echo handler only touches headers when the call carries request
		// metadata; with it, SendHeader flushes before the response exists.
		ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("foo", "bar"))
		_, err := client.Once(ctx, &echo.EchoRequest{})
		x.NoError(t, err)
		synctest.Wait()

		x.Equal(t, []string{"Begin", "InHeader", "InPayload", "OutHeader", "OutPayload", "OutTrailer", "End"}, hs.kinds())

		out := hs.find(t, "OutHeader", 0).(*stats.OutHeader)
		x.False(t, out.Client, "server events must carry Client=false")
		x.Equal(t, echo.EchoService_Once_FullMethodName, out.FullMethod)
		x.Equal(t, []string{"header"}, out.Header.Get("timing"))

		in := hs.find(t, "InHeader", 0).(*stats.InHeader)
		x.Equal(t, []string{"bar"}, in.Header.Get("foo"))

		trailer := hs.find(t, "OutTrailer", 0).(*stats.OutTrailer)
		x.Equal(t, []string{"trailer"}, trailer.Trailer.Get("timing"))
	})
}

// ---------------------------------------------------------------------------
// §14 gRPC parity: a bidi-streaming call reports one payload event per
// message, in order, on both sides. The interleaving is driven by the test
// (send, recv, send, recv, half-close), so the whole sequence is exact.
// ---------------------------------------------------------------------------

func TestStats_HandlerBidiParity(t *testing.T) {
	bubble(t, func(t *testing.T) {
		hc, hs := &stHandler{}, &stHandler{}
		client, stop := PipeOption{
			ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true), drpc.WithStatsHandler(hc)},
			ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), drpc.WithStatsHandler(hs)},
		}.Use(t)
		defer stop()

		stream, err := client.Live(t.Context())
		x.NoError(t, err)

		for _, msg := range []string{"one", "two"} {
			err = stream.Send(echo.EchoRequest_builder{Message: msg, Repeat: 1}.Build())
			x.NoError(t, err)
			res, err := stream.Recv()
			x.NoError(t, err)
			x.Equal(t, msg, res.GetMessage())
		}
		x.NoError(t, stream.CloseSend())
		_, err = stream.Recv()
		x.ErrorIs(t, err, io.EOF)
		synctest.Wait()

		// Client: Begin/OutHeader ride the eager OPEN, then one OutPayload per
		// Send and one InPayload per Recv, then the terminal pair.
		x.Equal(t, []string{
			"Begin", "OutHeader",
			"OutPayload", "InPayload",
			"OutPayload", "InPayload",
			"InTrailer", "End",
		}, hc.kinds())

		begin := hc.find(t, "Begin", 0).(*stats.Begin)
		x.True(t, begin.Client, "client events must carry Client=true")
		x.True(t, begin.IsClientStream, "bidi is a client stream")
		x.True(t, begin.IsServerStream, "bidi is a server stream")
		x.True(t, hc.find(t, "InPayload", 1).(*stats.InPayload).Client, "client events must carry Client=true")
		x.NoError(t, hc.find(t, "End", 0).(*stats.End).Error)

		// Server: the mirror image, all of it on the handler goroutine.
		x.Equal(t, []string{
			"Begin", "InHeader",
			"InPayload", "OutPayload",
			"InPayload", "OutPayload",
			"OutTrailer", "End",
		}, hs.kinds())

		sBegin := hs.find(t, "Begin", 0).(*stats.Begin)
		x.False(t, sBegin.Client, "server events must carry Client=false")
		x.True(t, sBegin.IsClientStream, "bidi is a client stream")
		x.True(t, sBegin.IsServerStream, "bidi is a server stream")

		inHdr := hs.find(t, "InHeader", 0).(*stats.InHeader)
		x.Equal(t, echo.EchoService_Live_FullMethodName, inHdr.FullMethod)
		x.Equal(t, 0, inHdr.WireLength) // the bidi OPEN is eager and bare (§8)
		x.False(t, hs.find(t, "OutPayload", 1).(*stats.OutPayload).Client, "server events must carry Client=false")
		x.NoError(t, hs.find(t, "End", 0).(*stats.End).Error)
	})
}

// ---------------------------------------------------------------------------
// §14 gRPC parity: a failed call reports the status on End.Error at BOTH
// ends, and reports no payload event that never happened — the handler
// produced no response, so there is no OutPayload and no InPayload.
// ---------------------------------------------------------------------------

func TestStats_HandlerEndCarriesError(t *testing.T) {
	bubble(t, func(t *testing.T) {
		hc, hs := &stHandler{}, &stHandler{}
		client, stop := PipeOption{
			ConnOpts:   []drpc.ConnOption{drpc.WithReliable(true), drpc.WithStatsHandler(hc)},
			ServerOpts: []drpc.ServerOption{drpc.WithReliable(true), drpc.WithStatsHandler(hs)},
		}.Use(t)
		defer stop()

		client.service.Err = status.Error(codes.OutOfRange, "foo")
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.Equal(t, codes.OutOfRange, status.Code(err))
		synctest.Wait()

		x.Equal(t, []string{"Begin", "OutHeader", "OutPayload", "InTrailer", "End"}, hc.kinds())
		end := hc.find(t, "End", 0).(*stats.End)
		x.True(t, end.Client, "client events must carry Client=true")
		x.Error(t, end.Error, "a failed call must report its status on End.Error")
		x.Equal(t, codes.OutOfRange, status.Code(end.Error))

		x.Equal(t, []string{"Begin", "InHeader", "InPayload", "OutTrailer", "End"}, hs.kinds())
		sEnd := hs.find(t, "End", 0).(*stats.End)
		x.False(t, sEnd.Client, "server events must carry Client=false")
		x.Error(t, sEnd.Error, "the server reports the handler's status on End.Error")
		x.Equal(t, codes.OutOfRange, status.Code(sEnd.Error))
	})
}

// ---------------------------------------------------------------------------
// §14 gap visibility: the receiver counts SKIPPED messages per stream from
// the seq deltas and exposes the count — this is the counter §14 promises to
// applications that react to gaps. Driven by a lossy filter that eats every
// third server data frame; the count must be the number of messages lost, not
// the number of gaps, and each event must name the call it belongs to.
// ---------------------------------------------------------------------------

func TestStats_CountersSkippedGaps(t *testing.T) {
	bubble(t, func(t *testing.T) {
		counters := &drpc.Counters{}
		log := &stEventLog{}

		// Every third server DATA frame (shape 0 + payload — never the H ack,
		// never the terminal) is dropped: 3 of the 9 responses vanish.
		dropEveryThird := func(next drpc.FrameHandler) drpc.FrameHandler {
			var n atomic.Int64
			return lossy.New(next, lossy.Options{
				Drop:   1,
				Filter: func(f *drpc.Frame) bool { return stIsData(f) && n.Add(1)%3 == 0 },
			})
		}
		pipe := unreliablePipe(nil, dropEveryThird)
		pipe.ConnOpts = append(pipe.ConnOpts, drpc.WithProtocolStats(counters), drpc.WithProtocolStats(log))
		client, stop := pipe.Use(t)
		defer stop()

		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
			Repeat:        9,
		}.Build())
		x.NoError(t, err)

		var seqs []uint32
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err) // a gap is never an error (§14 subsequence)
			seqs = append(seqs, res.GetSequence())
		}
		synctest.Wait()

		// Six of nine delivered, in order, and the three that were dropped are
		// exactly what the counter reports.
		x.Equal(t, []uint32{0, 1, 3, 4, 6, 7}, seqs)
		x.Equal(t, uint64(3), counters.Snapshot().Skipped)

		// One event per gap, each naming the stream it happened on (§14).
		evs := log.of(drpc.EventSkipped)
		x.Equal(t, 3, len(evs))
		for _, ev := range evs {
			x.Equal(t, uint32(1), ev.Count) // messages this gap ate
			x.Equal(t, uint32(1), ev.Sid)   // the first call of the Conn
			x.Equal(t, echo.EchoService_Many_FullMethodName, ev.Method)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.3 / §14: a window overrun (K_loud consistent beyond-window frames) is
// the one loss that is NOT silent — the call fails DATA_LOSS, and the event
// names the call so an operator can tell a loud failure from a quiet gap.
// ---------------------------------------------------------------------------

func TestStats_CountersDataLoss(t *testing.T) {
	rec := &dlRecorder{}
	counters := &drpc.Counters{}
	log := &stEventLog{}
	c, stream, cEpoch, sid := stManyStream(t, rec,
		drpc.WithReliable(false), // window mode: gaps are expected, overruns are not
		drpc.WithProtocolStats(counters), drpc.WithProtocolStats(log))

	// One accepted frame locks the stream to the server incarnation (L = 1)
	// and is delivered normally...
	x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, 1, "one")))
	got := &echo.EchoResponse{}
	x.NoError(t, stream.RecvMsg(got))
	x.Equal(t, "one", got.GetMessage())

	// ...then kLoud = 3 mutually consistent beyond-window frames (§6.3).
	for _, seq := range []uint32{5000, 5001, 5002} {
		x.NoError(t, c.Handle(t.Context(), dlServerData(cEpoch, sid, seq, "lost")))
	}

	x.Equal(t, codes.DataLoss, status.Code(stream.RecvMsg(&echo.EchoResponse{})))
	x.Equal(t, uint64(1), counters.Snapshot().DataLoss)
	x.Equal(t, uint64(0), counters.Snapshot().Skipped, "an overrun is a loud failure, not a gap")

	ev := log.first(t, drpc.EventDataLoss)
	x.Equal(t, sid, ev.Sid)
	x.Equal(t, echo.EchoService_Many_FullMethodName, ev.Method)
}

// ---------------------------------------------------------------------------
// §9.3 / §14: RESETs are counted on both ends — sent (this endpoint disowned
// a call) and received (the peer disowned ours). The server-side event names
// the TRANSPORT PEER it answered, which is what makes a RESET storm
// attributable (§15).
// ---------------------------------------------------------------------------

func TestStats_CountersResetAccounting(t *testing.T) {
	t.Run("server counts the RESET it volunteers", func(t *testing.T) {
		counters := &drpc.Counters{}
		log := &stEventLog{}
		is := newInjectServer(t, drpc.WithProtocolStats(counters), drpc.WithProtocolStats(log))

		// A data frame for a sid the server never opened: reliable channels
		// have no reordering, so the RESET is immediate (§9.3).
		const peer = "peer-reset"
		f := stDataFrame(0xC0FFEE, 0, 42, 2)
		x.NoError(t, is.srv.Handle(drpc.NewPeerContext(context.Background(), peer), f))

		r := is.recv(t)
		x.True(t, r != nil, "expected a RESET")
		x.Equal(t, drpc.FlagReset, r.GetFlags())

		x.Equal(t, uint64(1), counters.Snapshot().ResetSent)
		x.Equal(t, uint64(0), counters.Snapshot().ResetReceived)
		ev := log.first(t, drpc.EventResetSent)
		x.Equal(t, uint32(42), ev.Sid)
		x.Equal(t, peer, ev.Peer)
	})
	t.Run("client counts both directions", func(t *testing.T) {
		rec := &dlRecorder{}
		counters := &drpc.Counters{}
		log := &stEventLog{}
		c, stream, cEpoch, sid := stManyStream(t, rec,
			drpc.WithReliable(true), drpc.WithProtocolStats(counters), drpc.WithProtocolStats(log))

		// A server frame for a sid this client never opened: no OPEN can ever
		// arrive at a client, so it answers with a RESET at once (§9.3).
		x.NoError(t, c.Handle(t.Context(), stDataFrame(7, cEpoch, sid+7, 1)))
		x.Equal(t, uint64(1), counters.Snapshot().ResetSent)
		x.Equal(t, sid+7, log.first(t, drpc.EventResetSent).Sid)

		// And the mirror: a RESET for the live call, echoing this client
		// incarnation (§6.1) so it is ours to act on.
		reset := &drpc.Frame{}
		reset.SetFlags(drpc.FlagReset)
		reset.SetEpoch(cEpoch)
		reset.SetSid(sid)
		reset.SetPeerEpoch(cEpoch)
		x.NoError(t, c.Handle(t.Context(), reset))

		x.Equal(t, uint64(1), counters.Snapshot().ResetReceived)
		// A RESET is stateless and endpoint-scope: it names the sid it killed,
		// never a method (the frame carries none).
		x.Equal(t, sid, log.first(t, drpc.EventResetReceived).Sid)

		// The RESET was acted on, not merely counted.
		x.Equal(t, codes.Unavailable, status.Code(stream.RecvMsg(&echo.EchoResponse{})))
	})
}

// ---------------------------------------------------------------------------
// §10.3 / §9.2 / §14: under loss the client RETRANSMITS its control frames
// and the server REPLAYS the stored terminal — both are counted, and the call
// still succeeds. The response terminal is dropped once, so the OPEN's
// retransmission obligation is never cleared and the tombstone answers it.
// ---------------------------------------------------------------------------

func TestStats_CountersRetransmitUnderLoss(t *testing.T) {
	bubble(t, func(t *testing.T) {
		cc, sc := &drpc.Counters{}, &drpc.Counters{}
		log := &stEventLog{}
		pipe := unreliablePipe(nil, dropFirst(isTerminal), drpc.WithProtocolStats(sc))
		pipe.ConnOpts = append(pipe.ConnOpts, drpc.WithProtocolStats(cc), drpc.WithProtocolStats(log))
		client, stop := pipe.Use(t)
		defer stop()

		res, err := client.Once(t.Context(), echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err) // recovered by retransmission + replay
		x.Equal(t, "bca", res.GetMessage())
		synctest.Wait()

		x.True(t, cc.Snapshot().Retransmit > 0, "the lost terminal must leave the OPEN retransmitting (§10.3)")
		x.True(t, sc.Snapshot().TombstoneReplay > 0, "the duplicate OPEN must draw a tombstone replay (§9.2)")
		x.Equal(t, uint32(1), log.first(t, drpc.EventRetransmit).Sid)
	})
}

// ---------------------------------------------------------------------------
// §9.2 / §14: the tombstone replay event names the transport peer and the sid
// it answered — the per-peer view §15 needs to see one peer poking finished
// calls. Driven by a duplicate OPEN spaced past the per-tombstone 1/RTI
// replay limit.
// ---------------------------------------------------------------------------

func TestStats_CountersTombstoneReplayNamesPeer(t *testing.T) {
	counters := &drpc.Counters{}
	log := &stEventLog{}
	is := newInjectServerMode(t, false, // tombstones live in unreliable mode (§10.6)
		drpc.WithTiming(fastTiming), drpc.WithProtocolStats(counters), drpc.WithProtocolStats(log))

	const peer = "peer-tomb"
	const epoch uint32 = 0x9A
	ctx := drpc.NewPeerContext(context.Background(), peer)
	open := openFrame(epoch, 3, 1, echo.EchoService_Once_FullMethodName)

	x.NoError(t, is.srv.Handle(ctx, proto.CloneOf(open)))
	term := is.recv(t)
	x.True(t, term != nil, "expected the call's terminal")
	x.Equal(t, codes.OK, codes.Code(term.GetCode()))
	x.Equal(t, uint64(0), counters.Snapshot().TombstoneReplay)

	// Past the per-tombstone replay limit (1/RTI, §9.2) the duplicate draws
	// the stored terminal back.
	time.Sleep(2 * fastTiming.Retransmit)
	x.NoError(t, is.srv.Handle(ctx, proto.CloneOf(open)))
	replay := is.recv(t)
	x.True(t, replay != nil, "expected the stored terminal to be replayed")
	x.True(t, proto.Equal(term, replay), "the replay is the stored terminal")

	x.Equal(t, uint64(1), counters.Snapshot().TombstoneReplay)
	ev := log.first(t, drpc.EventTombstoneReplay)
	x.Equal(t, uint32(3), ev.Sid)
	x.Equal(t, peer, ev.Peer)
}

// ---------------------------------------------------------------------------
// §10.4 / §10.5 / §14: an idle call is kept alive by peer keepalives and
// stream probes — both counted, on both endpoints — and when the peer really
// vanishes, the liveness window expiring is counted too (and is what fails
// the call).
// ---------------------------------------------------------------------------

func TestStats_CountersProbeKeepaliveLiveness(t *testing.T) {
	bubble(t, func(t *testing.T) {
		cc, sc := &drpc.Counters{}, &drpc.Counters{}
		ks := &killswitch{}
		s2c := func(next drpc.FrameHandler) drpc.FrameHandler {
			ks.next = next
			return ks
		}
		pipe := unreliablePipe(nil, s2c, drpc.WithProtocolStats(sc))
		pipe.ConnOpts = append(pipe.ConnOpts, drpc.WithProtocolStats(cc))
		client, stop := pipe.Use(t)
		defer stop()

		stream, err := client.Live(t.Context())
		x.NoError(t, err)
		x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build()))
		_, err = stream.Recv()
		x.NoError(t, err)

		// Idle past T_probe (T_live/3 = 200ms) with zero application traffic:
		// the endpoints keepalive each other and probe the stream.
		time.Sleep(fastTiming.Liveness)
		x.True(t, cc.Snapshot().KeepaliveSent > 0, "an idle client must send peer keepalives (§10.4)")
		x.True(t, cc.Snapshot().ProbeSent > 0, "an idle client must probe its stream (§10.5)")
		x.True(t, sc.Snapshot().KeepaliveSent > 0, "an idle server must send peer keepalives (§10.4)")
		x.True(t, sc.Snapshot().ProbeSent > 0, "an idle server must probe its stream (§10.5)")
		x.Equal(t, uint64(0), cc.Snapshot().LivenessExpired, "a healthy peer never expires")

		// The server machine disappears: nothing reaches the client any more.
		ks.dead.Store(true)
		_, err = stream.Recv()
		x.Equal(t, codes.Unavailable, status.Code(err))
		x.Equal(t, uint64(1), cc.Snapshot().LivenessExpired)

		// The client is now silent too, so the server's own window expires and
		// reclaims the handler (§10.4).
		time.Sleep(2 * fastTiming.Liveness)
		x.Equal(t, uint64(1), sc.Snapshot().LivenessExpired)
	})
}

// ---------------------------------------------------------------------------
// §4.2 flow control (reliable mode): a sender that runs out of credit PARKS,
// and the stall is observable WHILE it is parked — a stall counter that only
// reported after the fact would be useless for the condition it names. The
// resume is a separate event, so a stuck sender is distinguishable from one
// that recovered.
// ---------------------------------------------------------------------------

func TestStats_CountersFlowStall(t *testing.T) {
	bubble(t, func(t *testing.T) {
		counters := &drpc.Counters{}
		log := &stEventLog{}
		gate := make(chan struct{})
		client, stop := PipeOption{
			ConnOpts: []drpc.ConnOption{
				drpc.WithReliable(true),
				drpc.WithProtocolStats(counters), drpc.WithProtocolStats(log),
			},
			ServerOpts: []drpc.ServerOption{
				drpc.WithReliable(true),
				drpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
					<-gate
					return handler(srv, ss)
				}),
			},
		}.Use(t)
		defer stop()

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)

		// The window is the peer's rx buffer, 32 by default (Appendix B) and
		// floored there in reliable mode, so message 33 has no credit.
		const n = 40
		sent := make(chan error, 1)
		go func() {
			for range n {
				if err := stream.Send(echo.EchoRequest_builder{Message: "m", Repeat: 1}.Build()); err != nil {
					sent <- err
					return
				}
			}
			sent <- nil
		}()

		// With the handler gated shut nothing drains the buffer: the sender is
		// parked right now, and the stall is already visible.
		synctest.Wait()
		x.Equal(t, uint64(1), counters.Snapshot().FlowStall)
		x.Equal(t, 0, len(log.of(drpc.EventFlowResume)), "a parked sender has not resumed")

		ev := log.first(t, drpc.EventFlowStall)
		x.Equal(t, uint32(1), ev.Sid)
		x.Equal(t, echo.EchoService_Buff_FullMethodName, ev.Method)

		// Draining the buffer grants credit, which unparks the sender.
		close(gate)
		x.NoError(t, <-sent)
		res, err := stream.CloseAndRecv()
		x.NoError(t, err)
		x.Equal(t, n, len(res.GetItems()))
		for i, item := range res.GetItems() {
			x.Equal(t, uint32(i), item.GetSequence()) // exact sequence (§14)
		}

		x.True(t, len(log.of(drpc.EventFlowResume)) > 0, "credit must resume the parked sender")
		x.Equal(t, echo.EchoService_Buff_FullMethodName, log.first(t, drpc.EventFlowResume).Method)
	})
}

// ---------------------------------------------------------------------------
// §4.2 / §14: the rx buffer's DROP POLICY discards messages in unreliable
// mode, and §14 promises that loss is counted ("per-stream drop ... counters"
// alongside the skipped counter) — a drop is otherwise invisible: the frame
// was accepted by the seq window, so it leaves no gap either.
// ---------------------------------------------------------------------------

func TestStats_CountersDroppedRxPolicy(t *testing.T) {
	counters := &drpc.Counters{}
	log := &stEventLog{}
	bubble(t, func(t *testing.T) {
		pipe := unreliablePipe(nil, nil)
		pipe.ConnOpts = append(pipe.ConnOpts,
			drpc.WithRxBuffer(2, drpc.DropNewest),
			drpc.WithProtocolStats(counters), drpc.WithProtocolStats(log))
		client, stop := pipe.Use(t)
		defer stop()

		const produced = 10
		stream, err := client.Many(t.Context(), echo.EchoRequest_builder{
			Message: "m",
			Repeat:  produced,
		}.Build())
		x.NoError(t, err)

		// Let the whole response burst arrive before anything is consumed: the
		// 2-slot buffer keeps the first two and the drop policy discards the
		// rest (unreliable mode never blocks the wire, §4.2).
		synctest.Wait()

		var seqs []uint32
		for {
			res, err := stream.Recv()
			if err == io.EOF {
				break
			}
			x.NoError(t, err)
			seqs = append(seqs, res.GetSequence())
		}
		x.Equal(t, []uint32{0, 1}, seqs) // DropNewest keeps the buffered prefix

		// The loss is invisible everywhere else: the frames were accepted by
		// the window, so they left no gap.
		x.Equal(t, uint64(0), counters.Snapshot().Skipped)
		x.Equal(t, 0, len(log.of(drpc.EventSkipped)))
	})

	// Asserted outside the bubble so a failure lands on this test, not on the
	// bubble's inner T.
	x.Equal(t, uint64(8), counters.Snapshot().Dropped) // 10 produced, 2 buffered
	evs := log.of(drpc.EventDropped)
	x.Equal(t, 8, len(evs))
	x.Equal(t, uint32(1), evs[0].Sid)
	x.Equal(t, echo.EchoService_Many_FullMethodName, evs[0].Method)
}
