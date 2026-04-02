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
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
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
	codec   encoding.CodecV2

	unary_int  grpc.UnaryClientInterceptor
	stream_int grpc.StreamClientInterceptor
}

func NewConn(tx FrameHandler, opts ...ConnOption) *Conn {
	opt := connOption{}
	for _, o := range opts {
		o.apply(&opt)
	}

	v := &Conn{
		tx: tx,
		ss: map[uint32]*clientStream{},

		timeout: 5 * time.Second,
	}
	if opt.unary_int != nil {
		opt.unary_ints = append([]grpc.UnaryClientInterceptor{opt.unary_int}, opt.unary_ints...)
	}
	if opt.unary_ints != nil {
		v.unary_int = chainUnaryClientInterceptors(opt.unary_ints)
	} else {
		v.unary_int = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
	if opt.stream_int != nil {
		opt.stream_ints = append([]grpc.StreamClientInterceptor{opt.stream_int}, opt.stream_ints...)
	}
	if opt.stream_ints != nil {
		v.stream_int = chainStreamClientInterceptors(opt.stream_ints)
	} else {
		v.stream_int = func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(ctx, desc, cc, method, opts...)
		}
	}

	return v
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

	if f.HasCode() {
		// Stream should NOT be closed until the rx queue is empty.
		s.tx_closed.Store(true)
		s.trailer = f.GetTrailer().MD()
	} else if s.tx_closed.Load() {
		// Stream is being closed but there are still frames in the rx queue.
		// Drop the extra frames.
		return io.EOF
	}

	if f.HasHeader() {
		s.header = f.GetHeader().MD()
	}

	return s.put(ctx, f)
}

func (c *Conn) Invoke(ctx context.Context, method string, in, out any, opts ...grpc.CallOption) error {
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

	return c.unary_int(ctx, method, in, out, nil, func(ctx context.Context, method string, in, out any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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

		for _, opt := range opts {
			switch opt := opt.(type) {
			case grpc.HeaderCallOption:
				*opt.HeaderAddr = stream.last.GetHeader().MD()
			case grpc.TrailerCallOption:
				*opt.TrailerAddr = stream.last.GetTrailer().MD()
			}
		}

		return nil
	}, opts...)
}

func (c *Conn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	stream := c.newStream(ctx, method)
	if c.stream_int == nil {
		return stream, nil
	}

	return c.stream_int(ctx, desc, nil, method, func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return stream, nil
	}, opts...)
}

func (c *Conn) newStream(ctx context.Context, method string) *clientStream {
	md, ok := metadata.FromOutgoingContext(ctx)

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
	if ok {
		stream.header = md
	}

	return stream
}

type connOption struct {
	unary_int   grpc.UnaryClientInterceptor
	unary_ints  []grpc.UnaryClientInterceptor
	stream_int  grpc.StreamClientInterceptor
	stream_ints []grpc.StreamClientInterceptor
}

type ConnOption interface {
	apply(*connOption)
}

type connOptionFunc func(*connOption)

func (f connOptionFunc) apply(o *connOption) {
	f(o)
}

func WithUnaryInterceptor(i grpc.UnaryClientInterceptor) ConnOption {
	return connOptionFunc(func(o *connOption) {
		if o.unary_int != nil {
			panic("The unary client interceptor was already set and may not be reset.")
		}
		o.unary_int = i
	})
}

func WithChainUnaryInterceptor(is ...grpc.UnaryClientInterceptor) ConnOption {
	return connOptionFunc(func(o *connOption) {
		o.unary_ints = append(o.unary_ints, is...)
	})
}

func chainUnaryClientInterceptors(is []grpc.UnaryClientInterceptor) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return is[0](ctx, method, req, reply, cc, getChainUnaryInvoker(is, 0, invoker), opts...)
	}
}

func getChainUnaryInvoker(is []grpc.UnaryClientInterceptor, curr int, last grpc.UnaryInvoker) grpc.UnaryInvoker {
	if curr == len(is)-1 {
		return last
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		return is[curr+1](ctx, method, req, reply, cc, getChainUnaryInvoker(is, curr+1, last), opts...)
	}
}

func WithStreamInterceptor(i grpc.StreamClientInterceptor) ConnOption {
	return connOptionFunc(func(o *connOption) {
		o.stream_int = i
	})
}

func WithChainStreamInterceptor(is ...grpc.StreamClientInterceptor) ConnOption {
	return connOptionFunc(func(o *connOption) {
		o.stream_ints = append(o.stream_ints, is...)
	})
}

func chainStreamClientInterceptors(is []grpc.StreamClientInterceptor) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return is[0](ctx, desc, cc, method, getChainStreamer(is, 0, streamer), opts...)
	}
}

func getChainStreamer(is []grpc.StreamClientInterceptor, curr int, last grpc.Streamer) grpc.Streamer {
	if curr == len(is)-1 {
		return last
	}
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return is[curr+1](ctx, desc, cc, method, getChainStreamer(is, curr+1, last), opts...)
	}
}
