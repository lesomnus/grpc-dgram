package x

import (
	"fmt"

	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
)

const Name = "x-json"

type JsonCodecV2 struct {
	MarshalOptions   protojson.MarshalOptions
	UnmarshalOptions protojson.UnmarshalOptions
}

func (c *JsonCodecV2) Name() string {
	return Name
}

func (c *JsonCodecV2) Marshal(v any) (out mem.BufferSlice, err error) {
	vv := messageV2Of(v)
	if vv == nil {
		return nil, fmt.Errorf("protojsoncodec: failed to marshal, message is %T, want proto.Message", v)
	}

	b, err := c.MarshalOptions.Marshal(vv)
	if err != nil {
		return nil, err
	}

	if mem.IsBelowBufferPoolingThreshold(len(b)) {
		out = append(out, mem.SliceBuffer(b))
	} else {
		pool := mem.DefaultBufferPool()
		buf := pool.Get(len(b))
		copy(*buf, b)
		out = append(out, mem.NewBuffer(buf, pool))
	}

	return out, nil
}

func (c *JsonCodecV2) Unmarshal(data mem.BufferSlice, v any) error {
	vv := messageV2Of(v)
	if vv == nil {
		return fmt.Errorf("protojsoncodec: failed to unmarshal, message is %T, want proto.Message", v)
	}

	buf := data.MaterializeToBuffer(mem.DefaultBufferPool())
	defer buf.Free()

	return c.UnmarshalOptions.Unmarshal(buf.ReadOnlyData(), vv)
}

func messageV2Of(v any) proto.Message {
	switch v := v.(type) {
	case protoadapt.MessageV1:
		return protoadapt.MessageV2Of(v)
	case protoadapt.MessageV2:
		return v
	}
	return nil
}

func init() {
	encoding.RegisterCodecV2(&JsonCodecV2{})
}
