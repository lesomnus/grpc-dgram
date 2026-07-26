package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gorilla/websocket"
	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/examples/websocket-echo/echopb"
	"github.com/lesomnus/grpc-dgram/transport/gorilla"
)

type client struct {
	conn *drpc.Conn
	svc  echopb.EchoServiceClient
}

func dial(ctx context.Context, url string) (*client, error) {
	c, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	// A drpc.Conn is a grpc.ClientConnInterface, and the transport attaches
	// itself: the read loop and the WebSocket keepalive start on their own,
	// and reliable mode is discovered from the transport (PROTOCOL.md §4.3).
	conn := drpc.NewConn(gorilla.New(c))
	return &client{conn: conn, svc: echopb.NewEchoServiceClient(conn)}, nil
}

// close tears down the conn, the transport, and the WebSocket in one call.
func (c *client) close() { c.conn.Close(nil) }

func (c *client) echo(ctx context.Context, msg string) error {
	res, err := c.svc.Echo(ctx, &echopb.EchoRequest{Message: msg})
	if err != nil {
		return err
	}
	fmt.Printf("Echo(%q) -> %q\n", msg, res.GetMessage())
	return nil
}

// count runs the server-streaming call and checks the one property reliable
// mode adds: the client receives *every* response, in order. A gap or a
// duplicate here would not be a silent subsequence — the core fails the call
// with INTERNAL, because a "reliable" transport that lost a frame is broken
// (PROTOCOL.md §10.6).
func (c *client) count(ctx context.Context, n uint32, interval time.Duration) error {
	fmt.Printf("\nCount(count=%d, interval=%s):\n", n, interval)
	stream, err := c.svc.Count(ctx, &echopb.CountRequest{
		Count:      n,
		IntervalMs: uint32(interval.Milliseconds()),
	})
	if err != nil {
		return err
	}

	want := uint32(1)
	for {
		res, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break // the terminal frame arrived: a clean end of stream
		}
		if err != nil {
			return fmt.Errorf("Count: %w", err)
		}
		if res.GetSequence() != want {
			return fmt.Errorf("reliable mode violated: got sequence %d, want %d", res.GetSequence(), want)
		}
		if want == 1 || want == n {
			fmt.Printf("  %s\n", res.GetMessage())
		} else if want == 2 {
			fmt.Println("  ...")
		}
		want++
	}
	fmt.Printf("  %d responses, sequence 1..%d exactly — no gaps, no reordering\n", want-1, want-1)
	if want-1 != n {
		return fmt.Errorf("stream ended after %d of %d responses", want-1, n)
	}
	return nil
}
