package drpc

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

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
	mode  mode

	maxHandlerTimeout time.Duration
	rx                rxConfig
	methodRx          map[string]rxConfig
	limits            Limits

	root       context.Context
	rootCancel context.CancelCauseFunc

	mu            sync.Mutex
	calls         map[callKey]*serverStream
	peers         map[epochKey]*peerState // per client incarnation (§9.4)
	pendingResets map[callKey]*pendingReset
	resetAt       map[callKey]int64 // immediate-RESET rate limit (§9.3)
	drain         bool
	closed        bool
	wg            sync.WaitGroup
	sw            sweeper

	// serving flips on the first Handle; the registry is immutable after
	// that (PROTOCOL.md §13).
	serving  atomic.Bool
	services map[string]*serviceDesc

	// sawUnreliable latches once any unreliable-mode state exists; until
	// then the sweeper has nothing it could ever do (reliable peers run no
	// timers) and is never started.
	sawUnreliable atomic.Bool

	unary_int  grpc.UnaryServerInterceptor
	stream_int grpc.StreamServerInterceptor
}

func NewServer(tx FrameHandler, opts ...ServerOption) *Server {
	opt := serverOption{}
	for _, o := range opts {
		o.applyServer(&opt)
	}

	v := &Server{
		epoch: rand.Uint32(),
		tx:    tx,
		mode:  resolveMode(tx, opt.reliable, opt.timing),
		sw:    newSweeper(),

		maxHandlerTimeout: opt.maxHandlerTimeout,
		rx:                opt.rx.withDefaults(),
		methodRx:          opt.methodRx,
		limits:            opt.limits.withDefaults(),

		calls:         map[callKey]*serverStream{},
		peers:         map[epochKey]*peerState{},
		pendingResets: map[callKey]*pendingReset{},
		resetAt:       map[callKey]int64{},

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
			d = &serviceDesc{fullname: fullname}
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

// rxReliable resolves the mode governing a received frame: the adapter's
// per-channel annotation when present (NewReliableContext, PROTOCOL.md
// §4.3), else the server's own mode.
func (s *Server) rxReliable(ctx context.Context) bool {
	if r, ok := reliableFromContext(ctx); ok {
		return r
	}
	return s.mode.reliable
}

// Handle delivers one client frame to this Server. Adapters call it for each
// frame of a received envelop, in order, with the peer attached to ctx
// (PROTOCOL.md §9.1).
func (s *Server) Handle(ctx context.Context, f *Frame) error {
	s.serving.Store(true)

	if f.isReset() {
		// Act only if the echoed epoch is ours; RESET never refreshes
		// liveness (PROTOCOL.md §9.1, §9.3).
		if f.GetEpoch() != s.epoch {
			return nil
		}
		peer, _ := PeerFromContext(ctx)
		s.resetByPeerSid(peer, f.GetSid())
		return nil
	}

	peer, _ := PeerFromContext(ctx)
	key := callKey{peer: peer, epoch: f.GetEpoch(), sid: f.GetSid()}
	ek := epochKey{peer: peer, epoch: f.GetEpoch()}
	now := time.Now()

	if f.isPing() {
		// Well-formed PINGs are validated (PROTOCOL.md §9.1).
		s.mu.Lock()
		ps := s.peers[ek]
		if ps != nil {
			ps.lastRx.Store(now.UnixNano())
		}
		if f.GetSid() == 0 {
			// Peer keepalive (§10.4).
			s.mu.Unlock()
			return nil
		}
		// Stream probe (§10.5): live → no-op; tombstone with a stored T →
		// replay; key-only or unknown → immediate RESET (§9.3).
		if _, live := s.calls[key]; live {
			s.mu.Unlock()
			return nil
		}
		if ps != nil {
			if tb := ps.tombs[key.sid]; tb != nil {
				replay := ps.replayTombLocked(tb, now, s.mode.timing.Retransmit)
				stored := tb.term != nil
				s.mu.Unlock()
				if replay != nil {
					return s.tx.Handle(ctx, replay)
				}
				if stored {
					return nil // replay rate-limited; next probe retries
				}
				return s.sendReset(ctx, key, f)
			}
		}
		s.mu.Unlock()
		return s.sendReset(ctx, key, f)
	}

	s.mu.Lock()
	if st := s.calls[key]; st != nil {
		s.mu.Unlock()
		st.handleRx(f)
		return nil
	}

	// Tombstoned call: validated; replay the stored terminal, rate-limited
	// (PROTOCOL.md §9.2).
	if ps := s.peers[ek]; ps != nil {
		if tb := ps.tombs[key.sid]; tb != nil {
			ps.lastRx.Store(now.UnixNano())
			replay := ps.replayTombLocked(tb, now, s.mode.timing.Retransmit)
			s.mu.Unlock()
			if replay != nil {
				return s.tx.Handle(ctx, replay)
			}
			return nil
		}
	}

	if f.isOpen() && f.GetSeq() == 1 {
		// Aged-watermark admission (PROTOCOL.md §9.4): an unknown sid at or
		// below hwm_aged is necessarily a stale straggler.
		if ps := s.peers[ek]; ps != nil {
			// sids never wrap (§6.2): plain comparison.
			aged := ps.hwmAgedLocked(now, s.mode.timing.Tombstone, ps.reliable)
			if key.sid <= aged {
				s.mu.Unlock()
				return s.sendReset(ctx, key, f)
			}
		}
		s.mu.Unlock()
		return s.open(ctx, key, f)
	}

	// Unknown, non-OPEN, non-PING: delayed RESET — the OPEN may merely be
	// late (PROTOCOL.md §9.3). A reliable channel has no reordering:
	// immediate.
	if s.rxReliable(ctx) {
		s.mu.Unlock()
		return s.sendReset(ctx, key, f)
	}
	if _, ok := s.pendingResets[key]; !ok && len(s.pendingResets) < s.limits.MaxPendingResets {
		s.pendingResets[key] = &pendingReset{
			due:  now.Add(s.mode.timing.Hold),
			echo: f.GetEpoch(),
		}
		s.sawUnreliable.Store(true)
	}
	s.mu.Unlock()
	s.kickSweep()
	return nil
}

// sendReset answers a frame with an immediate RESET, rate-limited per call
// key on unreliable channels (PROTOCOL.md §9.3, §15).
func (s *Server) sendReset(ctx context.Context, key callKey, f *Frame) error {
	if !s.rxReliable(ctx) {
		n := nowNano()
		s.mu.Lock()
		if last, ok := s.resetAt[key]; ok {
			if n-last < int64(s.mode.timing.Retransmit) {
				s.mu.Unlock()
				return nil
			}
		} else if len(s.resetAt) >= s.limits.MaxPendingResets {
			// Bounded: drop rather than grow (anti-amplification, §15).
			s.mu.Unlock()
			return nil
		}
		s.resetAt[key] = n
		s.sawUnreliable.Store(true)
		s.mu.Unlock()
		s.kickSweep()
	}
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
		// The peer disowned the call: no terminal is sent and the tombstone
		// is key-only (PROTOCOL.md §9.3).
		st.suppressTerm.Store(true)
		st.cancel(cause)
	}
}

func (s *Server) open(ctx context.Context, key callKey, f *Frame) error {
	// The frame's channel mode governs the whole call (PROTOCOL.md §4.3):
	// captured here, inherited by the stream and the peer container.
	rel := s.rxReliable(ctx)

	// Methods are addressed by full name, always (PROTOCOL.md §13).
	desc := s.services[f.GetMethod()]
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

	// The client-asserted budget bounds the handler ctx, clamped by the
	// server cap when configured (PROTOCOL.md §10.2).
	var cancelTimeout context.CancelFunc
	if f.HasTimeout() {
		d := f.GetTimeout().AsDuration()
		if s.maxHandlerTimeout > 0 && d > s.maxHandlerTimeout {
			d = s.maxHandlerTimeout
		}
		if d > 0 {
			sctx, cancelTimeout = context.WithTimeoutCause(sctx, d,
				status.Error(codes.DeadlineExceeded, "call timeout"))
		}
	}

	rxCfg := s.rx
	if c, ok := s.methodRx[desc.fullname]; ok {
		rxCfg = c
	}
	st := newServerStream(sctx, s, key, desc, codec, rxCfg.withDefaults(), rel)
	st.cancelTimeout = cancelTimeout

	var transport *serverTransportUnary
	if desc.IsUnary() {
		transport = &serverTransportUnary{method: desc.fullname}
		st.ctx = grpc.NewContextWithServerTransportStream(st.ctx, transport)
	} else {
		st.ctx = grpc.NewContextWithServerTransportStream(st.ctx, serverTransportStream{st})
	}

	release := func() {
		st.cancel(status.Error(codes.Unavailable, "call not admitted"))
		if st.cancelTimeout != nil {
			st.cancelTimeout()
		}
	}
	s.mu.Lock()
	if s.drain || s.closed {
		s.mu.Unlock()
		release()
		// Draining/stopped servers refuse new calls (PROTOCOL.md §9.4).
		return s.sendReset(ctx, key, f)
	}
	if _, dup := s.calls[key]; dup {
		// Lost the race against a concurrent duplicate OPEN.
		s.mu.Unlock()
		release()
		return nil
	}
	now := time.Now()
	ps := s.ensurePeerLocked(epochKey{peer: key.peer, epoch: key.epoch}, now, rel)
	if _, tombed := ps.tombs[key.sid]; tombed ||
		key.sid <= ps.hwmAgedLocked(now, s.mode.timing.Tombstone, ps.reliable) {
		// Re-check under the registration lock: a concurrent duplicate OPEN
		// may have run the whole call to completion since Handle's checks —
		// admitting it here would re-execute a finished call and break
		// at-most-once (PROTOCOL.md §9.4, §14).
		s.mu.Unlock()
		release()
		return nil
	}
	if ps.liveCalls >= ps.maxLive {
		// Per-peer live-call cap (PROTOCOL.md §15): refuse rather than let a
		// single peer's valid-method OPEN flood spawn unbounded handlers.
		s.mu.Unlock()
		release()
		return s.rejectOpen(ctx, f, codes.ResourceExhausted, "too many concurrent calls")
	}
	s.calls[key] = st
	if ps.hwm < key.sid {
		ps.hwm = key.sid
	}
	ps.liveCalls++
	ps.dead = false // the peer is evidently back
	ps.lastRx.Store(now.UnixNano())
	st.ps = ps
	// The OPEN arrived after all: cancel any RESET scheduled for its sid.
	delete(s.pendingResets, key)
	s.wg.Add(1)
	s.mu.Unlock()
	s.kickSweep()

	if desc.IsUnary() {
		go s.runUnary(st, transport, f)
	} else if !desc.stream.ClientStreams {
		// A server-streaming OPEN piggybacks the request message and the
		// half-close (PROTOCOL.md §8); generated handlers read the request
		// via RecvMsg.
		if f.HasPayload() {
			st.rx <- f
		}
		if f.isClose() {
			st.eofOnce.Do(func() { close(st.rxEOF) })
		}
		// Creation ack (§8): without it, a slow producer would leave the
		// client's OPEN|CLOSE — full request payload — retransmitting.
		st.sendH()
		go s.runStream(st)
	} else {
		// CS/bidi OPENs are eager and bare: payload or CLOSE here is
		// off-shape and dropped (PROTOCOL.md §8).
		if f.HasPayload() || f.isClose() {
			st.rxDropped.Add(1)
		}
		// Creation ack (PROTOCOL.md §8).
		st.sendH()
		go s.runStream(st)
	}
	return nil
}

// rejectOpen answers an OPEN that cannot start a call with a terminal frame,
// tombstone-stored so duplicates elicit a rate-limited replay instead of a
// fresh answer each (PROTOCOL.md §9.4).
func (s *Server) rejectOpen(ctx context.Context, f *Frame, code codes.Code, msg string, args ...any) error {
	t := &Frame{}
	t.SetEpoch(s.epoch)
	t.SetSid(f.GetSid())
	t.SetSeq(1)
	t.SetFlags(FlagClose)
	t.SetCode(uint32(code))
	t.SetDesc(fmt.Sprintf(msg, args...))

	if !s.rxReliable(ctx) {
		peer, _ := PeerFromContext(ctx)
		now := time.Now()
		s.mu.Lock()
		ps := s.ensurePeerLocked(epochKey{peer: peer, epoch: f.GetEpoch()}, now, false)
		ps.lastRx.Store(now.UnixNano())
		if ps.hwm < f.GetSid() {
			ps.hwm = f.GetSid()
		}
		ps.addTombLocked(f.GetSid(), t, now.Add(s.mode.timing.Tombstone))
		s.mu.Unlock()
		s.kickSweep()
	}
	return s.tx.Handle(ctx, t)
}

func (s *Server) runUnary(st *serverStream, transport *serverTransportUnary, open *Frame) {
	defer s.wg.Done()
	var term *Frame
	defer func() { s.finish(st, term) }()

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
	header, trailer := transport.snapshot()
	if header != nil {
		f.SetHeader(newMd(header))
	}
	if trailer != nil {
		f.SetTrailer(newMd(trailer))
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
	// its tombstone) decides what to do with it — unless the peer disowned
	// the call (RESET) or vanished (liveness), where nothing listens (§9.3).
	if st.suppressTerm.Load() {
		return
	}
	term = f
	st.transmit(context.WithoutCancel(st.ctx), f)
}

func (s *Server) runStream(st *serverStream) {
	defer s.wg.Done()
	var term *Frame
	defer func() { s.finish(st, term) }()

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
	if st.suppressTerm.Load() {
		return
	}
	term = f
	st.transmit(context.WithoutCancel(st.ctx), f)
}

func (s *Server) finish(st *serverStream, term *Frame) {
	now := time.Now()
	s.mu.Lock()
	delete(s.calls, st.key)
	if ps := st.ps; ps != nil {
		ps.liveCalls--
		if !st.reliable {
			ttl := s.mode.timing.Tombstone
			if dl, ok := st.ctx.Deadline(); ok {
				// TTL floor: the propagated timeout remainder (§9.2).
				ttl = max(ttl, time.Until(dl))
			}
			if ps.dead {
				term = nil // peer lost: key-only (§10.4)
			}
			ps.addTombLocked(st.key.sid, term, now.Add(ttl))
		}
	}
	s.mu.Unlock()
	st.cancel(status.Error(codes.Canceled, "call finished"))
	if st.cancelTimeout != nil {
		st.cancelTimeout()
	}
	s.kickSweep()
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
	s.sw.stop()
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
	s.rootCancel(cause)
	s.sw.stop()
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
	// The transport says this peer is gone, and gateways issue a fresh key
	// per connection, so nothing can ever address this state again: the
	// per-epoch containers — retained "until teardown" (§9.4, §10.6), and
	// this IS the teardown — die here. Without this, every disconnected
	// peer of a connection-oriented gateway would leak its container (the
	// sweep never GCs reliable ones, and the §15 dead-container cap only
	// evicts among containers of the *same* key).
	for k := range s.peers {
		if k.peer == peer {
			delete(s.peers, k)
		}
	}
	for k := range s.pendingResets {
		if k.peer == peer {
			delete(s.pendingResets, k)
		}
	}
	for k := range s.resetAt {
		if k.peer == peer {
			delete(s.resetAt, k)
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
	applyServer(*serverOption)
}

type serverOption struct {
	unary_int   grpc.UnaryServerInterceptor
	unary_ints  []grpc.UnaryServerInterceptor
	stream_int  grpc.StreamServerInterceptor
	stream_ints []grpc.StreamServerInterceptor

	reliable          *bool
	timing            Timing
	maxHandlerTimeout time.Duration
	rx                rxConfig
	methodRx          map[string]rxConfig
	limits            Limits
}

type serverOptionFunc func(*serverOption)

func (f serverOptionFunc) applyServer(o *serverOption) {
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
