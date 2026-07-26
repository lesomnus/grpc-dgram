// Command websocket-echo runs grpc-dgram over a WebSocket, where the channel
// is reliable and ordered: the core auto-detects reliable mode, turns every
// protocol timer and retransmission off, and delivers the exact sequence a
// handler sent — plain gRPC semantics, no datagram caveats. The other half of
// the demo is shutdown: a graceful stop drains an in-flight stream and refuses
// what comes after.
//
// By default the server and the client run in this one process:
//
//	go run ./...
//
// Two processes work as well (see README.md):
//
//	go run ./... -serve 127.0.0.1:9010
//	go run ./... -connect ws://127.0.0.1:9010/rpc
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
	serve   = flag.String("serve", "", "run only the server on this address and block until Ctrl-C")
	connect = flag.String("connect", "", "run only the client, against this ws:// URL")

	count    = flag.Uint("count", 20, "responses the Count stream produces")
	interval = flag.Duration("interval", 50*time.Millisecond, "delay between Count responses")
	after    = flag.Duration("shutdown-after", 400*time.Millisecond, "when to start the graceful shutdown, measured from the start of the Count stream")
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "websocket-echo:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	switch {
	case *serve != "" && *connect != "":
		return fmt.Errorf("-serve and -connect are mutually exclusive")

	case *connect != "":
		cl, err := dial(ctx, *connect)
		if err != nil {
			return err
		}
		defer cl.close()
		if err := cl.echo(ctx, "hello"); err != nil {
			return err
		}
		return cl.count(ctx, uint32(*count), *interval)

	case *serve != "":
		srv, err := startServer(ctx, *serve)
		if err != nil {
			return err
		}
		fmt.Printf("serving wsecho.EchoService on %s (Ctrl-C to stop)\n", srv.url)
		<-ctx.Done()
		fmt.Println("\ndraining...")
		srv.gracefulStop()
		return nil

	default:
		srv, err := startServer(ctx, "127.0.0.1:0")
		if err != nil {
			return err
		}
		fmt.Printf("server listening on %s\n", srv.url)

		cl, err := dial(ctx, srv.url)
		if err != nil {
			return err
		}
		defer cl.close()

		if err := cl.echo(ctx, "hello"); err != nil {
			return err
		}

		// Shut the server down while the stream below is still running.
		// GracefulStop refuses new calls and waits for the live handler, so
		// the stream must still deliver every response, in order.
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			time.Sleep(*after)
			fmt.Printf("\n[%s in] GracefulStop: refusing new calls, waiting for the live handler\n", *after)
			srv.gracefulStop()
			fmt.Println("[server] GracefulStop returned: every handler finished")
		}()

		if err := cl.count(ctx, uint32(*count), *interval); err != nil {
			return err
		}
		<-stopped

		// The server is drained: a new call is refused, and the client learns
		// so at once instead of waiting for a timer that reliable mode does
		// not run.
		fmt.Println("\ncalling Echo again, on a server that has already stopped:")
		if err := cl.echo(ctx, "too late"); err != nil {
			fmt.Printf("  refused, as expected: %v\n", err)
			return nil
		}
		return fmt.Errorf("Echo succeeded after GracefulStop")
	}
}
