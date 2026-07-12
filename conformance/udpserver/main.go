// Command udpserver is a cross-language conformance fixture: it serves the
// echo service over the drpc UDP transport (unreliable mode) so a TypeScript
// client can drive a real Go drpc.Server across the wire. It binds an
// ephemeral 127.0.0.1 UDP port, prints "PORT <n>" on stdout, and runs until
// stdin closes (the parent test process going away) — see
// ts/test/conformance.test.ts.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/udp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "udpserver:", err)
		os.Exit(1)
	}
}

func run() error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return err
	}

	gw := udp.NewGateway(conn)
	srv := drpc.NewServer(gw)
	// Registration must precede the first received frame (§13); done here,
	// before Serve starts.
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = gw.Serve(ctx, srv) }()

	port := conn.LocalAddr().(*net.UDPAddr).Port
	fmt.Printf("PORT %d\n", port)
	if f, ok := any(os.Stdout).(interface{ Sync() error }); ok {
		_ = f.Sync()
	}

	// Block until the parent closes our stdin, then tear down cleanly.
	_, _ = io.Copy(io.Discard, os.Stdin)
	cancel()
	_ = conn.Close()
	srv.Stop()
	return nil
}
