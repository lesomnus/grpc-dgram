package main

import (
	"context"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/examples/udp-sensor/sensorpb"
	"github.com/lesomnus/grpc-dgram/transport/udp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// sensor is the service implementation: a fake thermometer that samples at the
// rate the subscriber asked for and stops when the call's context ends. It is
// ordinary gRPC-generated-code work — nothing here knows it runs on datagrams.
type sensor struct {
	sensorpb.UnimplementedSensorServiceServer
	sent atomic.Uint64
}

func (s *sensor) Readings(req *sensorpb.Subscribe, stream grpc.ServerStreamingServer[sensorpb.Reading]) error {
	hz := req.GetHz()
	if hz == 0 {
		hz = 100
	}
	ticker := time.NewTicker(time.Second / time.Duration(hz))
	defer ticker.Stop()

	ctx := stream.Context()
	for seq := uint64(1); ; seq++ {
		select {
		case <-ctx.Done():
			// The client's deadline rode the OPEN frame and this server
			// enforced it on its own clock, without waiting for a frame
			// (PROTOCOL.md §10.2). For an endless feed that is the normal
			// ending, not a failure.
			return status.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}

		err := stream.Send(&sensorpb.Reading{
			Seq:       seq,
			Celsius:   20 + 5*math.Sin(float64(seq)/40),
			UnixNanos: time.Now().UnixNano(),
		})
		if err != nil {
			return err
		}
		s.sent.Add(1)
	}
}

// server bundles what one endpoint owns: the socket, the drpc.Server, and the
// handler whose counter the report reads.
type server struct {
	addr   string
	sensor *sensor

	closer sync.Once
	shut   func()
}

func startServer(ctx context.Context, addr string) (*server, error) {
	laddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}

	gw := udp.NewGateway(pc)
	impl := &sensor{}
	srv := drpc.NewServer(
		// The gateway is the transport; the wrapper only throws datagrams
		// away (see lossy.go). Sending through it is all the server does.
		&lossy{gw: gw, rate: *loss},

		// Sensor tuning: the feed gets a deep, freshest-wins buffer while
		// every other method keeps the default (PROTOCOL.md §4.2).
		drpc.WithMethodRxBuffer(sensorpb.SensorService_Readings_FullMethodName, 64, drpc.DropOldest),
		// Bound the handler goroutines a single peer can spawn (§15).
		drpc.WithLimits(drpc.Limits{MaxLiveCalls: 64}),
	)
	sensorpb.RegisterSensorServiceServer(srv, impl)

	// Registration must precede the first received frame (§13), so the
	// gateway starts only now — the same shape as grpc.Server.Serve(lis).
	serveCtx, cancelServe := context.WithCancel(ctx)
	go func() { _ = gw.Serve(serveCtx, srv) }()

	return &server{
		addr:   pc.LocalAddr().String(),
		sensor: impl,
		shut: func() {
			cancelServe()
			// GracefulStop waits for in-flight handlers, so the sample count
			// is final once it returns; the socket goes last.
			srv.GracefulStop()
			pc.Close()
		},
	}, nil
}

// stop tears the server down; safe to call more than once.
func (s *server) stop() { s.closer.Do(s.shut) }
