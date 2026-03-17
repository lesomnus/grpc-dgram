package drpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var _ grpc.ClientConnInterface = &Conn{}

type Conn struct {
	mu sync.Mutex
	tx FrameHandler
	ss map[uint32]*stream

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
		ss: map[uint32]*stream{},

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
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.rx <- f:
		return nil
	}
}

func (c *Conn) Invoke(ctx context.Context, method string, in, out any, opts ...grpc.CallOption) error {
	// TODO: use codec
	payload, err := proto.Marshal(in.(proto.Message))
	if err != nil {
		return err
	}

	req := &Frame{}
	req.SetPayload(payload)

	// Method.
	if i, ok := c.methods.Load(method); ok {
		req.SetMethodIndex(i.(uint32))
	} else {
		req.SetMethod(method)
	}

	// Metadata
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		req.SetMetadata(newMd(md))
	}

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

	stream := c.newStream()
	if err := stream.send(req); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()

	case res, ok := <-stream.rx:
		if !ok {
			return fmt.Errorf("unexpected close of stream")
		}
		if err := res.Err(); err != nil {
			return err
		}

		// TODO: use codec
		return proto.Unmarshal(res.GetPayload(), out.(proto.Message))
	}
}

func (c *Conn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *Conn) newStream() *stream {
	sid := c.sid.Add(1)
	stream := newStream(sid, c.tx)

	c.mu.Lock()
	defer c.mu.Unlock()

	for {
		if _, ok := c.ss[sid]; !ok {
			c.ss[sid] = stream
			break
		}

		sid = c.sid.Add(1)
	}

	return stream
}
