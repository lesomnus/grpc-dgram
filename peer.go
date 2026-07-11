package drpc

import "context"

type peerCtxKey struct{}

// NewPeerContext attaches the transport-level identity of the remote end to
// ctx. Adapters call this before delivering frames to Conn.Handle or
// Server.Handle, and read it back via PeerFromContext to route outgoing
// frames. key must be comparable. See PROTOCOL.md §6.4.
func NewPeerContext(ctx context.Context, key any) context.Context {
	return context.WithValue(ctx, peerCtxKey{}, key)
}

// PeerFromContext returns the peer key attached by NewPeerContext.
// Single-peer transports may never attach one; the nil peer is then the only
// peer.
func PeerFromContext(ctx context.Context) (any, bool) {
	v := ctx.Value(peerCtxKey{})
	return v, v != nil
}

type reliableCtxKey struct{}

// NewReliableContext annotates ctx with the reliability of the channel the
// frame arrived on. A gateway serving channels of differing reliability
// (e.g. WebRTC data channels) calls this per channel, before Handle, and the
// server then runs each peer in its channel's mode — strict sequencing with
// no timers on a reliable channel, the full timer machinery on an unreliable
// one — regardless of the server-wide default (PROTOCOL.md §4.3). The
// annotation MUST be constant for a given peer: reliability is a property of
// the channel, and one peer is one channel. Frames without the annotation
// run in the server's own mode.
func NewReliableContext(ctx context.Context, reliable bool) context.Context {
	return context.WithValue(ctx, reliableCtxKey{}, reliable)
}

func reliableFromContext(ctx context.Context) (bool, bool) {
	v, ok := ctx.Value(reliableCtxKey{}).(bool)
	return v, ok
}
