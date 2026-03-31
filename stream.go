package drpc

import (
	"context"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
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
	server *Server
}

func newServerStream(ctx context.Context, server *Server, sid uint32, desc *serviceDesc) *serverStream {
	return &serverStream{
		stream: newStream(ctx, server.tx, sid, desc.fullname, desc.index),
		server: server,
	}
}

func (s *serverStream) SendHeader(md metadata.MD) error {
	// TODO:
	return nil
}

func (s *serverStream) RecvMsg(m any) error {
	for {
		select {
		case <-s.ctx.Done():
			return io.EOF
		case v := <-s.rx:
			if v.GetCode() == uint32(codes.Canceled) {
				// Client closes the stream.
				return io.EOF
			}
			if v.HasDeadline() && time.Now().After(v.GetDeadline().AsTime()) {
				continue
			}
			if !s.rx_seq.checkAndSet(rx_seq(v.GetSeq())) {
				continue
			}

			return v.unmarshal(m)
		}
	}
}

func (s *serverStream) Close() error {
	s.server.mu.Lock()
	defer s.server.mu.Unlock()

	delete(s.server.ss, s.sid)
	s.cancel()
	return nil
}

type clientStream struct {
	stream
	conn *Conn
	last *Frame
}

func newClientStream(ctx context.Context, conn *Conn, sid uint32, method string) *clientStream {
	method_index := uint32(0)
	if v, ok := conn.methods.Load(method); ok {
		method_index = v.(uint32)
	}

	return &clientStream{
		stream: newStream(ctx, conn.tx, sid, method, method_index),
		conn:   conn,
	}
}

func (s *clientStream) CloseSend() error {
	f := s.nextFrame()
	f.SetCode(uint32(codes.Canceled))
	return s.tx.Handle(s.ctx, f)
}

func (s *clientStream) RecvMsg(m any) error {
	for {
		select {
		case <-s.ctx.Done():
			return io.EOF
		case v := <-s.rx:
			s.last = v
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

			if v.HasCode() {
				s.trailer = v.GetTrailer().MD()
				s.cancel()
			}

			return v.unmarshal(m)
		}
	}
}

func (s *clientStream) Close() error {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()

	delete(s.conn.ss, s.sid)
	s.cancel()
	return nil
}

type stream struct {
	sid uint32

	method       string
	method_index uint32

	ctx    context.Context
	cancel context.CancelFunc

	tx     FrameHandler
	tx_seq tx_seq
	rx     chan *Frame
	rx_seq rx_seq

	header  metadata.MD
	trailer metadata.MD
}

func newStream(ctx context.Context, tx FrameHandler, sid uint32, method string, method_index uint32) stream {
	s := stream{
		sid: sid,

		method:       method,
		method_index: method_index,

		tx: tx,
		rx: make(chan *Frame, 10),
	}

	s.ctx, s.cancel = context.WithCancel(ctx)

	return s
}

func (s *stream) Context() context.Context {
	return s.ctx
}

func (s *stream) SendMsg(m any) error {
	// TODO: codec
	payload, err := proto.Marshal(m.(proto.Message))
	if err != nil {
		return err
	}

	f := s.nextFrame()
	f.SetPayload(payload)
	return s.tx.Handle(s.ctx, f)
}

func (s *stream) nextFrame() *Frame {
	f := &Frame{}
	f.SetSid(s.sid)
	f.SetSeq(s.tx_seq.next()) // seq starts with 1.
	if s.method_index == 0 {
		f.SetMethod(s.method)
	} else {
		f.SetMethodIndex(s.method_index)
	}

	if s.header != nil {
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
