package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/examples/udp-sensor/sensorpb"
	"github.com/lesomnus/grpc-dgram/transport/udp"
)

func runClient(ctx context.Context, addr string) error {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}

	// Counters is a ready-made drpc.ProtocolStats: it just counts the
	// datagram-specific events gRPC has no concept of.
	counters := &drpc.Counters{}

	// A drpc.Conn is a grpc.ClientConnInterface. The transport attaches
	// itself, so there is no goroutine to manage, and Close tears down the
	// conn, the transport, and the socket.
	conn := drpc.NewConn(udp.New(c),
		// Freshest-wins: when this consumer lags, the per-stream buffer
		// evicts the oldest reading to admit the newest (PROTOCOL.md §4.2).
		// The server-side twin is WithMethodRxBuffer (see server.go).
		drpc.WithRxBuffer(*rxSize, drpc.DropOldest),
		drpc.WithProtocolStats(counters),
	)
	defer conn.Close(nil)

	client := sensorpb.NewSensorServiceClient(conn)

	// The explicit deadline. A sensor feed has no natural end, so a
	// subscription is a time budget: it rides the OPEN frame and both ends
	// enforce it independently (PROTOCOL.md §10.2).
	ctx, cancel := context.WithTimeout(ctx, *window)
	defer cancel()

	stream, err := client.Readings(ctx, &sensorpb.Subscribe{Hz: uint32(*hz)})
	if err != nil {
		return err
	}

	rep := report{}
	start := time.Now()
	for {
		reading, err := stream.Recv()
		if err != nil {
			rep.end = err
			break
		}
		rep.observe(reading.GetSeq())
		// Pretend the reading is being used for something. A consumer slower
		// than the feed is exactly what the drop policy exists for.
		time.Sleep(*consume)
	}
	rep.elapsed = time.Since(start)
	rep.print(counters.Snapshot())
	return nil
}

// report accounts for the readings that arrived. Because every Reading carries
// its own seq, the application can see the shape of what it lost without any
// help from the protocol: what it receives is an ordered subsequence — never
// reordered, never duplicated, only thinned.
type report struct {
	received   uint64
	first      uint64
	last       uint64
	longestGap uint64
	elapsed    time.Duration
	end        error
}

func (r *report) observe(seq uint64) {
	r.received++
	if r.first == 0 {
		r.first = seq
	}
	if r.last != 0 && seq > r.last+1 {
		if gap := seq - r.last - 1; gap > r.longestGap {
			r.longestGap = gap
		}
	}
	r.last = seq
}

func (r *report) print(c drpc.CounterSnapshot) {
	produced := uint64(0)
	if r.last >= r.first && r.first > 0 {
		produced = r.last - r.first + 1
	}
	missing := produced - r.received
	// Every missing reading was either lost on the wire — which the seq
	// window saw as a gap and counted — or evicted from the rx buffer by
	// DropOldest, which is invisible to the window because the frame did
	// arrive. The difference is the eviction count.
	lost := min(uint64(c.Skipped), missing)
	evicted := missing - lost

	end := "end of stream (io.EOF)"
	if r.end != nil && !errors.Is(r.end, io.EOF) {
		end = r.end.Error()
	}

	fmt.Printf("\n--- subscription report (%s) ---\n", r.elapsed.Round(time.Millisecond))
	fmt.Printf("  stream ended        : %s\n", end)
	fmt.Printf("  readings produced   : %d (seq %d..%d)\n", produced, r.first, r.last)
	fmt.Printf("  readings delivered  : %d (%.1f%%)\n", r.received, 100*float64(r.received)/float64(max(produced, 1)))
	fmt.Printf("  missing             : %d\n", missing)
	fmt.Printf("    lost on the wire  : %d (the §14 gap counter)\n", lost)
	fmt.Printf("    evicted, DropOldest: %d (rx buffer full while this consumer lagged)\n", evicted)
	fmt.Printf("  longest single gap  : %d readings\n", r.longestGap)
	fmt.Printf("  out of order        : 0, by construction — gaps are the only distortion\n")

	fmt.Printf("\ndrpc.Counters (client):\n")
	fmt.Printf("  Skipped %d  Dropped %d  DataLoss %d  OffShape %d\n", c.Skipped, c.Dropped, c.DataLoss, c.OffShape)
	fmt.Printf("  Retransmit %d  ProbeSent %d  KeepaliveSent %d  LivenessExpired %d\n", c.Retransmit, c.ProbeSent, c.KeepaliveSent, c.LivenessExpired)
	fmt.Printf("  ResetSent %d  ResetReceived %d  TombstoneReplay %d  FlowStall %d\n", c.ResetSent, c.ResetReceived, c.TombstoneReplay, c.FlowStall)
}
