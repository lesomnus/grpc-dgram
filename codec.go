package drpc

import (
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/encoding/proto"
)

var defaultCodec = encoding.GetCodecV2(proto.Name)
