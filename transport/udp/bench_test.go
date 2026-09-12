package udp_test

// Envelop batching, measured (issue #5) — the entry condition docs/TODO.md §1
// puts in front of the `Coalescer`, not the feature. Two questions, both
// answerable without writing a Coalescer:
//
//	what one message costs on the UDP send path today, and what a k-frame
//	Envelop (§4.1 has always allowed 1..n) would save of it.
//
// Six sub-benchmarks per payload size:
//
//	Marshal/<size>       the proto.Marshal of a one-frame envelop, isolated
//	Handle/<size>        Transport.Handle as it stands: that marshal + one write
//	RawWrite/<size>      net.Conn.Write of the same byte count — the floor a
//	                     perfect adapter could reach, i.e. the bare syscall
//	CoreSend/<size>      one SendMsg with an adapter that does nothing — the
//	                     core's half of sending a message
//	ClientSend/<size>    the same SendMsg through this adapter — what one
//	                     message costs to send, whole, so that Handle can be
//	                     read as a share of a measured total
//	Batch/<size>/k=<n>   one Send of an n-frame envelop (batched) against n
//	                     calls of Handle (unbatched), n frames per op either way
//
// Batch is the *upper bound* of what a Coalescer could save, without writing
// one: the receive side already takes n frames per datagram (udp.go serve →
// drpc.Unpack), so a hand-built k-frame envelop is a legal wire message today
// and only the sender-side policy is missing (§4.1).
//
// The byte metrics are reported by every sub-benchmark that puts a datagram on
// the wire, and all follow from the one marshaled envelop length, so a reader
// can redo them by hand:
//
//	wireB/frame     (28 B × datagrams + marshaled bytes) / frames
//	hdrB/frame      (marshaled bytes − frames × payload) / frames — the Frame
//	                header is per frame (epoch, sid, seq, payload tag+len,
//	                envelop tag+len), so batching does NOT amortise it
//	ipudpB/frame    28 B × datagrams / frames — the IPv4+UDP header is per
//	                datagram, so batching DOES amortise it
//	wireB/payloadB  wire bytes per payload byte
//
// Everything is the client send path: Transport, not Gateway. The two marshal
// the same envelop and differ only in the write (Write vs WriteToUDPAddrPort,
// connected vs addressed) and in the frame — a server→client data frame
// carries peer_epoch as well, +5 B per frame (fixed32 field 14, §6.1), which
// is arithmetic on hdrB/frame rather than a second benchmark.
//
// This wants one careful run on a quiet machine: every op here is a syscall,
// and a shared runner measures its neighbours. benchtable_test.go turns the
// output into the table issue #5 asks for.
//
// # Decision rule
//
// Written down before the run, from issue #5, so the result cannot be argued
// afterwards — quoted, not paraphrased:
//
//	batching is a go only if, at ≤ 1k msg/s and ≤ 100 B (the example's own
//	200 Hz / ~20 B with headroom, on what transport/udp/README.md:5 calls the
//	sensor-stream path and :61-62 says to keep small), the transport share is
//	a majority of per-message cost or of wire bytes, and the MaxDelay needed
//	to reach a k that recovers most of it is below the reading's useful life.
//	Otherwise close TODO §1 with the table as the reason.
//
// The price column of that table is just MaxDelay: §10.7 adds it to every
// bound, in both directions.

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"testing"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/udp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// ipUDPHeaderBytes is what every datagram pays under the Envelop on IPv4:
// 20 B IP + 8 B UDP. It is arithmetic, not a measurement — named here because
// it is the one overhead a batch amortises, and because it is invisible to
// anything the adapter can observe.
const ipUDPHeaderBytes = 28

// benchEpoch stands in for the incarnation nonce (§6.1). epoch, sid and seq
// are fixed32 (frame.pb.go), so their values never change a frame's size —
// only their presence does, and a real data frame has all three nonzero.
const benchEpoch = 0x9E3779B9

// sink keeps the results of timed calls live: nothing measured here may be
// eliminated as dead code.
var sink int

// batchKs are the batch sizes worth a row. The largest k that still fits the
// send budget is added per payload size, since that is the best case.
var batchKs = []int{2, 4, 8, 16, 32}

type benchPayload struct {
	name string
	data []byte
}

// benchPayloads is issue #5's grid: what the example actually sends, then
// 100 B and 1 KiB. Names carry the measured size, not a rounded one.
func benchPayloads() []benchPayload {
	ps := []benchPayload{
		{data: sensorReading()},
		{data: make([]byte, 100)},
		{data: make([]byte, 1024)},
	}
	for i := range ps {
		ps[i].name = fmt.Sprintf("%dB", len(ps[i].data))
	}
	return ps
}

// sensorReading is one examples/udp-sensor Reading on the wire: seq (varint,
// field 1), celsius (fixed64, 2), unix_nanos (varint, 3) — sensor.proto:20-26.
// The example is its own module and cannot be imported here, so the fields are
// appended in field order, which is byte for byte what proto.Marshal of a
// Reading produces: 22 B at the example's 200 Hz (21 B while seq < 128, 23 B
// once it passes 16383; 10 B of it is the nanosecond timestamp, a tag and a
// 9-byte varint). Measured against the real message, not assumed to be the
// ~20 B the issue estimates.
func sensorReading() []byte {
	const (
		seq   = 200                 // ~1 s into a 200 Hz feed (main.go: -hz 200)
		nanos = 1757638800000000000 // 2025-09-12T01:00:00Z; a wall clock only grows
	)
	b := protowire.AppendTag(nil, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, seq)
	b = protowire.AppendTag(b, 2, protowire.Fixed64Type)
	b = protowire.AppendFixed64(b, math.Float64bits(21.5))
	b = protowire.AppendTag(b, 3, protowire.VarintType)
	b = protowire.AppendVarint(b, nanos)
	return b
}

// benchFrame is a client data frame exactly as clientStream.nextFrame builds
// one (stream.go): epoch, sid, seq, payload. No peer_epoch — client→server
// frames leave it 0 (§6.1) — and no method/flags, which ride the OPEN once per
// call and would flatter a per-message number.
func benchFrame(sid, seq uint32, payload []byte) *drpc.Frame {
	return drpc.Frame_builder{
		Epoch:   benchEpoch,
		Sid:     sid,
		Seq:     seq,
		Payload: payload,
	}.Build()
}

// benchEnvelop packs k such frames into one envelop, the way a Coalescer would
// have to: distinct seq per frame, one stream (§4.1).
func benchEnvelop(k int, payload []byte) *drpc.Envelop {
	frames := make([]*drpc.Frame, k)
	for i := range frames {
		frames[i] = benchFrame(1, uint32(i+1), payload)
	}
	e := &drpc.Envelop{}
	e.SetFrames(frames)
	return e
}

// marshalLen is the marshaled size of e the way udp.go marshals it. The byte
// metrics are all derived from this one number.
func marshalLen(b *testing.B, e *drpc.Envelop) int {
	b.Helper()
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(e)
	if err != nil {
		b.Fatal(err)
	}
	return len(data)
}

// benchMaxFrames is the largest k whose marshaled envelop still fits the send
// limit; over it Send refuses with ErrMessageTooLarge (udp.go, §4.4). At 1 KiB
// it is 1 — batching is a small-message feature by construction.
func benchMaxFrames(b *testing.B, payload []byte, limit int) int {
	b.Helper()
	k := 0
	for marshalLen(b, benchEnvelop(k+1, payload)) <= limit {
		k++
	}
	return k
}

// benchConn is a connected loopback socket whose datagrams one goroutine
// drains. Without that reader the receive buffer fills and the sender starts
// paying for the receiver's backlog — the benchmark would time the wrong
// thing. The reader only discards: unmarshaling here would put the receive
// side on the sender's bill.
func benchConn(b *testing.B) net.Conn {
	b.Helper()

	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		b.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		buf := make([]byte, 65535) // the largest UDP payload, as udp.go reads
		for {
			if _, err := pc.Read(buf); err != nil {
				return
			}
		}
	}()

	c, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		pc.Close()
		<-drained
		b.Fatal(err)
	}
	b.Cleanup(func() {
		c.Close()
		pc.Close()
		<-drained
	})
	return c
}

// benchMessage is a proto message whose marshaled form is exactly n bytes, so
// the frame CoreSend and ClientSend put through the core carries the payload
// the other sub-benchmarks put on the wire, byte for byte. EchoRequest.message is field 2, a string:
// one tag byte, a length varint, then the bytes — the loop finds the string
// length instead of case-splitting on where that varint grows.
func benchMessage(b *testing.B, n int) *echo.EchoRequest {
	b.Helper()
	for l := n; l >= 0; l-- {
		m := echo.EchoRequest_builder{Message: strings.Repeat("x", l)}.Build()
		if proto.Size(m) == n {
			return m
		}
	}
	b.Fatalf("no EchoRequest marshals to exactly %d B", n)
	return nil
}

// benchStream is one open client-streaming call over tx. Unreliable is the
// mode udp.Transport puts a Conn in (Reliable() false, §4.3) and the mode the
// sensor example runs; it is also the mode without flow control (§4.2), so no
// op can park on a window the benchmark would then be timing. The mode is set
// here rather than discovered so that CoreSend's discarding tx runs in the one
// udp.Transport announces.
//
// The call is client-streaming so that its OPEN is eager (§8) and sent here,
// in setup: every timed op is a data frame, which is what a Coalescer batches.
// The OPEN keeps retransmitting into the silence (§10.3, RTI 1 s doubling) —
// per call and per second, not per message, so a handful of frames over a run.
func benchStream(b *testing.B, tx drpc.FrameHandler) grpc.ClientStream {
	b.Helper()

	c := drpc.NewConn(tx, drpc.WithReliable(false))
	b.Cleanup(func() { c.Close(nil) })

	s, err := c.NewStream(b.Context(), &grpc.StreamDesc{ClientStreams: true}, echo.EchoService_Buff_FullMethodName)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// reportBytes reports what `frames` frames of `payload` bytes each cost on the
// wire when sent as `datagrams` datagrams totalling `marshaled` bytes of
// Envelop. Split this way because the two overheads behave differently under
// batching: the Frame header is per frame, the IP/UDP header per datagram.
func reportBytes(b *testing.B, datagrams, marshaled, frames, payload int) {
	ipudp := float64(ipUDPHeaderBytes * datagrams)
	wire := ipudp + float64(marshaled)
	b.ReportMetric(wire/float64(frames), "wireB/frame")
	b.ReportMetric((float64(marshaled)-float64(frames*payload))/float64(frames), "hdrB/frame")
	b.ReportMetric(ipudp/float64(frames), "ipudpB/frame")
	b.ReportMetric(wire/float64(frames*payload), "wireB/payloadB")
}

// BenchmarkMarshal times udp.go's marshal call and nothing else: the envelop
// it wraps is built once, so the envelop and the one-frame slice Handle
// allocates per frame stay on Handle's bill. The frame itself is setup on both
// sides — building one is the core's cost, and is in CoreSend.
func BenchmarkMarshal(b *testing.B) {
	for _, p := range benchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			e := benchEnvelop(1, p.data)
			n := marshalLen(b, e)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				data, err := proto.MarshalOptions{Deterministic: true}.Marshal(e)
				if err != nil {
					b.Fatal(err)
				}
				sink += len(data)
			}
			b.StopTimer()

			reportBytes(b, 1, n, 1, len(p.data))
		})
	}
}

// BenchmarkHandle times the send path as it stands: one frame wrapped in a
// one-frame envelop, marshaled, written. This is what sending one message
// costs today, and the number every CPU-share row of the table is built on.
func BenchmarkHandle(b *testing.B) {
	for _, p := range benchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			tr := udp.New(benchConn(b))
			ctx := context.Background()
			f := benchFrame(1, 1, p.data)
			n := marshalLen(b, benchEnvelop(1, p.data))

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := tr.Handle(ctx, f); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()

			reportBytes(b, 1, n, 1, len(p.data))
		})
	}
}

// BenchmarkRawWrite is the floor: the same byte count Handle puts on the wire,
// written straight to the socket. Handle − RawWrite is everything the adapter
// adds on top of the syscall; RawWrite is what no adapter can undercut.
func BenchmarkRawWrite(b *testing.B) {
	for _, p := range benchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			c := benchConn(b)
			data := make([]byte, marshalLen(b, benchEnvelop(1, p.data)))

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n, err := c.Write(data)
				if err != nil {
					b.Fatal(err)
				}
				sink += n
			}
			b.StopTimer()

			reportBytes(b, 1, len(data), 1, len(p.data))
		})
	}
}

// BenchmarkCoreSend is the core's half of a sent message: clientStream.send
// from the codec marshal to the tx call — the flow-control check, the frame,
// the seq — with an adapter that only returns nil. It is the other half of
// ClientSend, so that "the transport share of per-message cost" is a division
// between two measured numbers rather than an assertion.
//
// The root package's BenchmarkServerHandle is not that half, which is why this
// exists: it drives a whole server-side RPC from an OPEN — the rx side, a
// different path (issue #5: "It measures nothing the Coalescer would change").
func BenchmarkCoreSend(b *testing.B) {
	for _, p := range benchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			m := benchMessage(b, len(p.data))
			s := benchStream(b, drpc.FrameHandlerFunc(func(context.Context, *drpc.Frame) error { return nil }))

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.SendMsg(m); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkClientSend is what one message costs to send, whole: the same
// SendMsg as CoreSend, through the real adapter. This is the per-message cost
// the decision rule divides Handle by — measured on one path, not summed
// across two — and CoreSend + Handle is the same quantity reached apart, which
// is the arithmetic the table prints for the reader to check. Its bytes are
// Handle's: the datagram it sends is the one Handle sends.
func BenchmarkClientSend(b *testing.B) {
	for _, p := range benchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			m := benchMessage(b, len(p.data))
			s := benchStream(b, udp.New(benchConn(b)))

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.SendMsg(m); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBatch is the whole question: k frames in one datagram against the
// same k frames in k datagrams. One op is k frames on both sides, so ns/op
// compares directly; ns/frame is reported for reading it next to Handle.
func BenchmarkBatch(b *testing.B) {
	for _, p := range benchPayloads() {
		b.Run(p.name, func(b *testing.B) {
			kMax := benchMaxFrames(b, p.data, udp.DefaultMaxMessageSize)
			if kMax < 2 {
				// Two of these do not fit one datagram, so there is nothing
				// for a Coalescer to coalesce at this size — a result, not a
				// gap in the run.
				b.Skipf("%d B payload: one frame is the whole %d B budget (§4.4)",
					len(p.data), udp.DefaultMaxMessageSize)
			}

			ks := []int{}
			for _, k := range batchKs {
				if k < kMax {
					ks = append(ks, k)
				}
			}
			ks = append(ks, kMax)

			for _, k := range ks {
				b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
					e := benchEnvelop(k, p.data)
					n := marshalLen(b, e)
					n1 := marshalLen(b, benchEnvelop(1, p.data))
					frames := e.GetFrames()

					b.Run("batched", func(b *testing.B) {
						tr := udp.New(benchConn(b))
						ctx := context.Background()

						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							if err := tr.Send(ctx, e); err != nil {
								b.Fatal(err)
							}
						}
						b.StopTimer()

						b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*k), "ns/frame")
						reportBytes(b, 1, n, k, len(p.data))
					})

					b.Run("unbatched", func(b *testing.B) {
						tr := udp.New(benchConn(b))
						ctx := context.Background()

						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							for _, f := range frames {
								if err := tr.Handle(ctx, f); err != nil {
									b.Fatal(err)
								}
							}
						}
						b.StopTimer()

						b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*k), "ns/frame")
						// k datagrams of one frame each: the bytes the adapter
						// actually puts on the wire today, for the same frames.
						reportBytes(b, k, n1*k, k, len(p.data))
					})
				})
			}
		})
	}
}
