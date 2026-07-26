// Command udp-sensor is the library's reason for existing, in one file tree:
// a server-streaming sensor feed over UDP, subscribed to with an explicit
// deadline, degrading into an ordered *subsequence* when datagrams are lost —
// and a report at the end of exactly what was lost and where.
//
// By default the server and the client run in this one process, talking over
// a loopback UDP socket:
//
//	go run ./...
//
// Two processes work as well (see README.md):
//
//	go run ./... -serve 127.0.0.1:9000
//	go run ./... -connect 127.0.0.1:9000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"
)

var (
	serve   = flag.String("serve", "", "run only the server on this UDP address and block until Ctrl-C")
	connect = flag.String("connect", "", "run only the client, against this UDP address")

	hz      = flag.Uint("hz", 200, "sample rate the sensor produces at")
	loss    = flag.Float64("loss", 0.05, "fraction of outbound data frames the server drops (loopback UDP loses nothing on its own)")
	window  = flag.Duration("for", 2*time.Second, "how long the client subscribes: its call deadline")
	consume = flag.Duration("consume", 8*time.Millisecond, "time the client spends handling each reading; slower than 1/hz makes the rx buffer drop")
	rxSize  = flag.Int("rx-buffer", 4, "client rx buffer, in frames; small enough to see DropOldest work")
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "udp-sensor:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	switch {
	case *serve != "" && *connect != "":
		return fmt.Errorf("-serve and -connect are mutually exclusive")

	case *connect != "":
		return runClient(ctx, *connect)

	case *serve != "":
		srv, err := startServer(ctx, *serve)
		if err != nil {
			return err
		}
		defer srv.stop()
		fmt.Printf("serving /sensor.SensorService/Readings on %s (Ctrl-C to stop)\n", srv.addr)
		<-ctx.Done()
		return nil

	default:
		// Both halves in one process: the client dials the server's own
		// loopback socket.
		srv, err := startServer(ctx, "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer srv.stop()
		fmt.Printf("server listening on %s\n", srv.addr)
		if err := runClient(ctx, srv.addr); err != nil {
			return err
		}
		// GracefulStop waits for the handler, so the count it reports is final.
		srv.stop()
		fmt.Printf("\nserver sent %d readings before the deadline cut the feed\n", srv.sensor.sent.Load())
		return nil
	}
}
