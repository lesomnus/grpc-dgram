package drpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	_ grpc.ClientStream = &clientStream{}
	_ grpc.ServerStream = &serverStream{}

	_ grpc.ServerTransportStream = &serverTransportUnary{}
	_ grpc.ServerTransportStream = serverTransportStream{}
)

// defaultRxBuffer is the per-stream rx buffer size (PROTOCOL.md §4.2).
const defaultRxBuffer = 32

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
	return status.Error(codes.Unknown, err.Error())
}

// ctxErr returns the status error describing why ctx ended, preferring the
// cancel cause when it is already a status error.
func ctxErr(ctx context.Context) error {
	cause := context.Cause(ctx)
	return toStatusErr(cause)
}

// ---------------------------------------------------------------------------
// client stream
// ---------------------------------------------------------------------------

type clientStream struct {
	conn *Conn
	sid  uint32

	method        string
	methodIndex   uint32 // learned index snapshotted at stream creation; 0 = send the string
	clientStreams bool
	serverStreams bool

	codec     encoding.CodecV2
	codecName string

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

	// rx sequencing, guarded by rxMu (transport side).
	rxMu  sync.Mutex
	rxWin rxWindow

	rx        chan *Frame
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

func newClientStream(ctx context.Context, c *Conn, sid uint32, method string, clientStreams, serverStreams bool) *clientStream {
	s := &clientStream{
		conn: c,
		sid:  sid,

		method:        method,
		clientStreams: clientStreams,
		serverStreams: serverStreams,

		codec:     defaultCodec,
		callerCtx: ctx,

		rx:       make(chan *Frame, defaultRxBuffer),
		hdrReady: make(chan struct{}),
		done:     make(chan struct{}),
	}
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		s.openHdr = md
	}
	if v, ok := c.methods.Load(method); ok {
		if li := v.(learnedIndex); li.epoch == c.serverEpoch.Load() {
			s.methodIndex = li.index
		}
	}
	s.ctx, s.cancel = context.WithCancel(ctx)

	// The caller's ctx ending is the abort trigger (PROTOCOL.md §8): send a
	// terminal CLOSE and finish the call locally at once.
	s.stopAfter = context.AfterFunc(ctx, s.abortFromCtx)
	return s
}

// handleRx processes one server frame for this stream. Called by Conn.Handle;
// serialized per stream via rxMu for the window state.
func (s *clientStream) handleRx(f *Frame) {
	select {
	case <-s.done:
		return
	default:
	}

	s.rxMu.Lock()
	v := s.rxWin.check(f.GetSeq())
	s.rxMu.Unlock()

	switch v {
	case rxDrop:
		return
	case rxDataLoss:
		// Window overrun on a live stream: fail loudly (PROTOCOL.md §6.3)
		// and abort so the server stops.
		err := status.Error(codes.DataLoss, "seq window overrun: >W_fwd consecutive frames lost")
		s.sendAbort(codes.DataLoss)
		s.finishLocal(err)
		return
	}

	// Accepted: epoch tracking and method-index learning (PROTOCOL.md §13).
	s.conn.noteServerFrame(f, s.method)

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
		select {
		case s.rx <- f:
		default:
			s.rxDropped.Add(1)
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
	f.SetFlags(FlagOpen)
	if s.methodIndex > 0 {
		f.SetMethodIndex(s.methodIndex)
	} else {
		f.SetMethod(s.method)
	}
	if s.codecName != "" {
		f.SetCodec(s.codecName)
	}
	if s.openHdr != nil {
		f.SetHeader(newMd(s.openHdr))
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
	f := s.openFrame()
	s.txMu.Unlock()
	return s.conn.tx.Handle(s.ctx, f)
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
	defer s.txMu.Unlock()
	if s.txClosed {
		return status.Error(codes.Internal, "SendMsg called after CloseSend")
	}

	buf, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	defer buf.Free()

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
	f.SetPayload(buf.Materialize())
	return s.conn.tx.Handle(s.ctx, f)
}

func (s *clientStream) SendMsg(m any) error {
	err := s.send(m)
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
	s.txMu.Unlock()

	// grpc-go contract: CloseSend always returns nil.
	s.conn.tx.Handle(s.ctx, f)
	return nil
}

func (s *clientStream) RecvMsg(m any) error {
	// Prefer queued data so frames enqueued before the terminal are
	// delivered in order even after done closes.
	select {
	case f := <-s.rx:
		return f.unmarshal(m, s.codec)
	default:
	}
	select {
	case f := <-s.rx:
		return f.unmarshal(m, s.codec)
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
		return f.unmarshal(m, s.codec)
	default:
	}
	return s.terminalRecv(m)
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
		return f.unmarshal(m, s.codec)
	}
	return io.EOF
}

func (s *clientStream) Header() (metadata.MD, error) {
	select {
	case <-s.hdrReady:
	case <-s.done:
	case <-s.ctx.Done():
		select {
		case <-s.hdrReady:
		case <-s.done:
		default:
			return nil, ctxErr(s.ctx)
		}
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
	s.txClosed = true
	f := s.nextFrame()
	f.SetFlags(FlagClose)
	f.SetCode(uint32(code))
	s.txMu.Unlock()

	// The stream ctx is (about to be) dead; keep its values for routing.
	s.conn.tx.Handle(context.WithoutCancel(s.ctx), f)
}

// finishTerm ends the call with the server's terminal frame.
func (s *clientStream) finishTerm(f *Frame) {
	s.doneOnce.Do(func() {
		s.term = f
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

// finishReset ends the call because the server declared it unknown
// (PROTOCOL.md §9.3).
func (s *clientStream) finishReset() {
	s.finishLocal(status.Error(codes.Unavailable, "call reset by peer"))
}

func (s *clientStream) release() {
	s.stopAfter()
	s.conn.remove(s.sid)
	s.hdrOnce.Do(func() { close(s.hdrReady) })
	s.cancel()
}

// ---------------------------------------------------------------------------
// server stream
// ---------------------------------------------------------------------------

type serverTransportUnary struct {
	method  string
	header  metadata.MD
	trailer metadata.MD
}

func (t *serverTransportUnary) Method() string { return t.method }

func (t *serverTransportUnary) SetHeader(md metadata.MD) error {
	t.header = metadata.Join(t.header, md)
	return nil
}

func (t *serverTransportUnary) SendHeader(md metadata.MD) error {
	// A unary call has a single response frame; the header rides it.
	return t.SetHeader(md)
}

func (t *serverTransportUnary) SetTrailer(md metadata.MD) error {
	t.trailer = metadata.Join(t.trailer, md)
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

	desc  *serviceDesc
	codec encoding.CodecV2

	ctx    context.Context
	cancel context.CancelCauseFunc

	// tx state, guarded by txMu.
	txMu     sync.Mutex
	txSeq    txSeq
	txHeader metadata.MD // set via SetHeader/SendHeader
	hdrSent  bool        // header MD already rode some frame
	trailer  metadata.MD
	resp     []byte // captured SendAndClose payload (client-streaming)
	respSet  bool

	// rx sequencing, guarded by rxMu (transport side).
	rxMu  sync.Mutex
	rxWin rxWindow

	rx        chan *Frame
	rxDropped atomic.Uint32
	eofOnce   sync.Once
	rxEOF     chan struct{}
}

func newServerStream(ctx context.Context, srv *Server, key callKey, desc *serviceDesc, codec encoding.CodecV2) *serverStream {
	s := &serverStream{
		server: srv,
		key:    key,
		desc:   desc,
		codec:  codec,

		rx:    make(chan *Frame, defaultRxBuffer),
		rxEOF: make(chan struct{}),
	}
	s.rxWin.l = 1 // the accepted OPEN
	s.ctx, s.cancel = context.WithCancelCause(ctx)
	return s
}

// handleRx processes one client frame for this live call. Called by
// Server.Handle; serialized per stream via rxMu for the window state.
func (s *serverStream) handleRx(f *Frame) {
	if f.isOpen() {
		// Duplicate OPEN (its seq 1 is always a dedup). For streaming calls
		// it re-elicits the creation ack (PROTOCOL.md §8 ack recovery);
		// unary is deadline-bounded and sends no ack.
		if !s.desc.IsUnary() {
			s.replayH()
		}
		return
	}

	s.rxMu.Lock()
	v := s.rxWin.check(f.GetSeq())
	s.rxMu.Unlock()

	switch v {
	case rxDrop:
		return
	case rxDataLoss:
		s.cancel(status.Error(codes.DataLoss, "seq window overrun: >W_fwd consecutive frames lost"))
		return
	}

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
		select {
		case s.rx <- f:
		default:
			s.rxDropped.Add(1)
		}
	default:
		s.rxDropped.Add(1)
	}
}

// sendH emits the creation-ack header frame (PROTOCOL.md §8). The header
// field is present only if the handler already set one.
func (s *serverStream) sendH() {
	s.txMu.Lock()
	f := s.nextFrameLocked()
	s.attachHeaderLocked(f)
	s.txMu.Unlock()
	s.server.tx.Handle(s.ctx, f)
}

// replayH re-sends a creation ack in response to a duplicate OPEN, carrying
// the call's current tx-header state (PROTOCOL.md §8 ack recovery).
func (s *serverStream) replayH() {
	s.sendH()
}

func (s *serverStream) nextFrameLocked() *Frame {
	f := &Frame{}
	f.SetEpoch(s.server.epoch)
	f.SetSid(s.key.sid)
	f.SetSeq(s.txSeq.next())
	f.SetMethodIndex(s.desc.index)
	return f
}

// attachHeaderLocked piggybacks the pending header MD once (PROTOCOL.md §11).
func (s *serverStream) attachHeaderLocked(f *Frame) {
	if s.txHeader != nil && !s.hdrSent {
		f.SetHeader(newMd(s.txHeader))
		s.hdrSent = true
	}
}

func (s *serverStream) SetHeader(md metadata.MD) error {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	s.txHeader = metadata.Join(s.txHeader, md)
	return nil
}

func (s *serverStream) SendHeader(md metadata.MD) error {
	s.txMu.Lock()
	s.txHeader = metadata.Join(s.txHeader, md)
	f := s.nextFrameLocked()
	s.attachHeaderLocked(f)
	s.txMu.Unlock()
	return s.server.tx.Handle(s.ctx, f)
}

func (s *serverStream) SetTrailer(md metadata.MD) {
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

	s.txMu.Lock()
	if !s.desc.stream.ServerStreams {
		// Client-streaming: SendAndClose's message rides the terminal frame
		// (PROTOCOL.md §8).
		s.resp = payload
		s.respSet = true
		s.txMu.Unlock()
		return nil
	}
	f := s.nextFrameLocked()
	f.SetPayload(payload)
	s.attachHeaderLocked(f)
	s.txMu.Unlock()

	if err := s.ctx.Err(); err != nil {
		return io.EOF
	}
	return s.server.tx.Handle(s.ctx, f)
}

func (s *serverStream) RecvMsg(m any) error {
	select {
	case f := <-s.rx:
		return f.unmarshal(m, s.codec)
	default:
	}
	select {
	case f := <-s.rx:
		return f.unmarshal(m, s.codec)
	case <-s.rxEOF:
		select {
		case f := <-s.rx:
			return f.unmarshal(m, s.codec)
		default:
		}
		return io.EOF
	case <-s.ctx.Done():
		return ctxErr(s.ctx)
	}
}

// terminalFrame builds T after the handler returned (PROTOCOL.md §8).
// T re-carries the header MD once set so it survives first-frame loss.
func (s *serverStream) terminalFrame(err error) *Frame {
	s.txMu.Lock()
	defer s.txMu.Unlock()

	f := s.nextFrameLocked()
	f.SetFlags(FlagClose)
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
		f.SetPayload(s.resp)
	}
	f.SetCode(uint32(codes.OK))
	return f
}

// serviceDesc describes one registered method (PROTOCOL.md §13: indices are
// 1-based in registration order; 0 means unset).
type serviceDesc struct {
	index    uint32
	fullname string

	service *grpc.ServiceDesc
	method  *grpc.MethodDesc
	stream  *grpc.StreamDesc

	impl any
}

func (d *serviceDesc) IsUnary() bool { return d.method != nil }

func (d *serviceDesc) String() string {
	return fmt.Sprintf("%s#%d", d.fullname, d.index)
}
