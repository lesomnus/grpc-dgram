package drpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

type FrameHandler interface {
	Handle(ctx context.Context, f *Frame) error
}

type FrameHandlerFunc func(ctx context.Context, f *Frame) error

func (f FrameHandlerFunc) Handle(ctx context.Context, frame *Frame) error {
	return f(ctx, frame)
}

func SkipNextFrame(hp *FrameHandler, h FrameHandler) FrameHandler {
	return FrameHandlerFunc(func(ctx context.Context, f *Frame) error {
		*hp = h
		return nil
	})
}

func (x *Frame) Status() *status.Status {
	return status.New(codes.Code(x.GetCode()), x.GetDesc())
}

func (x *Frame) Err() error {
	return x.Status().Err()
}

func (x *Frame) unmarshal(m any, codec encoding.CodecV2) error {
	buf := mem.SliceBuffer(x.GetPayload())
	return codec.Unmarshal(mem.BufferSlice{buf}, m)
}

func (x *Frame) getCodec() encoding.CodecV2 {
	name := x.GetCodec()
	if name == "" {
		return defaultCodec
	}

	return encoding.GetCodecV2(name)
}

func (x *Frame) setError(err error) {
	st, ok := status.FromError(err)
	if ok {
		x.SetCode(uint32(st.Code()))
		x.SetDesc(st.Message())
	} else {
		x.SetCode(uint32(codes.Unknown))
		x.SetDesc(err.Error())
	}
}
