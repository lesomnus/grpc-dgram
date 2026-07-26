package main

import (
	"fmt"

	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// jsonCodec is what lets the browser page have no build step at all.
//
// The wire's default codec is protobuf (PROTOCOL.md §12), which a page would
// need a protobuf runtime and generated code for. Instead the page names the
// codec "json" on its OPEN frame; the server resolves that name against
// grpc-go's codec registry and marshals with protojson, so the handlers keep
// their generated stubs and never learn about it.
//
// With a bundler you would drop this file, point the page at
// @lesomnus/grpc-dgram/transport/protobuf-es with protoc-gen-es output, and let
// the default protobuf codec carry the call — the Go side would not change.
type jsonCodec struct{}

func init() { encoding.RegisterCodecV2(jsonCodec{}) }

// Name is the codec name that travels on the OPEN frame.
func (jsonCodec) Name() string { return "json" }

func (jsonCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("json codec: %T is not a proto.Message", v)
	}
	// Field names travel as protojson writes them: served_by is servedBy.
	data, err := protojson.Marshal(m)
	if err != nil {
		return nil, err
	}
	return mem.BufferSlice{mem.SliceBuffer(data)}, nil
}

func (jsonCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("json codec: %T is not a proto.Message", v)
	}
	// A browser is a peer like any other: tolerate fields it does not know.
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data.Materialize(), m)
}
