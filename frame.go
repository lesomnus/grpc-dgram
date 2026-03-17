package drpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type FrameHandler func(ctx context.Context, f *Frame) error

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
