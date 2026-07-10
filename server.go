package drpc

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ grpc.ServiceRegistrar = &Server{}

// callKey identifies a live call: streams are keyed by
// (peer, client-epoch, sid) (PROTOCOL.md §6.2).
type callKey struct {
	peer  any
	epoch uint32
	sid   uint32
}

// epochKey identifies one client incarnation seen from one peer.
type epochKey struct {
	peer  any
	epoch uint32
}

type Server struct {
	// epoch is this Server incarnation's nonce (PROTOCOL.md §6.1).
	epoch uint32
	tx    FrameHandler

	root       context.Context
	rootCancel context.CancelCauseFunc

	mu     sync.Mutex
	calls  map[callKey]*serverStream
	hwm    map[epochKey]uint32 // high-water mark per client incarnation (§9.4)
	drain  bool
	closed bool
	wg     sync.WaitGroup

	// serving flips on the first Handle; the registry is immutable after
	// that (PROTOCOL.md §13).
	serving  atomic.Bool
	methods  []*serviceDesc // 1-based: methods[i-1] has index i
	services map[string]*serviceDesc

	unary_int  grpc.UnaryServerInterceptor
	stream_int grpc.StreamServerInterceptor
}

func NewServer(tx FrameHandler, opts ...ServerOption) *Server {
	opt := serverOption{}
	for _, o := range opts {
		o.apply(&opt)
	}

	v := &Server{
		epoch: rand.Uint32(),
		tx:    tx,

		calls: map[callKey]*serverStream{},
		hwm:   map[epochKey]uint32{},

		methods:  []*serviceDesc{},
		services: map[string]*serviceDesc{},
	}
	v.root, v.rootCancel = context.WithCancelCause(context.Background())

	if opt.unary_int != nil {
		opt.unary_ints = append([]grpc.UnaryServerInterceptor{opt.unary_int}, opt.unary_ints...)
	}
	if opt.unary_ints != nil {
		v.unary_int = chainUnaryServerInterceptors(opt.unary_ints)
	}
	if opt.stream_int != nil {
		opt.stream_ints = append([]grpc.StreamServerInterceptor{opt.stream_int}, opt.stream_ints...)
	}
	if opt.stream_ints != nil {
		v.stream_int = chainStreamServerInterceptors(opt.stream_ints)
	}

	return v
}

func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if s.serving.Load() {
		panic("drpc: RegisterService called after the server started serving")
	}

	register := func(fullname string) *serviceDesc {
		d, ok := s.services[fullname]
		if !ok {
			s.methods = append(s.methods, nil)
			d = &serviceDesc{
				index:    uint32(len(s.methods)), // 1-based (PROTOCOL.md §13)
				fullname: fullname,
			}
			s.methods[d.index-1] = d
			s.services[fullname] = d
		}
		return d
	}

	for i, method := range desc.Methods {
		d := register(fmt.Sprintf("/%s/%s", desc.ServiceName, method.MethodName))
		d.service = desc
		d.method = &desc.Methods[i]
		d.impl = impl
	}
	for i, stream := range desc.Streams {
		d := register(fmt.Sprintf("/%s/%s", desc.ServiceName, stream.StreamName))
		d.service = desc
		d.stream = &desc.Streams[i]
		d.impl = impl
	}
}

// Handle delivers one client frame to this Server. Adapters call it for each
// frame of a received envelop, in order, with the peer attached to ctx
// (PROTOCOL.md §9.1).
func (s *Server) Handle(ctx context.Context, f *Frame) error {
	s.serving.Store(true)

	if f.isReset() {
		// Act only if the echoed epoch is ours (PROTOCOL.md §9.3).
		if f.GetEpoch() != s.epoch {
			return nil
		}
		peer, _ := PeerFromContext(ctx)
		s.resetByPeerSid(peer, f.GetSid())
		return nil
	}
	if f.isPing() {
		// Liveness and stream probes arrive with the timeout system (M3).
		return nil
	}

	peer, _ := PeerFromContext(ctx)
	key := callKey{peer: peer, epoch: f.GetEpoch(), sid: f.GetSid()}

	s.mu.Lock()
	st := s.calls[key]
	s.mu.Unlock()
	if st != nil {
		st.handleRx(f)
		return nil
	}

	if f.isOpen() && f.GetSeq() == 1 {
		return s.open(ctx, key, f)
	}

	// Unknown, non-OPEN: RESET so a desynced client fails fast. Delayed by
	// T_hold in unreliable mode; immediate here until the M3 timer machinery
	// lands (the reliable-mode reading, PROTOCOL.md §9.3, §10.6).
	return s.tx.Handle(ctx, resetFor(f))
}

// resetByPeerSid cancels every live call from peer with the given sid,
// regardless of client epoch (a RESET echoes the server epoch, which does not
// name the client incarnation).
func (s *Server) resetByPeerSid(peer any, sid uint32) {
	s.mu.Lock()
	var targets []*serverStream
	for k, st := range s.calls {
		if k.peer == peer && k.sid == sid {
			targets = append(targets, st)
		}
	}
	s.mu.Unlock()

	cause := status.Error(codes.Unavailable, "call reset by peer")
	for _, st := range targets {
		st.cancel(cause)
	}
}

func (s *Server) open(ctx context.Context, key callKey, f *Frame) error {
	var desc *serviceDesc
	if i := f.GetMethodIndex(); i > 0 {
		if int(i) <= len(s.methods) {
			desc = s.methods[i-1]
		}
	} else if m := f.GetMethod(); m != "" {
		desc = s.services[m]
	}
	if desc == nil {
		return s.rejectOpen(ctx, f, codes.Unimplemented, "method not found")
	}
	codec := f.getCodec()
	if codec == nil {
		return s.rejectOpen(ctx, f, codes.Unimplemented, "unsupported codec: %s", f.GetCodec())
	}

	// The handler ctx derives from the Server root — never from the
	// per-datagram rx ctx — with the peer re-attached (PROTOCOL.md §6.4).
	sctx := s.root
	if key.peer != nil {
		sctx = NewPeerContext(sctx, key.peer)
	}
	sctx = newIncomingContext(sctx, f)

	st := newServerStream(sctx, s, key, desc, codec)

	var transport *serverTransportUnary
	if desc.IsUnary() {
		transport = &serverTransportUnary{method: desc.fullname}
		st.ctx = grpc.NewContextWithServerTransportStream(st.ctx, transport)
	} else {
		st.ctx = grpc.NewContextWithServerTransportStream(st.ctx, serverTransportStream{st})
	}

	s.mu.Lock()
	if s.drain || s.closed {
		s.mu.Unlock()
		// Draining/stopped servers refuse new calls (PROTOCOL.md §9.4).
		return s.tx.Handle(ctx, resetFor(f))
	}
	if _, dup := s.calls[key]; dup {
		// Lost the race against a concurrent duplicate OPEN.
		s.mu.Unlock()
		return nil
	}
	s.calls[key] = st
	ek := epochKey{peer: key.peer, epoch: key.epoch}
	if s.hwm[ek] < key.sid {
		s.hwm[ek] = key.sid
	}
	s.wg.Add(1)
	s.mu.Unlock()

	if desc.IsUnary() {
		go s.runUnary(st, transport, f)
	} else {
		// A server-streaming OPEN piggybacks the request message and the
		// half-close (PROTOCOL.md §8); generated handlers read the request
		// via RecvMsg.
		if f.HasPayload() {
			st.rx <- f
		}
		if f.isClose() {
			st.eofOnce.Do(func() { close(st.rxEOF) })
		}
		if desc.stream.ClientStreams {
			// Creation ack (PROTOCOL.md §8). Server-streaming emits its
			// first data frame promptly instead.
			st.sendH()
		}
		go s.runStream(st)
	}
	return nil
}

// rejectOpen answers an OPEN that cannot start a call with a terminal frame
// (PROTOCOL.md §9.4).
func (s *Server) rejectOpen(ctx context.Context, f *Frame, code codes.Code, msg string, args ...any) error {
	t := &Frame{}
	t.SetEpoch(s.epoch)
	t.SetSid(f.GetSid())
	t.SetSeq(1)
	t.SetFlags(FlagClose)
	t.SetCode(uint32(code))
	t.SetDesc(fmt.Sprintf(msg, args...))
	return s.tx.Handle(ctx, t)
}

func (s *Server) runUnary(st *serverStream, transport *serverTransportUnary, open *Frame) {
	defer s.wg.Done()
	defer s.finish(st)

	dec := func(v any) error {
		return open.unmarshal(v, st.codec)
	}

	resp, err := st.desc.method.Handler(st.desc.impl, st.ctx, dec, s.unary_int)
	if err == nil && st.ctx.Err() != nil {
		err = context.Cause(st.ctx)
	}

	st.txMu.Lock()
	f := st.nextFrameLocked()
	st.txMu.Unlock()
	f.SetFlags(FlagClose)
	if transport.header != nil {
		f.SetHeader(newMd(transport.header))
	}
	if transport.trailer != nil {
		f.SetTrailer(newMd(transport.trailer))
	}
	if err != nil {
		f.setError(toStatusErr(err))
	} else {
		buf, merr := st.codec.Marshal(resp)
		if merr != nil {
			f.setError(status.Errorf(codes.Internal, "marshal response: %v", merr))
		} else {
			f.SetPayload(buf.Materialize())
			buf.Free()
			f.SetCode(uint32(codes.OK))
		}
	}

	// The terminal is sent even when the handler ctx ended: the client (or
	// its tombstone, once M3 lands) decides what to do with it.
	s.tx.Handle(context.WithoutCancel(st.ctx), f)
}

func (s *Server) runStream(st *serverStream) {
	defer s.wg.Done()
	defer s.finish(st)

	var err error
	if s.stream_int != nil {
		info := grpc.StreamServerInfo{
			FullMethod:     st.desc.fullname,
			IsClientStream: st.desc.stream.ClientStreams,
			IsServerStream: st.desc.stream.ServerStreams,
		}
		err = s.stream_int(st.desc.impl, st, &info, st.desc.stream.Handler)
	} else {
		err = st.desc.stream.Handler(st.desc.impl, st)
	}
	if err == nil && st.ctx.Err() != nil {
		err = context.Cause(st.ctx)
	}

	f := st.terminalFrame(err)
	s.tx.Handle(context.WithoutCancel(st.ctx), f)
}

func (s *Server) finish(st *serverStream) {
	s.mu.Lock()
	delete(s.calls, st.key)
	s.mu.Unlock()
	st.cancel(status.Error(codes.Canceled, "call finished"))
}

// GracefulStop refuses new calls and waits for in-flight handlers.
func (s *Server) GracefulStop() {
	s.mu.Lock()
	s.drain = true
	s.mu.Unlock()

	s.wg.Wait()

	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Stop cancels every in-flight handler and refuses new calls
// (PROTOCOL.md §9.4). Idempotent.
func (s *Server) Stop() {
	s.mu.Lock()
	s.drain = true
	s.closed = true
	targets := make([]*serverStream, 0, len(s.calls))
	for _, st := range s.calls {
		targets = append(targets, st)
	}
	s.mu.Unlock()

	cause := status.Error(codes.Unavailable, "server stopped")
	for _, st := range targets {
		st.cancel(cause)
	}
	s.wg.Wait()
}

// DisconnectPeer fails every live call from peer. Adapters call it when a
// peer's transport dies (PROTOCOL.md §4.5). Idempotent.
func (s *Server) DisconnectPeer(peer any, err error) {
	s.mu.Lock()
	var targets []*serverStream
	for k, st := range s.calls {
		if k.peer == peer {
			targets = append(targets, st)
		}
	}
	s.mu.Unlock()

	cause := status.Error(codes.Unavailable, "transport closed")
	if err != nil {
		cause = status.Errorf(codes.Unavailable, "transport closed: %v", err)
	}
	for _, st := range targets {
		st.cancel(cause)
	}
}

type ServerOption interface {
	apply(*serverOption)
}

type serverOption struct {
	unary_int   grpc.UnaryServerInterceptor
	unary_ints  []grpc.UnaryServerInterceptor
	stream_int  grpc.StreamServerInterceptor
	stream_ints []grpc.StreamServerInterceptor
}

type serverOptionFunc func(*serverOption)

func (f serverOptionFunc) apply(o *serverOption) {
	f(o)
}

func UnaryInterceptor(i grpc.UnaryServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		if o.unary_int != nil {
			panic("The unary server interceptor was already set and may not be reset.")
		}
		o.unary_int = i
	})
}

func ChainUnaryInterceptors(is ...grpc.UnaryServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		o.unary_ints = append(o.unary_ints, is...)
	})
}

func chainUnaryServerInterceptors(is []grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return is[0](ctx, req, info, getChainUnaryHandler(is, 0, info, handler))
	}
}

func getChainUnaryHandler(is []grpc.UnaryServerInterceptor, curr int, info *grpc.UnaryServerInfo, last grpc.UnaryHandler) grpc.UnaryHandler {
	if curr == len(is)-1 {
		return last
	}
	return func(ctx context.Context, req any) (any, error) {
		return is[curr+1](ctx, req, info, getChainUnaryHandler(is, curr+1, info, last))
	}
}

func StreamInterceptor(i grpc.StreamServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		if o.stream_int != nil {
			panic("The stream server interceptor was already set and may not be reset.")
		}
		o.stream_int = i
	})
}

func ChainStreamInterceptors(is ...grpc.StreamServerInterceptor) ServerOption {
	return serverOptionFunc(func(o *serverOption) {
		o.stream_ints = append(o.stream_ints, is...)
	})
}

func chainStreamServerInterceptors(is []grpc.StreamServerInterceptor) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return is[0](srv, ss, info, getChainStreamHandler(is, 0, info, handler))
	}
}

func getChainStreamHandler(is []grpc.StreamServerInterceptor, curr int, info *grpc.StreamServerInfo, last grpc.StreamHandler) grpc.StreamHandler {
	if curr == len(is)-1 {
		return last
	}
	return func(srv any, stream grpc.ServerStream) error {
		return is[curr+1](srv, stream, info, getChainStreamHandler(is, curr+1, info, last))
	}
}
