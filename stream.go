package drpc

import (
	"context"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var _ grpc.ServerStream = &stream{}

type stream struct {
	ctx    context.Context
	cancel context.CancelFunc

	sid uint32

	tx     FrameHandler
	tx_seq tx_seq
	rx     chan *Frame
	rx_seq uint32
}

func newStream(sid uint32, tx FrameHandler) *stream {
	s := &stream{sid: sid, tx: tx, rx: make(chan *Frame)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

func (s *stream) SetHeader(md metadata.MD) error {
	// TODO:
	return nil
}

func (s *stream) SendHeader(md metadata.MD) error {
	// TODO:
	return nil
}

func (s *stream) SetTrailer(md metadata.MD) {
	// TODO:
	return
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
	f.SetPayload(payload)

	return s.tx(s.ctx, f)
}

func (s *stream) send(f *Frame) error {
	f.SetSid(s.sid)
	f.SetSeq(s.tx_seq.next())
	return s.tx(s.ctx, f)
}

func (s *stream) RecvMsg(m any) error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case v, ok := <-s.rx:
			if !ok {
				return io.EOF
			}
			if time.Now().After(v.GetDeadline().AsTime()) {
				continue
			}

			seq := v.GetSeq()
			if s.rx_seq >= seq {
				continue
			}

			s.rx_seq = seq

			return v.unmarshal(m)
		}
	}
}
