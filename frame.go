package drpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type FrameHandler interface {
	Handle(ctx context.Context, f *Frame) error
}

type FrameHandlerFunc func(ctx context.Context, f *Frame) error

func (f FrameHandlerFunc) Handle(ctx context.Context, frame *Frame) error {
	return f(ctx, frame)
}

func (x *Frame) Status() *status.Status {
	return status.New(codes.Code(x.GetCode()), x.GetDesc())
}

func (x *Frame) Err() error {
	return x.Status().Err()
}

func (x *Frame) unmarshal(m any /*, codec encoding.CodecV2*/) error {
	if x.GetCode() != uint32(codes.OK) {
		return x.Err()
	}

	return proto.Unmarshal(x.GetPayload(), m.(proto.Message))
}
