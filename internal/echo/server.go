package echo

import (
	"context"
	"errors"
	"io"
	"maps"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (x *EchoRequest) Error() error {
	s := x.GetStatus()
	if s == nil {
		return nil
	}

	return status.Error(codes.Code(s.GetCode()), s.GetMessage())
}

type EchoServer struct {
	UnimplementedEchoServiceServer
	MD         metadata.MD
	LazyHeader bool
}

func (s *EchoServer) Once(ctx context.Context, req *EchoRequest) (*EchoResponse, error) {
	s.handleMd(ctx)

	if err := req.Error(); err != nil {
		return nil, err
	}
	if req.GetOverVoid() {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	v := req.GetMessage()
	v = CircularShift(v, int(req.GetCircularShift()))
	return EchoResponse_builder{
		Message:     v,
		Sequence:    0,
		DateCreated: timestamppb.Now(),
	}.Build(), nil
}

func (s *EchoServer) many(seq *uint32, req *EchoRequest, h func(res *EchoResponse) error) error {
	if err := req.Error(); err != nil {
		return err
	}

	v := req.GetMessage()
	n := req.GetRepeat()
	for range n {
		v = CircularShift(v, int(req.GetCircularShift()))
		if err := h(EchoResponse_builder{
			Message:     v,
			Sequence:    *seq,
			DateCreated: timestamppb.Now(),
		}.Build()); err != nil {
			return err
		}

		*seq++
	}
	return nil
}

func (s *EchoServer) Many(req *EchoRequest, stream grpc.ServerStreamingServer[EchoResponse]) error {
	ctx := stream.Context()
	s.handleMd(ctx)

	if req.GetOverVoid() {
		<-ctx.Done()
		return ctx.Err()
	}

	seq := uint32(0)
	return s.many(&seq, req, stream.Send)
}

func (s *EchoServer) Buff(stream grpc.ClientStreamingServer[EchoRequest, EchoBatchResponse]) error {
	ctx := stream.Context()
	s.handleMd(ctx)

	items := []*EchoResponse{}

	seq := uint32(0)
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if err := s.many(&seq, req, func(res *EchoResponse) error {
			items = append(items, res)
			return nil
		}); err != nil {
			return err
		}
	}

	return stream.SendAndClose(EchoBatchResponse_builder{
		Items: items,
	}.Build())
}

func (s *EchoServer) Live(stream grpc.BidiStreamingServer[EchoRequest, EchoResponse]) error {
	ctx := stream.Context()
	s.handleMd(ctx)

	seq := uint32(0)
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := s.many(&seq, req, stream.Send); err != nil {
			return err
		}
	}
}

func (s *EchoServer) handleMd(ctx context.Context) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return
	}

	s.MD = md

	stream := grpc.ServerTransportStreamFromContext(ctx)
	if stream == nil {
		return
	}

	header := maps.Clone(md)
	header.Set("timing", "header")
	if s.LazyHeader {
		stream.SetHeader(header)
	} else {
		stream.SendHeader(header)
	}

	trailer := maps.Clone(md)
	trailer.Set("timing", "trailer")
	stream.SetTrailer(trailer)
}

func CircularShift(s string, n int) string {
	l := len(s)
	if l == 0 {
		return s
	}

	n %= l
	if n < 0 {
		n += l
	}

	return s[n:] + s[:n]
}
