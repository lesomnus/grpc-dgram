package drpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
)

var _ grpc.ClientConnInterface = &Conn{}

type Conn struct {
	mu sync.Mutex
	tx FrameHandler
	ss map[uint32]*clientStream

	sid atomic.Uint32

	// methods is a mapping from method name to index.
	// Key must be a full method name (e.g. "/hday.HolderService/Add").
	methods sync.Map // map[string]uint32
	// timeout specifies a time limit for requests.
	// No deadline will be set if timeout is zero.
	timeout time.Duration
}

func NewConn(tx FrameHandler) *Conn {
	return &Conn{
		tx: tx,
		ss: map[uint32]*clientStream{},

		timeout: 5 * time.Second,
	}
}

func (c *Conn) Handle(ctx context.Context, f *Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	sid := f.GetSid()
	s, ok := c.ss[sid]
	if !ok {
		// Corresponding stream not found.
		// Maybe the f is delayed or for the previous Conn?
		return io.EOF
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.rx <- f:
		return nil
	}
}

func (c *Conn) Invoke(ctx context.Context, method string, in, out any, opts ...grpc.CallOption) error {
	// // Metadata
	// if md, ok := metadata.FromOutgoingContext(ctx); ok {
	// 	req.SetMetadata(newMd(md))
	// }

	// // Deadline.
	// if deadline, ok := ctx.Deadline(); ok {
	// 	req.SetDeadline(timestamppb.New(deadline))
	// } else if c.timeout > 0 {
	// 	deadline := time.Now().Add(c.timeout)
	// 	req.SetDeadline(timestamppb.New(deadline))

	// 	ctx_, cancel := context.WithDeadline(ctx, deadline)
	// 	defer cancel()
	// 	ctx = ctx_
	// }

	stream := c.newStream(ctx, method)
	defer stream.Close()

	if err := stream.SendMsg(in); err != nil {
		return err
	}
	if err := stream.RecvMsg(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("unexpected close of stream")
		}
		return err
	}

	return nil
}

func (c *Conn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	stream := c.newStream(ctx, method)
	return stream, nil
}

func (c *Conn) newStream(ctx context.Context, method string) *clientStream {
	c.mu.Lock()
	defer c.mu.Unlock()

	var stream *clientStream
	for {
		sid := c.sid.Add(1)
		if _, ok := c.ss[sid]; !ok {
			stream = newClientStream(ctx, c, sid, method)
			c.ss[sid] = stream
			break
		}

		sid = c.sid.Add(1)
	}

	return stream
}
