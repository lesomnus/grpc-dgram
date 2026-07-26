package drpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

var (
	_ grpc.ClientStream = &clientStream{}
	_ grpc.ServerStream = &serverStream{}

	_ grpc.ServerTransportStream = serverTransportUnary{}
	_ grpc.ServerTransportStream = serverTransportStream{}
)

// errCallEnded unparks a flow-controlled sender when its call ends; callers
// map it to the contract's own end-of-call error (io.EOF on the client).
var errCallEnded = errors.New("drpc: call ended")

// toStatusErr converts err to a gRPC status error, mapping context errors to
// their canonical codes and honoring a status cause if one was attached via
// context.CancelCause.
func toStatusErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, context.Canceled.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
	}
	if errors.Is(err, ErrMessageTooLarge) {
		// The adapter refused the send: too large for the transport
		// (PROTOCOL.md §4.4).
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return status.Error(codes.Unknown, err.Error())
}

// ctxErr returns the status error describing why ctx ended, preferring the
// cancel cause when it is already a status error.
func ctxErr(ctx context.Context) error {
	cause := context.Cause(ctx)
	return toStatusErr(cause)
}

// enqueueRxFlow delivers f into a flow-controlled stream's buffer without
// blocking. A conforming peer never exceeds the window it was granted, so a
// full buffer here is a contract violation — and blocking on it would be the
// deadlock flow control exists to remove: the grant that would unpark the
// peer travels the very read loop this would stall (PROTOCOL.md §4.2).
func enqueueRxFlow(rx chan *Frame, f *Frame) bool {
	select {
	case rx <- f:
		return true
	default:
		return false
	}
}

// enqueueRxReliable delivers f into the per-stream buffer, blocking until
// there is room: dropping would violate the exact-sequence contract
// (PROTOCOL.md §14), so a slow consumer stalls Handle instead and the stall
// propagates into the transport's own flow control (§4.2). Bounded by the rx
// ctx (adapter teardown) and by the stream ending — a frame for a finished
// call is moot. It returns false only for the ctx bound: the frame is lost
// while the call is still live, which on a reliable channel must fail loud,
// not surface as a silent gap (the strict window already advanced).
func enqueueRxReliable(ctx context.Context, rx chan *Frame, f *Frame, done <-chan struct{}) bool {
	select {
	case rx <- f:
		// A ready buffer always wins: a dead rx ctx must not race delivery
		// (an adapter flushing its queue after transport death still delivers
		// every frame that fits).
		return true
	default:
	}
	select {
	case rx <- f:
	case <-done:
	case <-ctx.Done():
		return false
	}
	return true
}

// enqueueRx delivers f into the per-stream buffer under the configured drop
// policy (unreliable mode, PROTOCOL.md §4.2). DropNewest discards the arrival
// on a full buffer; DropOldest evicts the oldest to admit it.
func enqueueRx(rx chan *Frame, f *Frame, policy DropPolicy, dropped *atomic.Uint32) {
	select {
	case rx <- f:
		return
	default:
	}
	if policy == DropOldest {
		select {
		case <-rx: // evict one
			dropped.Add(1)
		default:
		}
		select {
		case rx <- f:
			return
		default:
		}
	}
	dropped.Add(1)
}

// ---------------------------------------------------------------------------
// client stream
// ---------------------------------------------------------------------------

type clientStream struct {
	conn *Conn
	sid  uint32

	method        string
	clientStreams bool
	serverStreams bool

	ci        *callInfo
	codec     encoding.CodecV2
	codecName string
	comp      encoding.Compressor // message compressor, nil = none (§12.1)

	// Per-stream flow control (reliable mode, §4.2): flowTx is credit for
	// what this side sends, flowRx accounts what the application consumed.
	flowTx flowSender
	flowRx flowReceiver

	ctx       context.Context
	cancel    context.CancelFunc
	callerCtx context.Context
	stopAfter func() bool

	// tx state, guarded by txMu.
	txMu     sync.Mutex
	txSeq    txSeq
	txOpened bool
	txClosed bool
	openHdr  metadata.MD // outgoing request MD; rides the OPEN frame only

	// Retransmission obligations (unreliable mode, PROTOCOL.md §10.3);
	// guarded by txMu. Frames are stored for byte-identical resends.
	retxOpen   *Frame
	retxClose  *Frame
	retxAt     time.Time
	retxIval   time.Duration
	abortFrame *Frame

	// Idle clocks (unreliable mode, PROTOCOL.md §10.5).
	lastRx    atomic.Int64
	lastTx    atomic.Int64
	lastProbe atomic.Int64

	// rx sequencing, guarded by rxMu (transport side).
	rxMu        sync.Mutex
	rxWin       rxWindow
	srvEpoch    uint32 // server incarnation this stream is locked to
	srvEpochSet bool

	rx        chan *Frame
	rxCfg     rxConfig
	rxDropped atomic.Uint32

	// header/trailer state, guarded by stMu.
	stMu     sync.Mutex
	rxHeader metadata.MD
	trailer  metadata.MD
	hdrOnce  sync.Once
	hdrReady chan struct{}

	// Terminal state: term/termErr are written exactly once before done is
	// closed; readers load them only after observing done.
	doneOnce    sync.Once
	done        chan struct{}
	term        *Frame // server terminal frame, if that is what ended the call
	termErr     error  // local termination (cancel, RESET, DATA_LOSS)
	termPayload atomic.Bool
}

func newClientStream(ctx context.Context, c *Conn, sid uint32, method string, ci *callInfo, openHdr metadata.MD, clientStreams, serverStreams bool) *clientStream {
	rxCfg := c.rx.withReliableFloor(c.mode.reliable)
	s := &clientStream{
		conn: c,
		sid:  sid,

		method:        method,
		clientStreams: clientStreams,
		serverStreams: serverStreams,

		ci:        ci,
		codec:     ci.codec,
		codecName: ci.codecName,
		callerCtx: ctx,
		openHdr:   openHdr,

		rx:       make(chan *Frame, rxCfg.size),
		rxCfg:    rxCfg,
		hdrReady: make(chan struct{}),
		done:     make(chan struct{}),
	}
	if ci.compressor != "" {
		s.comp = encoding.GetCompressor(ci.compressor)
	}
	if c.mode.reliable {
		// Advertise this side's buffer as the server's initial send window
		// (§4.2); it rides the OPEN. Until the server advertises its own, this
		// side paces itself by the protocol's initial window.
		s.flowRx.enable(uint32(rxCfg.size))
		s.flowTx.assume(wInit)
	}
	s.rxWin.strict = c.mode.reliable
	if c.peer != nil {
		// grpc-go's ClientStream.Context() names the peer; interceptors read
		// it with the standard peer.FromContext (§6.4).
		ctx = peer.NewContext(ctx, c.peer)
	}
	s.ctx, s.cancel = context.WithCancel(ctx)

	n := nowNano()
	s.lastRx.Store(n)
	s.lastTx.Store(n)

	// The caller's ctx ending is the abort trigger (PROTOCOL.md §8): send a
	// terminal CLOSE and finish the call locally at once.
	s.stopAfter = context.AfterFunc(ctx, s.abortFromCtx)
	return s
}

// finalErr is the call's outcome as a gRPC status error (nil on success).
// Valid only once done is closed.
func (s *clientStream) finalErr() error {
	if s.termErr != nil {
		return s.termErr
	}
	if s.term != nil && codes.Code(s.term.GetCode()) != codes.OK {
		return s.term.Err()
	}
	return nil
}

// reportFinish runs the completion side effects gRPC promises to have applied
// by the time the caller sees the result: the grpc.Peer call option and the
// grpc.OnFinish callbacks. It runs before done closes, which is what makes
// them observable on return.
func (s *clientStream) reportFinish() {
	err := s.finalErr()
	if p := s.conn.peer; p != nil {
		for _, out := range s.ci.peerOut {
			*out = *p
		}
	}
	for _, f := range s.ci.onFinish {
		f(err)
	}
}

// recvInto delivers one received frame to the application: size-capped
// (grpc.MaxCallRecvMsgSize), then unmarshaled. A message past the cap fails
// the call, as it does on gRPC.
func (s *clientStream) recvInto(f *Frame, m any) error {
	payload, err := decodePayload(f, s.comp, s.ci.maxRecv)
	if err == nil {
		err = checkRecvSize(len(payload), s.ci.maxRecv)
	}
	if err != nil {
		// Oversize or undecodable: the RPC fails, as it does on gRPC.
		s.sendAbort(codes.ResourceExhausted)
		s.finishLocal(err)
		return err
	}
	return unmarshalBytes(payload, m, s.codec)
}

// handleRx processes one server frame for this stream. Called by Conn.Handle;
// serialized per stream via rxMu for the window state. In reliable mode it
// may block on a full buffer, bounded by the rx ctx (PROTOCOL.md §4.2).
func (s *clientStream) handleRx(ctx context.Context, f *Frame) {
	select {
	case <-s.done:
		return
	default:
	}

	if f.hasUnknownFlags() || !legalShape(f.shape()) {
		// A modifier bit from a newer peer changes something about this frame
		// that we cannot honor, and an illegal shape combination is not a
		// frame we can route: delivering either would be a silent corruption,
		// dropping it a silent gap (§7.1, §8).
		err := status.Errorf(codes.Internal, "drpc: frame carries unsupported flags %#x", f.GetFlags())
		s.sendAbort(codes.Internal)
		s.finishLocal(err)
		return
	}

	s.rxMu.Lock()
	if s.srvEpochSet && f.GetEpoch() != s.srvEpoch {
		// A frame from a different server incarnation (stale straggler after
		// a restart, or a raw-UDP injection) must not touch this live call.
		s.rxMu.Unlock()
		s.rxDropped.Add(1)
		return
	}
	s.rxMu.Unlock()

	if f.shape() == FlagWindow {
		// Stateless flow-control grant: no seq, no delivery, and only where
		// flow control exists at all — a stray or injected WINDOW must never
		// park an unreliable-mode sender (§4.2, §7, §15).
		if s.conn.mode.reliable {
			s.flowTx.grant(f.GetWindow())
		}
		return
	}

	s.rxMu.Lock()
	v := s.rxWin.check(f.GetSeq())
	if v == rxAccept && !s.srvEpochSet {
		s.srvEpoch = f.GetEpoch()
		s.srvEpochSet = true
	}
	s.rxMu.Unlock()

	switch v {
	case rxDup:
		// Validated: any server frame for the sid clears the OPEN
		// retransmission obligation (PROTOCOL.md §10.3).
		s.noteValidatedRx(f)
		return
	case rxBeyond:
		return
	case rxDataLoss:
		// Window overrun on a live stream: fail loudly (PROTOCOL.md §6.3)
		// and abort so the server stops.
		err := status.Error(codes.DataLoss, "seq window overrun: >W_fwd consecutive frames lost")
		s.sendAbort(codes.DataLoss)
		s.finishLocal(err)
		return
	case rxProtocolError:
		// Reliable-mode gap/duplicate: the transport is broken (§10.6).
		err := status.Error(codes.Internal, "reliable transport lost or reordered a frame")
		s.sendAbort(codes.Internal)
		s.finishLocal(err)
		return
	}
	s.noteValidatedRx(f)
	if s.conn.mode.reliable {
		// The first server frame — the creation ack for a streaming call —
		// advertises the server's receive window and replaces the assumed one
		// (§4.2). Absent means the peer does no flow control.
		s.flowTx.observe(f.GetWindow())
	}

	switch {
	case f.isTerminal():
		s.latchHeader(f)
		s.stMu.Lock()
		if f.HasTrailer() {
			s.trailer = f.GetTrailer().MD()
		}
		s.stMu.Unlock()
		s.finishTerm(f)
	case f.isHeaderFrame():
		s.latchHeader(f)
	case f.isData():
		if !s.serverStreams {
			// Off-shape: unary/client-streaming has no server data frames.
			s.rxDropped.Add(1)
			return
		}
		s.latchHeader(f)
		if s.conn.mode.reliable {
			if s.flowRx.active() {
				if !enqueueRxFlow(s.rx, f) {
					// The peer sent past its window: fail loud instead of
					// stalling the channel for every other call (§4.2).
					err := status.Error(codes.Internal, "drpc: peer exceeded the advertised flow-control window")
					s.sendAbort(codes.Internal)
					s.finishLocal(err)
				}
			} else if !enqueueRxReliable(ctx, s.rx, f, s.done) {
				// The rx ctx died mid-delivery: the transport is tearing down
				// (§4.5). The frame is gone and the window advanced — end the
				// call rather than leave a silent gap (§14).
				s.rxDropped.Add(1)
				s.finishLocal(status.Error(codes.Unavailable, "transport closed during delivery"))
			}
		} else {
			enqueueRx(s.rx, f, s.rxCfg.policy, &s.rxDropped)
		}
	default:
		s.rxDropped.Add(1)
	}
}

// latchHeader records the first header MD present on an accepted frame;
// frames without the header field never latch (PROTOCOL.md §7, §11).
func (s *clientStream) latchHeader(f *Frame) {
	if !f.HasHeader() {
		return
	}
	s.stMu.Lock()
	if s.rxHeader == nil {
		s.rxHeader = f.GetHeader().MD()
	}
	s.stMu.Unlock()
	s.hdrOnce.Do(func() { close(s.hdrReady) })
}

func (s *clientStream) openFrame() *Frame {
	s.txOpened = true
	f := &Frame{}
	f.SetEpoch(s.conn.epoch)
	f.SetSid(s.sid)
	f.SetSeq(s.txSeq.next()) // 1
	f.SetFlags(f.GetFlags() | FlagOpen)
	f.SetMethod(s.method)
	if s.codecName != "" {
		f.SetCodec(s.codecName)
	}
	if s.ci.compressor != "" {
		f.SetCompressor(s.ci.compressor)
	}
	if s.conn.mode.reliable {
		f.SetWindow(uint32(s.rxCfg.size))
	}
	if s.openHdr != nil {
		f.SetHeader(newMd(s.openHdr))
	}
	if dl, ok := s.ctx.Deadline(); ok {
		// The remaining call budget travels on OPEN (PROTOCOL.md §10.2).
		f.SetTimeout(durationpb.New(time.Until(dl)))
	}
	if !s.conn.mode.reliable {
		s.retxOpen = f
		s.scheduleRetxLocked()
	}
	return f
}

func (s *clientStream) nextFrame() *Frame {
	f := &Frame{}
	f.SetEpoch(s.conn.epoch)
	f.SetSid(s.sid)
	f.SetSeq(s.txSeq.next())
	return f
}

// sendOpen emits the eager OPEN for client-streaming/bidi calls
// (PROTOCOL.md §8).
func (s *clientStream) sendOpen() error {
	s.txMu.Lock()
	if s.txOpened || s.txClosed {
		// A racing abort already closed the call; an OPEN now would be a
		// protocol violation (its seq would not be 1).
		s.txMu.Unlock()
		return nil
	}
	f := s.openFrame()
	s.txMu.Unlock()
	return s.transmit(s.ctx, f)
}

// send marshals and transmits one message, returning any error. The public
// SendMsg wraps it with the grpc-go swallowing contract.
func (s *clientStream) send(m any) error {
	select {
	case <-s.done:
		return io.EOF
	default:
	}

	s.txMu.Lock()
	// Re-check under the lock: an abort may have won the race between the
	// done-check above and here.
	select {
	case <-s.done:
		s.txMu.Unlock()
		return io.EOF
	default:
	}
	if s.txClosed {
		aborted := s.abortFrame != nil
		s.txMu.Unlock()
		if aborted {
			// Closed by an abort, not by the user: not a contract violation.
			return io.EOF
		}
		return status.Error(codes.Internal, "SendMsg called after CloseSend")
	}

	buf, err := s.codec.Marshal(m)
	if err != nil {
		s.txMu.Unlock()
		return err
	}
	payload := buf.Materialize()
	buf.Free()
	opening := !s.txOpened
	s.txMu.Unlock()

	// Flow control (§4.2): the OPEN creates the call and is never credited;
	// every later message waits for the peer's window. Parking here — not in
	// the receiver's Handle — is what keeps one slow consumer from stalling
	// the whole channel.
	if !opening {
		_, ferr := s.flowTx.acquire(s.ctx, s.done, s.conn.mode.timing.Stall, nil)
		if ferr != nil {
			if ferr == errCallEnded {
				return io.EOF
			}
			return ferr
		}
	}

	s.txMu.Lock()
	select {
	case <-s.done:
		s.txMu.Unlock()
		return io.EOF
	default:
	}

	var f *Frame
	if !s.txOpened {
		f = s.openFrame()
		if !s.clientStreams {
			// Unary/server-streaming: the request piggybacks OPEN|CLOSE.
			// No code — this is the client's half-close (PROTOCOL.md §8).
			f.SetFlags(f.GetFlags() | FlagClose)
			s.txClosed = true
		}
	} else {
		f = s.nextFrame()
	}
	wire, cerr := setPayload(f, s.comp, payload)
	if cerr == nil {
		// grpc.MaxCallSendMsgSize caps the bytes that would go on the wire,
		// i.e. after compression, as grpc-go does.
		cerr = checkSendSize(len(wire), s.ci.maxSend)
	}
	if cerr != nil {
		s.txSeq.undo(f.GetSeq())
		if opening {
			s.txOpened, s.txClosed = false, false
			s.retxOpen = nil
		} else {
			s.flowTx.undo() // the message never reached the wire (§4.2)
		}
		s.txMu.Unlock()
		return cerr
	}
	s.txMu.Unlock()

	// Transmit outside txMu: a blocking adapter must not stall the whole
	// Conn through retire()'s c.mu -> txMu ordering.
	if err := s.transmit(s.ctx, f); err != nil {
		s.undoRefused(f, err)
		return err
	}
	return nil
}

// undoRefused reclaims f's seq when the adapter refused the send
// synchronously — the frame never reached the wire, so the next frame (the
// abort, or a later message) must reuse the number (see txSeq.undo).
func (s *clientStream) undoRefused(f *Frame, err error) {
	if !errors.Is(err, ErrMessageTooLarge) {
		return
	}
	s.txMu.Lock()
	s.txSeq.undo(f.GetSeq())
	if f.isOpen() {
		// The OPEN never reached the wire: nothing to retransmit, and the
		// call was never created at the server.
		s.txOpened, s.txClosed = false, false
		s.retxOpen, s.retxAt = nil, time.Time{}
	}
	s.txMu.Unlock()
	if f.isData() {
		s.flowTx.undo()
	}
}

func (s *clientStream) SendMsg(m any) error {
	err := s.send(m)
	if err != nil && err != io.EOF {
		// Marshal or transport failure: end the call (grpc-go does the
		// same), or a swallowed error would leave RecvMsg waiting for a
		// response that can never come.
		err = toStatusErr(err)
		s.sendAbort(codes.Canceled)
		s.finishLocal(err)
	}
	if !s.clientStreams {
		// grpc-go contract: SendMsg on a ClientStreams=false RPC returns nil
		// unconditionally; the status surfaces via RecvMsg.
		return nil
	}
	return err
}

func (s *clientStream) CloseSend() error {
	if !s.clientStreams {
		// Server-streaming: the request frame already half-closed.
		return nil
	}

	s.txMu.Lock()
	if s.txClosed {
		s.txMu.Unlock()
		return nil
	}
	select {
	case <-s.done:
		s.txMu.Unlock()
		return nil
	default:
	}
	s.txClosed = true
	f := s.nextFrame()
	f.SetFlags(FlagClose) // no code: half-close
	if !s.conn.mode.reliable {
		// Retransmit until the terminal or a RESET (PROTOCOL.md §10.3).
		s.retxClose = f
		s.scheduleRetxLocked()
	}
	s.txMu.Unlock()

	// grpc-go contract: CloseSend always returns nil.
	s.transmit(s.ctx, f)
	return nil
}

func (s *clientStream) RecvMsg(m any) error {
	// Prefer queued data so frames enqueued before the terminal are
	// delivered in order even after done closes.
	select {
	case f := <-s.rx:
		return s.recvBuffered(f, m)
	default:
	}
	select {
	case f := <-s.rx:
		return s.recvBuffered(f, m)
	case <-s.done:
	case <-s.ctx.Done():
		// The stream ctx is cancelled as part of finishing; prefer the
		// terminal outcome when both are ready.
		select {
		case <-s.done:
		default:
			return ctxErr(s.ctx)
		}
	}
	select {
	case f := <-s.rx:
		return s.recvBuffered(f, m)
	default:
	}
	return s.terminalRecv(m)
}

// recvBuffered delivers a frame taken out of the rx buffer and returns the
// slot to the peer as flow-control credit.
func (s *clientStream) recvBuffered(f *Frame, m any) error {
	if err := s.recvInto(f, m); err != nil {
		// A failed delivery ended the call; granting now would only draw a
		// RESET for a sid the peer has already forgotten (§4.2).
		return err
	}
	s.grantWindow(1)
	return nil
}

// grantWindow reports messages the application consumed and sends the
// resulting credit (§4.2). Called only for buffered data frames — a terminal
// payload never occupied a buffer slot.
func (s *clientStream) grantWindow(n uint32) {
	select {
	case <-s.done:
		return // the peer has forgotten this sid; a grant would draw a RESET
	default:
	}
	g := s.flowRx.consumed(n)
	if g == 0 {
		return
	}
	f := &Frame{}
	f.SetEpoch(s.conn.epoch)
	f.SetSid(s.sid)
	f.SetFlags(FlagWindow)
	f.SetWindow(g)
	s.transmit(context.WithoutCancel(s.ctx), f)
}

func (s *clientStream) terminalRecv(m any) error {
	if s.termErr != nil {
		return s.termErr
	}
	f := s.term
	if codes.Code(f.GetCode()) != codes.OK {
		return f.Err()
	}
	if f.HasPayload() && s.termPayload.CompareAndSwap(false, true) {
		// Terminal payload (unary response, SendAndClose result) is
		// delivered once; the next Recv reports end-of-stream.
		return s.recvInto(f, m)
	}
	return io.EOF
}

// Header blocks until the server's header metadata is latched or the call
// ends, then returns it (nil if the call ended without one). It never returns
// the call's status — grpc-go swallows it too, leaving RecvMsg to deliver it —
// and never returns a context error: a cancelled caller ctx ends the call
// through the abort path, which closes both channels below, so the outcome is
// the same whichever of the two the scheduler runs first (PROTOCOL.md §11).
func (s *clientStream) Header() (metadata.MD, error) {
	select {
	case <-s.hdrReady:
	case <-s.done:
	}
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.rxHeader, nil
}

func (s *clientStream) Trailer() metadata.MD {
	s.stMu.Lock()
	defer s.stMu.Unlock()
	return s.trailer
}

func (s *clientStream) Context() context.Context { return s.ctx }

// abortFromCtx runs when the caller's ctx ends: send a terminal CLOSE with
// the mapped code and finish locally (abort is local-immediate,
// PROTOCOL.md §10.3).
func (s *clientStream) abortFromCtx() {
	select {
	case <-s.done:
		return
	default:
	}
	code := codes.Canceled
	if errors.Is(context.Cause(s.callerCtx), context.DeadlineExceeded) {
		code = codes.DeadlineExceeded
	}
	s.sendAbort(code)
	s.finishLocal(ctxErr(s.callerCtx))
}

func (s *clientStream) sendAbort(code codes.Code) {
	s.txMu.Lock()
	if !s.txOpened {
		// The OPEN never reached the wire (a local refusal): there is no call
		// at the server to abort, and a bare CLOSE would only draw a delayed
		// RESET for a sid it has never seen (§9.3).
		s.txClosed = true
		s.txMu.Unlock()
		return
	}
	if s.abortFrame != nil {
		// A ctx AfterFunc and abandon() can both pass the done check; one
		// abort obligation is enough (§10.3) — the first one stands.
		s.txMu.Unlock()
		return
	}
	s.txClosed = true
	f := s.nextFrame()
	f.SetFlags(FlagClose)
	f.SetCode(uint32(code))
	// The abort obligation outlives the call on its tombstone
	// (PROTOCOL.md §10.3); retire() picks it up.
	s.abortFrame = f
	s.txMu.Unlock()

	// The stream ctx is (about to be) dead; keep its values for routing.
	s.transmit(context.WithoutCancel(s.ctx), f)
}

// finishTerm ends the call with the server's terminal frame.
func (s *clientStream) finishTerm(f *Frame) {
	s.doneOnce.Do(func() {
		s.term = f
		// The completion side effects run BEFORE done closes: gRPC promises
		// grpc.Peer(&p) and OnFinish are observable once the call returns,
		// and done closing is exactly what lets the caller return.
		s.reportFinish()
		// done must close before release cancels the stream ctx, so waiters
		// racing between the two observe the terminal, not a cancellation.
		close(s.done)
		s.release()
	})
}

// finishLocal ends the call with a local error (cancel, RESET, DATA_LOSS).
func (s *clientStream) finishLocal(err error) {
	s.doneOnce.Do(func() {
		s.termErr = err
		s.reportFinish()
		close(s.done)
		s.release()
	})
}

// abandon releases a call whose caller has returned. If no terminal was
// observed, the server must still be told to stop — otherwise the deferred
// cleanup could win the race against the ctx-cancel AfterFunc and swallow
// the abort frame, leaking the remote handler.
func (s *clientStream) abandon() {
	select {
	case <-s.done:
		return
	default:
	}
	if s.callerCtx.Err() != nil {
		s.abortFromCtx()
		return
	}
	s.sendAbort(codes.Canceled)
	s.finishLocal(status.Error(codes.Canceled, "call abandoned"))
}

// finishReset ends the call because the server declared it unknown, and
// enters the abort path: if the RESET was stale or forged while a real
// handler lives, the retransmitted abort reclaims it (PROTOCOL.md §9.3, §10.3).
func (s *clientStream) finishReset() {
	select {
	case <-s.done:
		return
	default:
	}
	s.sendAbort(codes.Canceled)
	s.finishLocal(status.Error(codes.Unavailable, "call reset by peer"))
}

func (s *clientStream) release() {
	s.flowTx.release()
	s.stopAfter()
	s.conn.retire(s)
	s.hdrOnce.Do(func() { close(s.hdrReady) })
	s.cancel()
}

// ---------------------------------------------------------------------------
// server stream
// ---------------------------------------------------------------------------

// serverTransportUnary bridges grpc.SetHeader / grpc.SendHeader /
// grpc.SetTrailer — which reach a handler through its ctx — to the unary
// call's own stream, so a unary handler flushes headers exactly like a
// streaming one (PROTOCOL.md §8, §11).
type serverTransportUnary struct{ *serverStream }

func (t serverTransportUnary) Method() string { return t.desc.fullname }

func (t serverTransportUnary) SetHeader(md metadata.MD) error {
	return t.serverStream.SetHeader(md)
}

func (t serverTransportUnary) SendHeader(md metadata.MD) error {
	return t.serverStream.SendHeader(md)
}

func (t serverTransportUnary) SetTrailer(md metadata.MD) error {
	t.serverStream.SetTrailer(md)
	return nil
}

type serverTransportStream struct{ *serverStream }

func (t serverTransportStream) Method() string { return t.desc.fullname }

func (t serverTransportStream) SetTrailer(md metadata.MD) error {
	t.serverStream.SetTrailer(md)
	return nil
}

type serverStream struct {
	server *Server
	key    callKey
	ps     *peerState // container of this client incarnation; set at open

	// reliable is the mode of the channel this call arrived on
	// (PROTOCOL.md §4.3): it selects strict sequencing, and gates the
	// probe/tombstone machinery of the peer-mixed server.
	reliable bool

	desc  *serviceDesc
	codec encoding.CodecV2
	comp  encoding.Compressor // message compressor, nil = none (§12.1)

	// Per-stream flow control (reliable mode, §4.2).
	flowTx flowSender
	flowRx flowReceiver

	ctx           context.Context
	cancel        context.CancelCauseFunc
	cancelTimeout context.CancelFunc // releases the handler-deadline timer

	// Idle clocks and ack-replay limiter (unreliable, PROTOCOL.md §10.5, §8).
	lastRx    atomic.Int64
	lastTx    atomic.Int64
	lastProbe atomic.Int64
	hReplayAt atomic.Int64

	// suppressTerm: the peer disowned the call (RESET) or vanished
	// (liveness expiry) — no terminal is sent, the tombstone is key-only.
	suppressTerm atomic.Bool

	// tx state, guarded by txMu.
	txMu       sync.Mutex
	txSeq      txSeq
	txHeader   metadata.MD // set via SetHeader/SendHeader
	hdrSent    bool        // header MD already rode some frame
	hdrFlushed bool        // the HANDLER flushed a header (SendHeader), §11
	hdrFrame   *Frame      // stored creation ack for byte-identical replay (§8)
	trailer    metadata.MD
	resp       []byte // captured SendAndClose payload (client-streaming)
	respSet    bool

	// rx sequencing, guarded by rxMu (transport side). The server enforces
	// incarnation isolation structurally — calls are keyed by
	// (peer, epoch, sid) in the demux — so no per-stream epoch gate here.
	rxMu  sync.Mutex
	rxWin rxWindow

	rx        chan *Frame
	rxCfg     rxConfig
	rxDropped atomic.Uint32
	eofOnce   sync.Once
	rxEOF     chan struct{}

	// Per-call size caps (grpc.MaxRecvMsgSize / MaxSendMsgSize).
	maxRecv int
	maxSend int
}

func newServerStream(ctx context.Context, srv *Server, key callKey, desc *serviceDesc, codec encoding.CodecV2, rxCfg rxConfig, reliable bool) *serverStream {
	s := &serverStream{
		server:   srv,
		key:      key,
		desc:     desc,
		codec:    codec,
		reliable: reliable,

		rx:    make(chan *Frame, rxCfg.size),
		rxCfg: rxCfg,
		rxEOF: make(chan struct{}),
	}
	s.maxRecv = srv.maxRecv
	s.maxSend = srv.maxSend
	if reliable {
		// Advertise this side's buffer as the client's initial send window;
		// it rides the creation ack H (§4.2).
		s.flowRx.enable(uint32(rxCfg.size))
	}
	s.rxWin.l = 1 // the accepted OPEN
	s.rxWin.strict = reliable
	n := nowNano()
	s.lastRx.Store(n)
	s.lastTx.Store(n)
	s.ctx, s.cancel = context.WithCancelCause(ctx)
	return s
}

// handleRx processes one client frame for this live call. Called by
// Server.Handle; serialized per stream via rxMu for the window state. In
// reliable mode it may block on a full buffer, bounded by the rx ctx
// (PROTOCOL.md §4.2).
func (s *serverStream) handleRx(ctx context.Context, f *Frame) {
	if f.hasUnknownFlags() || !legalShape(f.shape()) {
		// See the client twin: an unimplemented modifier bit or an illegal
		// shape fails the call rather than corrupting or gapping it (§7.1).
		s.cancel(status.Errorf(codes.Internal, "drpc: frame carries unsupported flags %#x", f.GetFlags()))
		return
	}
	if f.shape() == FlagWindow {
		// Stateless flow-control grant: no seq, no delivery, reliable only
		// (§4.2, §7).
		if s.reliable {
			s.flowTx.grant(f.GetWindow())
		}
		return
	}
	if f.isOpen() {
		if f.GetSeq() != 1 {
			// Off-shape: an OPEN's seq MUST be 1 (PROTOCOL.md §8).
			s.rxDropped.Add(1)
			return
		}
		if s.reliable {
			// No retransmission exists in reliable mode, so a duplicate OPEN
			// means the transport duplicated a frame: fail loud (§10.6).
			s.cancel(status.Error(codes.Internal, "reliable transport lost or reordered a frame"))
			return
		}
		// Duplicate OPEN (its seq 1 is always a dedup). For streaming calls
		// it re-elicits the creation ack (PROTOCOL.md §8 ack recovery);
		// unary is deadline-bounded and sends no ack.
		s.noteValidatedRx()
		if !s.desc.IsUnary() || s.storedH() != nil {
			// Streaming calls always owe an ack; a unary call owes one only if
			// its handler flushed a header, which the client is now blocked
			// on (§8 ack recovery, §11).
			s.replayH()
		}
		return
	}

	s.rxMu.Lock()
	v := s.rxWin.check(f.GetSeq())
	s.rxMu.Unlock()

	switch v {
	case rxDup:
		s.noteValidatedRx()
		return
	case rxBeyond:
		return
	case rxDataLoss:
		s.cancel(status.Error(codes.DataLoss, "seq window overrun: >W_fwd consecutive frames lost"))
		return
	case rxProtocolError:
		s.cancel(status.Error(codes.Internal, "reliable transport lost or reordered a frame"))
		return
	}
	s.noteValidatedRx()

	switch {
	case f.isTerminal():
		// Client abort: cancel the handler; the terminal T is produced as it
		// unwinds (PROTOCOL.md §10.3).
		s.cancel(f.Err())
	case f.isHalfClose():
		s.eofOnce.Do(func() { close(s.rxEOF) })
	case f.isData():
		if s.desc.IsUnary() || !s.desc.stream.ClientStreams {
			s.rxDropped.Add(1)
			return
		}
		if s.reliable {
			if s.flowRx.active() {
				if !enqueueRxFlow(s.rx, f) {
					s.cancel(status.Error(codes.Internal, "drpc: peer exceeded the advertised flow-control window"))
				}
			} else if !enqueueRxReliable(ctx, s.rx, f, s.ctx.Done()) {
				// See the client twin: teardown ate the frame — fail loud
				// rather than leave a silent gap on a reliable channel (§14).
				s.rxDropped.Add(1)
				s.cancel(status.Error(codes.Unavailable, "transport closed during delivery"))
			}
		} else {
			enqueueRx(s.rx, f, s.rxCfg.policy, &s.rxDropped)
		}
	default:
		s.rxDropped.Add(1)
	}
}

// noteValidatedRx runs for every validated client frame of this stream
// (accepted or dedup-dropped, PROTOCOL.md §9.1): refresh the idle clocks.
func (s *serverStream) noteValidatedRx() {
	n := nowNano()
	s.lastRx.Store(n)
	if s.ps != nil {
		s.ps.lastRx.Store(n)
	}
}

// transmit sends a non-probe frame, feeding the tx idle clocks.
func (s *serverStream) transmit(ctx context.Context, f *Frame) error {
	n := nowNano()
	s.lastTx.Store(n)
	if s.ps != nil {
		s.ps.lastTx.Store(n)
	}
	return s.server.tx.Handle(ctx, f)
}

// sendH emits the creation-ack header frame (PROTOCOL.md §8). The header
// field is present only if the handler already set one. The first H is
// stored for byte-identical replay.
func (s *serverStream) sendH() {
	s.txMu.Lock()
	f := s.nextFrameLocked()
	if s.reliable {
		f.SetWindow(uint32(s.rxCfg.size))
	}
	s.attachHeaderLocked(f, false)
	if s.hdrFrame == nil {
		s.hdrFrame = f
	}
	s.txMu.Unlock()
	s.transmit(s.ctx, f)
}

// storedH returns the H frame kept for byte-identical replay, if any.
func (s *serverStream) storedH() *Frame {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	return s.hdrFrame
}

// replayH answers a duplicate OPEN with the creation ack, rate-limited to
// one per RTI per call plus the peer's aggregate reply budget (PROTOCOL.md
// §8 ack recovery, §15): the stored H replayed byte-identically, else a
// freshly-seq'd H with the current header state.
func (s *serverStream) replayH() {
	n := nowNano()
	last := s.hReplayAt.Load()
	if n-last < int64(s.server.mode.timing.Retransmit) || !s.hReplayAt.CompareAndSwap(last, n) {
		return
	}
	s.server.mu.Lock()
	ok := s.server.allowReplyLocked(s.key.peer, n)
	s.server.mu.Unlock()
	if !ok {
		return
	}
	s.txMu.Lock()
	f := s.hdrFrame
	if f == nil {
		f = s.nextFrameLocked()
		s.attachHeaderLocked(f, false)
	}
	s.txMu.Unlock()
	s.transmit(context.WithoutCancel(s.ctx), f)
}

// probeDue emits a stream probe when both idle clocks passed T_probe
// (PROTOCOL.md §10.5). Probes reset neither idle clock.
func (s *serverStream) probeDue(now time.Time, probe time.Duration, epoch uint32) *Frame {
	n := now.UnixNano()
	p := int64(probe)
	if n-s.lastRx.Load() < p || n-s.lastTx.Load() < p || n-s.lastProbe.Load() < p {
		return nil
	}
	s.lastProbe.Store(n)
	f := &Frame{}
	f.SetEpoch(epoch)
	f.SetSid(s.key.sid)
	f.SetFlags(FlagPing)
	f.SetPeerEpoch(s.key.epoch)
	return f
}

func (s *serverStream) nextFrameLocked() *Frame {
	f := &Frame{}
	f.SetEpoch(s.server.epoch)
	f.SetSid(s.key.sid)
	f.SetSeq(s.txSeq.next())
	// Name the client incarnation (PROTOCOL.md §6.1): a restarted client
	// re-allocates sids, so the sid alone must never route this frame there.
	f.SetPeerEpoch(s.key.epoch)
	return f
}

// attachHeaderLocked piggybacks the pending header MD once (PROTOCOL.md §11).
// force makes the field present even when the handler set no metadata: an
// explicit SendHeader must unblock the client's Header(), while a creation
// ack must not pin the header to nil (§8).
func (s *serverStream) attachHeaderLocked(f *Frame, force bool) {
	if s.hdrSent {
		return
	}
	if s.txHeader != nil {
		f.SetHeader(newMd(s.txHeader))
		s.hdrSent = true
		return
	}
	if force {
		f.SetHeader(newMd(nil))
		s.hdrSent = true
	}
}

// errIllegalHeaderWrite mirrors grpc-go's transport.ErrIllegalHeaderWrite.
var errIllegalHeaderWrite = status.Error(codes.Internal, "drpc: SendHeader called multiple times")

func (s *serverStream) SetHeader(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	if err := validateMD(md); err != nil {
		return mdStatusErr(err)
	}
	s.txMu.Lock()
	defer s.txMu.Unlock()
	if s.hdrFlushed || s.hdrSent {
		// The header is already on the wire; joining more would silently lose
		// it (§11 first-wins). grpc-go returns the same error here.
		return errIllegalHeaderWrite
	}
	s.txHeader = metadata.Join(s.txHeader, md)
	return nil
}

// SendHeader flushes the header metadata as an H frame at once — including on
// unary calls, so a client's Header() returns before the response exists, as
// it does on gRPC (PROTOCOL.md §8, §11). Calling it twice is an error, as in
// grpc-go; the core's own creation ack is not a flush.
func (s *serverStream) SendHeader(md metadata.MD) error {
	if err := validateMD(md); err != nil {
		return mdStatusErr(err)
	}
	s.txMu.Lock()
	if s.hdrFlushed || s.hdrSent {
		// Flushed twice, or after the header already rode a data frame:
		// grpc-go's ErrIllegalHeaderWrite, and refusing is what keeps the
		// metadata from being silently dropped (§11).
		s.txMu.Unlock()
		return errIllegalHeaderWrite
	}
	s.hdrFlushed = true
	s.txHeader = metadata.Join(s.txHeader, md)
	sent := s.txHeader
	f := s.nextFrameLocked()
	s.attachHeaderLocked(f, true)
	if s.hdrFrame == nil {
		// Keep it for byte-identical replay: on a unary call this H is the
		// only thing a client's Header() is waiting for (§8 ack recovery).
		s.hdrFrame = f
	}
	s.txMu.Unlock()
	if err := s.transmit(s.ctx, f); err != nil {
		s.undoRefused(f, err)
		return toStatusErr(err)
	}
	_ = sent
	return nil
}

// SetTrailer records trailer metadata. Invalid metadata is dropped, as
// grpc-go does (the signature has no error to return) — validating here is
// what keeps it from failing the terminal frame's marshal (§11).
func (s *serverStream) SetTrailer(md metadata.MD) {
	if md.Len() == 0 {
		return
	}
	if err := validateMD(md); err != nil {
		return
	}
	s.txMu.Lock()
	defer s.txMu.Unlock()
	s.trailer = metadata.Join(s.trailer, md)
}

func (s *serverStream) Context() context.Context { return s.ctx }

func (s *serverStream) SendMsg(m any) error {
	buf, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	payload := buf.Materialize()
	buf.Free()

	if s.desc.stream.ServerStreams {
		// Flow control (§4.2): park until the client has room, instead of
		// letting its full buffer stall every call on the channel.
		_, ferr := s.flowTx.acquire(s.ctx, s.ctx.Done(), s.server.mode.timing.Stall, nil)
		if ferr != nil {
			if ferr == errCallEnded {
				return ctxErr(s.ctx)
			}
			return ferr
		}
	}

	s.txMu.Lock()
	if !s.desc.stream.ServerStreams {
		// Client-streaming: SendAndClose's message rides the terminal frame
		// (PROTOCOL.md §8).
		if s.respSet {
			s.txMu.Unlock()
			return status.Error(codes.Internal, "SendAndClose called multiple times")
		}
		s.resp = payload
		s.respSet = true
		s.txMu.Unlock()
		return nil
	}
	f := s.nextFrameLocked()
	wire, cerr := setPayload(f, s.comp, payload)
	if cerr == nil {
		cerr = checkSendSize(len(wire), s.maxSend)
	}
	if cerr != nil {
		s.txSeq.undo(f.GetSeq())
		s.flowTx.undo() // the message never reached the wire (§4.2)
		s.txMu.Unlock()
		return cerr
	}
	s.attachHeaderLocked(f, false)
	s.txMu.Unlock()

	if s.ctx.Err() != nil {
		// grpc-go returns the status describing why the stream ended.
		return ctxErr(s.ctx)
	}
	if err := s.transmit(s.ctx, f); err != nil {
		// A synchronous adapter refusal reclaims the seq so the terminal
		// carrying the handler's real status stays gap-free (see txSeq.undo).
		s.undoRefused(f, err)
		return err
	}
	return nil
}

// undoRefused reclaims f's seq when the adapter refused the send
// synchronously — the frame never reached the wire (see txSeq.undo).
func (s *serverStream) undoRefused(f *Frame, err error) {
	if !errors.Is(err, ErrMessageTooLarge) {
		return
	}
	s.txMu.Lock()
	s.txSeq.undo(f.GetSeq())
	s.txMu.Unlock()
	if f.isData() {
		s.flowTx.undo()
	}
}

func (s *serverStream) RecvMsg(m any) error {
	select {
	case f := <-s.rx:
		return s.recvBuffered(f, m)
	default:
	}
	select {
	case f := <-s.rx:
		return s.recvBuffered(f, m)
	case <-s.rxEOF:
		select {
		case f := <-s.rx:
			return s.recvBuffered(f, m)
		default:
		}
		return io.EOF
	case <-s.ctx.Done():
		return ctxErr(s.ctx)
	}
}

// recvInto delivers one received frame to the handler: size-capped
// (grpc.MaxRecvMsgSize), then unmarshaled.
func (s *serverStream) recvInto(f *Frame, m any) error {
	payload, err := decodePayload(f, s.comp, s.maxRecv)
	if err == nil {
		err = checkRecvSize(len(payload), s.maxRecv)
	}
	if err != nil {
		return err
	}
	return unmarshalBytes(payload, m, s.codec)
}

// recvBuffered delivers a frame taken out of the rx buffer and returns the
// slot to the client as flow-control credit (§4.2).
func (s *serverStream) recvBuffered(f *Frame, m any) error {
	if err := s.recvInto(f, m); err != nil {
		return err
	}
	s.grantWindow(1)
	return nil
}

// grantWindow sends flow-control credit for consumed messages.
func (s *serverStream) grantWindow(n uint32) {
	if s.ctx.Err() != nil {
		return // the call is over; a grant would draw a RESET
	}
	g := s.flowRx.consumed(n)
	if g == 0 {
		return
	}
	f := &Frame{}
	f.SetEpoch(s.server.epoch)
	f.SetSid(s.key.sid)
	f.SetPeerEpoch(s.key.epoch)
	f.SetFlags(FlagWindow)
	f.SetWindow(g)
	s.transmit(context.WithoutCancel(s.ctx), f)
}

// transmitTerminal sends the call's terminal frame, shrinking it until the
// channel accepts it. Every termination path depends on this frame arriving
// (§10.7), so its passengers are shed in order of expendability: first the
// status details (§5), then the response payload — which cannot be delivered
// anyway, so the call ends with ResourceExhausted rather than with silence.
// The frame is mutated in place and keeps its seq: a refused frame never
// reached the wire, so no sequence number is burned (§4.4) and what the
// tombstone stores for replay is exactly what was sent (§9.2).
func (s *serverStream) transmitTerminal(f *Frame) error {
	ctx := context.WithoutCancel(s.ctx)
	err := s.transmit(ctx, f)
	if err == nil || !errors.Is(err, ErrMessageTooLarge) {
		return err
	}
	if len(f.GetDetails()) > 0 {
		f.SetDetails(nil)
		err = s.transmit(ctx, f)
		if err == nil || !errors.Is(err, ErrMessageTooLarge) {
			return err
		}
	}
	if !f.HasPayload() && codes.Code(f.GetCode()) == codes.ResourceExhausted {
		return err // already minimal; the channel simply cannot carry it
	}
	f.ClearPayload()
	f.SetFlags(f.GetFlags() &^ FlagCompressed)
	f.setError(status.Errorf(codes.ResourceExhausted,
		"drpc: the terminal frame does not fit the transport: %v", err))
	return s.transmit(ctx, f)
}

// setResp stores the unary / SendAndClose response payload, which rides the
// terminal frame (PROTOCOL.md §8).
func (s *serverStream) setResp(payload []byte) {
	s.txMu.Lock()
	s.resp, s.respSet = payload, true
	s.txMu.Unlock()
}

// terminalFrame builds T after the handler returned (PROTOCOL.md §8).
// T re-carries the header MD once set so it survives first-frame loss.
func (s *serverStream) terminalFrame(err error) *Frame {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	f := s.nextFrameLocked()
	f.SetFlags(f.GetFlags() | FlagClose)
	if s.txHeader != nil {
		f.SetHeader(newMd(s.txHeader))
		s.hdrSent = true
	}
	if s.trailer != nil {
		f.SetTrailer(newMd(s.trailer))
	}
	if err != nil {
		f.setError(toStatusErr(err))
		return f
	}
	if s.respSet {
		wire, cerr := setPayload(f, s.comp, s.resp)
		if cerr == nil {
			// The cap measures what the frame carries — after compression, as
			// grpc-go does, and here rather than at marshal time because the
			// unary and SendAndClose responses only become wire bytes now.
			cerr = checkSendSize(len(wire), s.maxSend)
		}
		if cerr != nil {
			f.ClearPayload()
			f.SetFlags(f.GetFlags() &^ FlagCompressed)
			f.setError(toStatusErr(cerr))
			return f
		}
	}
	f.SetCode(uint32(codes.OK))
	return f
}

// serviceDesc describes one registered method, addressed by its full name
// always (PROTOCOL.md §13).
type serviceDesc struct {
	fullname string

	service *grpc.ServiceDesc
	method  *grpc.MethodDesc
	stream  *grpc.StreamDesc

	impl any
}

func (d *serviceDesc) IsUnary() bool { return d.method != nil }

func (d *serviceDesc) String() string {
	return d.fullname
}
