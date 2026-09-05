package drpc

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

var _ grpc.ClientConnInterface = &Conn{}

type Conn struct {
	// epoch is this Conn incarnation's nonce (PROTOCOL.md §6.1).
	epoch  uint32
	tx     FrameHandler
	mode   mode
	rx     rxConfig
	limits Limits

	// Endpoint-wide call defaults; per-call options override them
	// (see callinfo.go).
	compressor   string
	maxRecv      int
	maxSend      int
	creds        []credentials.PerRPCCredentials
	assumeSecure bool
	authority    string
	stats        []stats.Handler
	pstats       []ProtocolStats

	// peer names the remote end when the transport knows it (grpc.Peer,
	// PROTOCOL.md §6.4); connCtx carries the stats handler's per-connection
	// tags.
	peer    *peer.Peer
	connCtx context.Context

	mu        sync.Mutex
	ss        map[uint32]*clientStream
	tombs     map[uint32]*clientTomb
	resetAt   map[uint32]int64
	sidNext   uint32
	exhausted bool
	closed    bool

	// Connection flow control (reliable mode, PROTOCOL.md §4.2.1): connTx is
	// credit for what this side sends to the server across all calls, connRx
	// bounds what the server has buffered here (Limits.MaxPeerWindow).
	// srvEpoch is the server incarnation connTx is counted against — the
	// Conn-level twin of the per-stream lock (guarded by mu): a restarted
	// server on a surviving channel (§10.6) counts from zero, so the sender
	// starts over when a call first accepts a frame from a new one.
	connTx      flowSender
	connRx      peerFlowRx
	srvEpoch    uint32
	srvEpochSet bool

	// Peer-liveness clocks (unreliable mode, PROTOCOL.md §10.4).
	lastRx   atomic.Int64
	lastTx   atomic.Int64
	lastPing atomic.Int64
	sw       sweeper

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
		epoch:  nonzeroEpoch(),
		tx:     tx,
		mode:   resolveMode(tx, opt.reliable, opt.timing),
		rx:     opt.rx.withDefaults(),
		limits: opt.limits.withDefaults(),
		sw:     newSweeper(),
		ss:     map[uint32]*clientStream{},

		tombs:   map[uint32]*clientTomb{},
		resetAt: map[uint32]int64{},

		maxRecv:      sizeOr(opt.maxRecv, defaultMaxRecvMsgSize),
		maxSend:      sizeOr(opt.maxSend, defaultMaxSendMsgSize),
		creds:        opt.creds,
		assumeSecure: opt.assumeSecure,
		authority:    opt.authority,
		stats:        opt.stats,
		pstats:       opt.pstats,

		call_opts: []grpc.CallOption{},
	}
	// A transport that knows the remote end names it, so grpc.Peer(&p) works
	// as it does on gRPC (PROTOCOL.md §6.4).
	if p, ok := tx.(TransportPeer); ok {
		v.peer = p.Peer()
	}
	if v.mode.reliable {
		// The connection window (§4.2.1): this side paces itself by W_conn
		// from its first data frame — the server's first per-stream
		// advertisement settles it, a sid-0 WINDOW adds to it — and bounds
		// the server by MaxPeerWindow. Unreliable mode has neither: a full
		// buffer there drops by policy (§4.2).
		v.connTx.assume(wConn)
		v.connRx.enable(uint32(v.limits.MaxPeerWindow), 0)
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

	v.connBegin()

	// Last, with the Conn fully usable: the transport may start delivering
	// frames from inside AttachConn.
	if a, ok := tx.(ConnAttacher); ok {
		a.AttachConn(v)
	}
	return v
}

// Handle delivers one server frame to this Conn. Adapters call it for each
// frame of a received envelop, in order (PROTOCOL.md §9.1).
func (c *Conn) Handle(ctx context.Context, f *Frame) error {
	sid := f.GetSid()

	if f.isReset() {
		// Act only if the echoed epoch is ours; RESET never refreshes
		// liveness (PROTOCOL.md §9.1, §9.3).
		if f.GetEpoch() != c.epoch {
			return nil
		}
		c.protoEvent(ProtocolEvent{Kind: EventResetReceived, Sid: sid})
		if s := c.lookup(sid); s != nil {
			s.finishReset()
			return nil
		}
		// Obligation-clear at tombstones (PROTOCOL.md §10.3).
		c.clearTombAbort(sid)
		return nil
	}
	// Every other server frame echoes the client incarnation it addresses
	// (PROTOCOL.md §6.1). One that names another — a dead incarnation
	// coexisting behind this address, or an injection — must not touch this
	// Conn's calls or clocks: sids restart at 1 across restarts, so a sid
	// match means nothing without the epoch echo.
	if f.GetPeerEpoch() != c.epoch {
		if sid == 0 && (f.isPing() || f.shape() == FlagWindow) {
			// Another incarnation's keepalive or connection grant: not ours
			// to answer (§9.1).
			return nil
		}
		// Tell the desynced server to stop (§9.3): the RESET echoes the
		// offending frame's peer_epoch, so exactly that incarnation's call
		// dies at the server.
		return c.sendReset(ctx, f)
	}

	if sid == 0 && f.shape() == FlagWindow {
		// A connection grant (§4.2.1): additive credit for this side's sends
		// to the server across all calls. Only the incarnation the sender is
		// counted against may credit it — a grant from any other is dropped
		// in silence, as is one in unreliable mode; it never enables, never
		// refreshes liveness and never draws a RESET (§9.1, §9.3).
		if c.mode.reliable && c.serverEpochIs(f.GetEpoch()) {
			c.connTx.grant(f.GetWindow())
		}
		return nil
	}

	if f.shape() == FlagWindow {
		// A flow-control grant is advisory and stateless: for a live call it
		// credits the sender, for anything else it is dropped in silence — a
		// grant legitimately races the call's end, and answering it with a
		// RESET would turn every well-behaved stream into a RESET exchange
		// (§4.2, §9.3).
		if s := c.lookup(sid); s != nil {
			s.handleRx(ctx, f)
		}
		return nil
	}

	if f.isPing() {
		// Well-formed PINGs are validated: refresh peer liveness
		// (PROTOCOL.md §9.1, §10.4).
		c.lastRx.Store(nowNano())
		if sid == 0 {
			return nil
		}
		// Stream probe (§10.5): live stream → no-op; tombstoned or unknown
		// → RESET so the prober fails fast.
		if s := c.lookup(sid); s != nil {
			return nil
		}
		return c.sendReset(ctx, f)
	}

	if s := c.lookup(sid); s != nil {
		s.handleRx(ctx, f)
		return nil
	}

	// A sequenced server frame for a call this side no longer has still
	// names the incarnation that answered one of this Conn's OPENs — as
	// validated as the RESET decision below — so the Conn locks to it here
	// exactly as a live call's first accepted frame would (§4.2.1 Restart).
	// The one raise the server sends rides right behind its first H, and a
	// Conn whose first streaming call died before that H arrived would
	// otherwise drop the raise as a stranger's and stay at W_conn against a
	// larger window for its whole life: a forever-park (§4.2.1 Raise).
	c.lockServerEpoch(ctx, f.GetEpoch())

	// Whatever happens to it below, a data frame for a call this side no
	// longer has spent one connection credit at the server and is never
	// buffered: return it, or the window shrinks for good (§4.2.1).
	c.creditUnbuffered(ctx, f)

	c.mu.Lock()
	tomb := c.tombs[sid]
	c.mu.Unlock()
	if tomb != nil {
		// Straggler for a finished call: validated, dropped. A matching
		// terminal clears the pending abort (PROTOCOL.md §9.1-5b, §10.3).
		c.lastRx.Store(nowNano())
		if f.isTerminal() {
			c.clearTombAbort(sid)
		}
		return nil
	}

	// Unknown sid: tell the desynced server to stop — no OPEN can ever
	// arrive at a client (PROTOCOL.md §9.3).
	return c.sendReset(ctx, f)
}

// lockServerEpoch is called with the epoch of a sequenced server frame that
// answers one of this Conn's calls — a live call's first accepted frame, or
// one for a call already released (Handle's no-live-stream path, a done
// stream): the Conn locks to the first incarnation it hears and starts its
// connection sender over when it hears a new one (§4.2.1, §10.6). The dead
// incarnation's calls die by RESET on their own; what must not survive it is
// the sender's count, which the new server never saw. The new incarnation
// has never seen this side's window either, so the raise is due again — its
// container exists, it just answered a call. A reliable channel is ordered,
// so a dead incarnation's frame never follows a live one's (§10.6).
func (c *Conn) lockServerEpoch(ctx context.Context, epoch uint32) {
	c.mu.Lock()
	changed := c.srvEpochSet && c.srvEpoch != epoch
	c.srvEpoch, c.srvEpochSet = epoch, true
	c.mu.Unlock()
	if !changed || !c.mode.reliable {
		return
	}
	c.connTx.reassume(wConn)
	c.connRx.renew()
	c.raise(ctx)
}

// serverEpochIs reports whether epoch names the server incarnation the Conn
// is locked to — what a connection grant must echo to count.
func (c *Conn) serverEpochIs(epoch uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.srvEpochSet && c.srvEpoch == epoch
}

// serverCurrent reports whether a frame from epoch still has someone to
// return connection credit to: the incarnation the Conn is locked to, or any
// while it is locked to none. Frames of an incarnation the Conn moved past
// leave the buffer uncredited — their calls are RESET-failed anyway (§10.6),
// and the new server never counted them.
func (c *Conn) serverCurrent(epoch uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.srvEpochSet || c.srvEpoch == epoch
}

// creditUnbuffered returns the connection credit of a data frame this side
// received and will never buffer — off-shape, unknown flag, seq failure, a
// call it no longer has, an overrun refusal (§4.2.1). It never touched
// outstanding. Anything but a reliable-mode data frame spent no credit.
func (c *Conn) creditUnbuffered(ctx context.Context, f *Frame) {
	if !c.mode.reliable || !f.isData() || !c.serverCurrent(f.GetEpoch()) {
		return
	}
	if g := c.connRx.unadmitted(f.GetEpoch(), 1); g > 0 {
		c.grantPeer(ctx, g)
	}
}

// retirePeer returns the connection credit of n admitted frames of a call
// locked to epoch that stopped occupying its buffer: consumed, or discarded
// with the call (§4.2.1).
func (c *Conn) retirePeer(ctx context.Context, n uint32, epoch uint32) {
	if n == 0 {
		return
	}
	if g := c.connRx.retire(epoch, n, c.serverCurrent(epoch)); g > 0 {
		c.grantPeer(ctx, g)
	}
}

// raise lifts the server's assumed W_conn to this side's MaxPeerWindow, once
// per server incarnation (§4.2.1): a sid-0 grant of the difference, right
// behind the first OPEN — the server's container for this incarnation exists
// from then on. It is a MUST, not an optimisation: this side's grant cadence
// is computed against MaxPeerWindow, so a sender left at W_conn against a
// larger window would park before any batched grant fired.
func (c *Conn) raise(ctx context.Context) {
	if g := c.connRx.raise(); g > 0 {
		c.grantPeer(ctx, g)
	}
}

// grantPeer transmits a connection-window grant: sid 0, seq 0, no payload
// (§4.2.1, §7). It is a control frame like any other — outside every lock,
// so a blocking adapter never wedges Handle — and pointless once the Conn is
// closed.
func (c *Conn) grantPeer(ctx context.Context, n uint32) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return
	}
	f := &Frame{}
	f.SetEpoch(c.epoch)
	f.SetFlags(FlagWindow)
	f.SetWindow(n)
	c.tx.Handle(ctx, f)
}

// Close fails every live call with UNAVAILABLE. Adapters call it when the
// transport dies (PROTOCOL.md §4.5). It also closes a tx that implements
// io.Closer, so closing the Conn tears the whole endpoint down — transport
// goroutine and socket included. Idempotent (an io.Closer tx must be too:
// its death path calls back into Close).
func (c *Conn) Close(err error) {
	st := status.Error(codes.Unavailable, "transport closed")
	if err != nil {
		st = status.Errorf(codes.Unavailable, "transport closed: %v", err)
	}
	// Latch before failing: a stream inserted before the latch is caught by
	// failAll's snapshot, one attempted after it is refused by newStream —
	// nothing can slip between and hang with the pump gone.
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.failAll(st)
	// A sender parked on connection credit has no call left to wake it
	// through — its stream's release only covers the stream window (§4.2.1).
	c.connTx.release()
	c.sw.stop()
	c.connEnd()
	if cl, ok := c.tx.(io.Closer); ok {
		cl.Close()
	}
}

// protoEvent reports one endpoint-scope drpc protocol event (stats.go).
func (c *Conn) protoEvent(ev ProtocolEvent) {
	if len(c.pstats) == 0 {
		return
	}
	statsSink(c.pstats).emit(ev)
}

func (c *Conn) lookup(sid uint32) *clientStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ss[sid]
}

func (c *Conn) Invoke(ctx context.Context, method string, in, out any, opts ...grpc.CallOption) error {
	if !c.mode.reliable {
		if _, ok := ctx.Deadline(); !ok {
			// T_call: the default unary deadline (PROTOCOL.md §10.2).
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeoutCause(ctx, c.mode.timing.Call,
				status.Error(codes.DeadlineExceeded, "drpc: default call timeout"))
			defer cancel()
		}
	}
	opts = append(c.call_opts, opts...)
	return c.unary_int(ctx, method, in, out, nil, func(ctx context.Context, method string, in, out any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		ci, err := c.resolveCallOptions(opts)
		if err != nil {
			return err
		}
		s, err := c.newStream(ctx, method, ci, false, false)
		if err != nil {
			// The call never started; grpc-go still runs OnFinish on this
			// path, and interceptors release resources there.
			endOfCall(ci, err)
			return err
		}
		defer s.abandon()

		err = nil
		if serr := s.send(in); serr != nil && serr != io.EOF {
			// io.EOF means the call already ended (a racing abort or
			// teardown); the terminal outcome surfaces via RecvMsg below.
			err = toStatusErr(serr)
		}
		if err == nil {
			err = s.RecvMsg(out)
			if err == io.EOF {
				// A unary terminal without a payload is a protocol anomaly.
				err = status.Error(codes.Internal, "unary call ended without a response")
			}
		}

		// End is the RPC's last stats event, after the response was delivered
		// (gRPC parity).
		defer s.reportStatsEnd()

		// grpc-go populates Header/Trailer call options on finish regardless
		// of the status.
		for _, opt := range opts {
			switch opt := opt.(type) {
			case grpc.HeaderCallOption:
				*opt.HeaderAddr, _ = s.Header()
			case grpc.TrailerCallOption:
				*opt.TrailerAddr = s.Trailer()
			}
		}
		return err
	}, opts...)
}

func (c *Conn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	opts = append(c.call_opts, opts...)

	// The stream is created by the innermost streamer so the OPEN frame sees
	// the interceptor-final ctx and merged call options (PROTOCOL.md §8).
	streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ci, err := c.resolveCallOptions(opts)
		if err != nil {
			return nil, err
		}
		s, err := c.newStream(ctx, method, ci, desc.ClientStreams, desc.ServerStreams)
		if err != nil {
			endOfCall(ci, err)
			return nil, err
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

func (c *Conn) newStream(ctx context.Context, method string, ci *callInfo, clientStreams, serverStreams bool) (*clientStream, error) {
	// Outgoing metadata is validated before the call exists, as grpc-go does:
	// an illegal key or a non-printable value in a text key must surface as
	// Internal here, not as a marshal failure inside the adapter (§11).
	md, _ := metadata.FromOutgoingContext(ctx)
	if md != nil {
		if err := validateMD(md); err != nil {
			return nil, mdStatusErr(err)
		}
	}
	// Per-RPC credentials are a metadata producer; they ride the OPEN like any
	// request header (§11, §15).
	md, err := c.applyPerRPCCredentials(ctx, ci, method, md)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()

	if c.closed {
		// With the pump gone and the sweeper stopped, nothing could ever
		// terminate a call admitted now.
		c.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "drpc: the connection is closed")
	}
	if c.exhausted {
		c.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "sid space exhausted; create a new Conn")
	}
	c.sidNext++
	if c.sidNext == 0 {
		// The sid space is never recycled within an epoch (PROTOCOL.md §6.2).
		c.exhausted = true
		c.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "sid space exhausted; create a new Conn")
	}

	if !c.mode.reliable && len(c.ss) == 0 {
		// Arm the peer-liveness clocks with the first live call (§10.4).
		n := nowNano()
		c.lastRx.Store(n)
		c.lastTx.Store(n)
	}
	s := newClientStream(ctx, c, c.sidNext, method, ci, md, clientStreams, serverStreams)
	c.ss[s.sid] = s
	c.mu.Unlock()

	c.kickSweep()
	s.begin()
	return s, nil
}

type connOption struct {
	call_opts   []grpc.CallOption
	unary_int   grpc.UnaryClientInterceptor
	unary_ints  []grpc.UnaryClientInterceptor
	stream_int  grpc.StreamClientInterceptor
	stream_ints []grpc.StreamClientInterceptor

	reliable *bool
	timing   Timing
	rx       rxConfig
	limits   Limits

	maxRecv      *int
	maxSend      *int
	creds        []credentials.PerRPCCredentials
	assumeSecure bool
	authority    string
	stats        []stats.Handler
	pstats       []ProtocolStats
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
