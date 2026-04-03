package drpc

import (
	"context"

	"google.golang.org/grpc/metadata"
)

func (x *Metadata) MD() metadata.MD {
	v := metadata.MD{}
	for k, e := range x.GetEntries() {
		v[k] = e.GetValues()
	}

	return v
}

func newMd(md metadata.MD) *Metadata {
	es := map[string]*Metadata_Entry{}
	for k, v := range md {
		es[k] = Metadata_Entry_builder{Values: v}.Build()
	}

	return Metadata_builder{Entries: es}.Build()
}

func newIncomingContext(ctx context.Context, req *Frame) context.Context {
	h := req.GetHeader()
	if h == nil {
		return ctx
	}

	md := h.MD()
	return metadata.NewIncomingContext(ctx, md)
}
