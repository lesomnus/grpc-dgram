package drpc_test

import (
	"context"

	drpc "github.com/lesomnus/grpc-dgram"
)

// wrap1 turns an envelope sink into the FrameHandler a Conn or Server takes,
// one frame per envelope. Tests use it to stand a closure in for an adapter;
// it deliberately re-exposes nothing, which is fine here and is exactly why
// it is not a library export (PROTOCOL.md §3: a wrapper hides what NewConn
// discovers by type assertion).
func wrap1(h drpc.EnvelopeHandler) drpc.FrameHandler {
	return drpc.FrameHandlerFunc(func(ctx context.Context, f *drpc.Frame) error {
		e := &drpc.Envelope{}
		e.SetFrames([]*drpc.Frame{f})
		return h.Send(ctx, e)
	})
}
