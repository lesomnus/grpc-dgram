package drpc

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
)

var (
	_ grpc.ServerStream = &serverStream{}
	_ grpc.ClientStream = &clientStream{}

	_ grpc.ServerTransportStream = &serverTransportUnary{}
	_ grpc.ServerTransportStream = &serverTransportStream{}
)

type serverTransportUnary struct {
	method  string
	header  metadata.MD
	trailer metadata.MD
}

func (t *serverTransportUnary) Method() string {
	return t.method
}
func (t *serverTransportUnary) SetHeader(md metadata.MD) error {
	t.header = md
	return nil
}
func (t *serverTransportUnary) SendHeader(md metadata.MD) error {
	t.header = md
	return nil
}
func (t *serverTransportUnary) SetTrailer(md metadata.MD) error {
	t.trailer = md
	return nil
}

type serverTransportStream struct {
	*serverStream
}

func (t serverTransportStream) Method() string {
	return t.serverStream.method
}

func (t serverTransportStream) SetTrailer(md metadata.MD) error {
	t.serverStream.SetTrailer(md)
	return nil
}

type serverStream struct {
	stream
	server    *Server
	rx_retain *Frame
}

func newServerStream(ctx context.Context, server *Server, sid uint32, desc *serviceDesc) *serverStream {
	s := &serverStream{
		stream: stream{
			sid: sid,

			codec: defaultCodec,

			method:       desc.fullname,
			method_index: desc.index,

			tx: server.tx,
			rx: make(chan *Frame, 10),
		},
		server: server,
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	return s
}

func (s *serverStream) SendHeader(md metadata.MD) error {
	// Header is piggybacked to the first frame, so it is
	// set to the stream and will be sent with the next frame.
	s.header = md
	return nil
}

func (s *serverStream) RecvMsg(m any) error {
	if s.rx_retain != nil {
		// `s.rx_retain` is mean to be used for keep last frame that
		// should be handled in next [RecvMsg] call.
		// However, currently it is only used for handling piggybacked
		// close frame in unary RPC so we can just return EOF without
		// handling the frame.
		return io.EOF
	}

	for {
		select {
		case <-s.ctx.Done():
			return io.EOF
		case v := <-s.rx:
			if v.HasCode() && v.GetCode() == uint32(codes.OK) {
				if v.GetPayload() == nil {
					// Client closes the stream.
					return io.EOF
				}

				s.rx_retain = v
				return v.unmarshal(m, s.codec)
			}
			if v.HasDeadline() && time.Now().After(v.GetDeadline().AsTime()) {
				continue
			}
			if !s.rx_seq.checkAndSet(rx_seq(v.GetSeq())) {
				continue
			}

			return v.unmarshal(m, s.codec)
		}
	}
}

func (s *serverStream) Close() error {
	s.server.mu.Lock()
	defer s.server.mu.Unlock()

	delete(s.server.ss, s.sid)
	return s.close()
}

type clientStream struct {
	stream
	conn    *Conn
	rx_last *Frame
}

func newClientStream(ctx context.Context, conn *Conn, sid uint32, method string) *clientStream {
	method_index := uint32(0)
	if v, ok := conn.methods.Load(method); ok {
		method_index = v.(uint32)
	}

	s := &clientStream{
		stream: stream{
			sid: sid,

			codec: defaultCodec,

			method:       method,
			method_index: method_index,

			tx: conn.tx,
			rx: make(chan *Frame, 10),
		},
		conn: conn,
	}
	s.ctx, s.cancel = context.WithCancel(ctx)

	return s
}

func (s *clientStream) CloseSend() error {
	f := s.nextFrame()
	f.SetCode(uint32(codes.OK))
	return s.tx.Handle(s.ctx, f)
}

func (s *clientStream) RecvMsg(m any) error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case v := <-s.rx:
			s.rx_last = v

			if s.method_index == 0 {
				i := v.GetMethodIndex()
				s.method_index = i
				s.conn.methods.Store(s.method, i)
			}
			if v.HasDeadline() && time.Now().After(v.GetDeadline().AsTime()) {
				continue
			}
			if !s.rx_seq.checkAndSet(rx_seq(v.GetSeq())) {
				continue
			}
			if v.GetCode() != uint32(codes.OK) {
				s.close()
				return v.Err()
			}

			return v.unmarshal(m, s.codec)
		}
	}
}

func (s *clientStream) Close() error {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()

	delete(s.conn.ss, s.sid)
	return s.close()
}

type stream struct {
	sid uint32

	codec      encoding.CodecV2
	codec_name string

	method       string
	method_index uint32

	ctx    context.Context
	cancel context.CancelFunc

	tx     FrameHandler
	tx_seq tx_seq
	rx     chan *Frame
	rx_seq rx_seq

	// Server may close the stream while client is sending.
	// If the client noticed that, this value is set to true and
	// the client should stop sending more frames.
	tx_closed atomic.Bool

	header  metadata.MD
	trailer metadata.MD
}

func (s *stream) Context() context.Context {
	return s.ctx
}

func (s *stream) SendMsg(m any) error {
	if s.tx_closed.Load() {
		// In the case of unary or server-streaming RPC, nil must be returned.
		// However, in that case, shouldn’t SendMsg not be called more than once in the first place?
		return io.EOF
	}

	buf, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	defer buf.Free()

	f := s.nextFrame()
	f.SetPayload(buf.Materialize())
	return s.tx.Handle(s.ctx, f)
}

func (s *stream) nextFrame() *Frame {
	seq := s.tx_seq.next() // seq starts with 1.

	f := &Frame{}
	f.SetSid(s.sid)
	f.SetSeq(seq)
	f.SetCodec(s.codec_name)
	if s.method_index == 0 {
		f.SetMethod(s.method)
	} else {
		f.SetMethodIndex(s.method_index)
	}

	if seq == 1 && s.header != nil {
		f.SetHeader(newMd(s.header))
		s.header = nil
	}

	return f
}

func (s *stream) put(ctx context.Context, f *Frame) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return nil
	case s.rx <- f:
		return nil
	}
}

func (s *stream) SetHeader(md metadata.MD) error {
	s.header = md
	return nil
}

func (s *stream) Header() (metadata.MD, error) {
	return s.header, nil
}

func (s *stream) SetTrailer(md metadata.MD) {
	s.trailer = md
}

func (s *stream) Trailer() metadata.MD {
	return s.trailer
}

func (s *stream) close() error {
	s.tx_closed.Store(true)
	s.cancel()
	return nil
}
