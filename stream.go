package drpc

import (
	"context"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var (
	_ grpc.ServerStream = &serverStream{}
	_ grpc.ClientStream = &clientStream{}
)

type serverStream struct {
	server *Server
	stream
}

func newServerStream(ctx context.Context, server *Server, sid uint32) *serverStream {
	desc := server.methods[sid]

	return &serverStream{
		server: server,
		stream: newStream(ctx, server.tx, sid, desc.service.ServiceName, desc.index),
	}
}

func (s *serverStream) SetHeader(md metadata.MD) error {
	// TODO:
	return nil
}

func (s *serverStream) SendHeader(md metadata.MD) error {
	// TODO:
	return nil
}

func (s *serverStream) SetTrailer(md metadata.MD) {
	// TODO:
}

func (s *serverStream) RecvMsg(m any) error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case v, ok := <-s.rx:
			if !ok {
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
	close(s.rx)
	return nil
}

type clientStream struct {
	conn *Conn
	stream
}

func newClientStream(ctx context.Context, conn *Conn, sid uint32, method string) *clientStream {
	method_index := uint32(0)
	if v, ok := conn.methods.Load(method); ok {
		method_index = v.(uint32)
	}

	return &clientStream{
		conn:   conn,
		stream: newStream(ctx, conn.tx, sid, method, method_index),
	}
}

func (s *clientStream) Header() (metadata.MD, error) {
	// TODO:
	return metadata.MD{}, nil
}

func (s *clientStream) Trailer() metadata.MD {
	// TODO:
	return metadata.MD{}
}

func (s *clientStream) CloseSend() error {
	// TODO:
	return nil
}

func (s *clientStream) RecvMsg(m any) error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case v, ok := <-s.rx:
			if !ok {
				return io.EOF
			}
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

			return v.unmarshal(m)
		}
	}
}

func (s *clientStream) Close() error {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()

	delete(s.conn.ss, s.sid)
	close(s.rx)
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
}

func newStream(ctx context.Context, tx FrameHandler, sid uint32, method string, method_index uint32) stream {
	s := stream{
		sid: sid,

		method:       method,
		method_index: method_index,

		tx: tx,
		rx: make(chan *Frame),
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

	f := &Frame{}
	f.SetSid(s.sid)
	f.SetSeq(s.tx_seq.next())
	if s.method_index == 0 {
		f.SetMethod(s.method)
	} else {
		f.SetMethodIndex(s.method_index)
	}

	f.SetPayload(payload)
	return s.tx(s.ctx, f)
}
