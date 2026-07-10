package drpc

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ grpc.ClientConnInterface = &Conn{}

// learnedIndex is a method index learned from a server, valid only for the
// epoch it was learned under (PROTOCOL.md §13).
type learnedIndex struct {
	epoch uint32
	index uint32
}

type Conn struct {
	// epoch is this Conn incarnation's nonce (PROTOCOL.md §6.1).
	epoch uint32
	tx    FrameHandler

	mu        sync.Mutex
	ss        map[uint32]*clientStream
	sidNext   uint32
	exhausted bool

	// serverEpoch tracks the peer incarnation; learned method indices are
	// keyed by it (PROTOCOL.md §6.1, §13).
	serverEpoch atomic.Uint32
	methods     sync.Map // string -> learnedIndex

	call_opts  []grpc.CallOption
	unary_int  grpc.UnaryClientInterceptor
	stream_int grpc.StreamClientInterceptor
}

func NewConn(tx FrameHandler, opts ...ConnOption) *Conn {
	opt := connOption{}
	for _, o := range opts {
		o.apply(&opt)
	}

	v := &Conn{
		epoch: rand.Uint32(),
		tx:    tx,
		ss:    map[uint32]*clientStream{},

		call_opts: []grpc.CallOption{},
	}
	if opt.call_opts != nil {
		v.call_opts = append(v.call_opts, opt.call_opts...)
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

// Handle delivers one server frame to this Conn. Adapters call it for each
// frame of a received envelop, in order (PROTOCOL.md §9.1).
func (c *Conn) Handle(ctx context.Context, f *Frame) error {
	if f.isReset() {
		// Act only if the echoed epoch is ours (PROTOCOL.md §9.3).
		if f.GetEpoch() != c.epoch {
			return nil
		}
		if s := c.lookup(f.GetSid()); s != nil {
			s.finishReset()
		}
		return nil
	}
	if f.isPing() {
		// Liveness and stream probes arrive with the timeout system (M3).
		return nil
	}

	s := c.lookup(f.GetSid())
	if s == nil {
		// Unknown sid: tell a desynced server to stop, immediately —
		// no OPEN can ever arrive at a client (PROTOCOL.md §9.3).
		return c.tx.Handle(ctx, resetFor(f))
	}
	s.handleRx(f)
	return nil
}

// Close fails every live call with UNAVAILABLE. Adapters call it when the
// transport dies (PROTOCOL.md §4.5). Idempotent.
func (c *Conn) Close(err error) {
	c.mu.Lock()
	ss := make([]*clientStream, 0, len(c.ss))
	for _, s := range c.ss {
		ss = append(ss, s)
	}
	c.mu.Unlock()

	st := status.Error(codes.Unavailable, "transport closed")
	if err != nil {
		st = status.Errorf(codes.Unavailable, "transport closed: %v", err)
	}
	for _, s := range ss {
		s.finishLocal(st)
	}
}

func (c *Conn) lookup(sid uint32) *clientStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ss[sid]
}

func (c *Conn) remove(sid uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.ss, sid)
}

// noteServerFrame runs for every frame accepted by a live stream: it tracks
// the server epoch (flushing learned indices on change) and learns method
// indices (PROTOCOL.md §6.1, §13).
func (c *Conn) noteServerFrame(f *Frame, method string) {
	e := f.GetEpoch()
	if c.serverEpoch.Load() != e {
		c.serverEpoch.Store(e)
		c.methods.Range(func(k, _ any) bool {
			c.methods.Delete(k)
			return true
		})
	}
	if idx := f.GetMethodIndex(); idx > 0 {
		c.methods.Store(method, learnedIndex{epoch: e, index: idx})
	}
}

func (c *Conn) Invoke(ctx context.Context, method string, in, out any, opts ...grpc.CallOption) error {
	opts = append(c.call_opts, opts...)
	return c.unary_int(ctx, method, in, out, nil, func(ctx context.Context, method string, in, out any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		s, err := c.newStream(ctx, method, false, false)
		if err != nil {
			return err
		}
		defer s.abandon()

		for _, opt := range opts {
			if opt, ok := opt.(grpc.ForceCodecV2CallOption); ok {
				s.codec = opt.CodecV2
				s.codecName = opt.CodecV2.Name()
			}
		}

		if err := s.send(in); err != nil {
			return err
		}
		if err := s.RecvMsg(out); err != nil {
			return err
		}

		for _, opt := range opts {
			switch opt := opt.(type) {
			case grpc.HeaderCallOption:
				*opt.HeaderAddr, _ = s.Header()
			case grpc.TrailerCallOption:
				*opt.TrailerAddr = s.Trailer()
			}
		}
		return nil
	}, opts...)
}

func (c *Conn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	opts = append(c.call_opts, opts...)

	// The stream is created by the innermost streamer so the OPEN frame sees
	// the interceptor-final ctx and merged call options (PROTOCOL.md §8).
	streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		s, err := c.newStream(ctx, method, desc.ClientStreams, desc.ServerStreams)
		if err != nil {
			return nil, err
		}
		for _, opt := range opts {
			if opt, ok := opt.(grpc.ForceCodecV2CallOption); ok {
				s.codec = opt.CodecV2
				s.codecName = opt.CodecV2.Name()
			}
		}

		if desc.ClientStreams {
			// Eager OPEN: the server must be able to start the handler even
			// if the client never sends (PROTOCOL.md §8).
			if err := s.sendOpen(); err != nil {
				s.finishLocal(toStatusErr(err))
				return nil, err
			}
		}
		return s, nil
	}

	return c.stream_int(ctx, desc, nil, method, streamer, opts...)
}

func (c *Conn) newStream(ctx context.Context, method string, clientStreams, serverStreams bool) (*clientStream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.exhausted {
		return nil, status.Error(codes.ResourceExhausted, "sid space exhausted; create a new Conn")
	}
	c.sidNext++
	if c.sidNext == 0 {
		// The sid space is never recycled within an epoch (PROTOCOL.md §6.2).
		c.exhausted = true
		return nil, status.Error(codes.ResourceExhausted, "sid space exhausted; create a new Conn")
	}

	s := newClientStream(ctx, c, c.sidNext, method, clientStreams, serverStreams)
	c.ss[s.sid] = s
	return s, nil
}

type connOption struct {
	call_opts   []grpc.CallOption
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

func WithDefaultCallOptions(opts ...grpc.CallOption) ConnOption {
	return connOptionFunc(func(o *connOption) {
		o.call_opts = append(o.call_opts, opts...)
	})
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
