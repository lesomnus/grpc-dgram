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
