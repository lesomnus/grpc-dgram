package drpc

import (
	"fmt"
	"net"
	"net/netip"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// This file holds the gRPC-compatibility surface that is not part of the wire
// protocol: endpoint options mirroring grpc.DialOption/grpc.ServerOption, and
// the transport seam that names the remote end.

// TransportPeer is discovered on the tx by NewConn, the way TransportInfo and
// ConnAttacher are: a transport that knows the remote end names it, so
// grpc.Peer(&p) on a call — and peer.FromContext in a client interceptor —
// behave as they do on gRPC (PROTOCOL.md §6.4).
//
// Gateways name their peers per frame instead, by attaching a *peer.Peer to
// the rx ctx with peer.NewContext alongside drpc.NewPeerContext; the server
// copies it into the handler ctx.
type TransportPeer interface {
	Peer() *peer.Peer
}

// peerFromKey derives a *peer.Peer from an adapter's peer key when the key is
// itself an address. Adapters with opaque keys (a WebSocket connection, a
// DataChannel) attach a richer *peer.Peer to the rx ctx instead.
func peerFromKey(key any) *peer.Peer {
	switch v := key.(type) {
	case nil:
		return nil
	case *peer.Peer:
		return v
	case net.Addr:
		return &peer.Peer{Addr: v}
	case netip.AddrPort:
		return &peer.Peer{Addr: net.UDPAddrFromAddrPort(v)}
	}
	return nil
}

// peerOf is peerFromKey with a guaranteed result: an adapter whose peer key
// is opaque still gets a named address, so peer.FromContext never hands a
// handler a nil *peer.Peer.
func peerOf(key any) *peer.Peer {
	if p := peerFromKey(key); p != nil {
		return p
	}
	return &peer.Peer{Addr: keyAddr{key: key}}
}

// keyAddr names an adapter's opaque peer key as a net.Addr.
type keyAddr struct{ key any }

func (a keyAddr) Network() string { return "drpc" }
func (a keyAddr) String() string {
	if a.key == nil {
		return "unknown"
	}
	return fmt.Sprint(a.key)
}

// WithMaxRecvMsgSize caps the size of a single received message, as
// grpc.MaxCallRecvMsgSize / grpc.MaxRecvMsgSize do. Default 4 MiB, gRPC's own.
// A message past the cap fails its call with ResourceExhausted; it does not
// tear the channel down. This is an application-level guard and is unrelated
// to the transport's message ceiling (PROTOCOL.md §4.4), which the adapter
// owns.
func WithMaxRecvMsgSize(n int) interface {
	ConnOption
	ServerOption
} {
	return compatOption{maxRecv: &n}
}

// WithMaxSendMsgSize caps the size of a single sent message, as
// grpc.MaxCallSendMsgSize / grpc.MaxSendMsgSize do. Default: effectively
// unlimited, gRPC's own.
func WithMaxSendMsgSize(n int) interface {
	ConnOption
	ServerOption
} {
	return compatOption{maxSend: &n}
}

// WithPerRPCCredentials attaches credentials whose metadata rides every
// call's OPEN, mirroring grpc.WithPerRPCCredentials. drpc authenticates
// nothing itself (PROTOCOL.md §15) — credentials are a metadata producer, and
// the channel is expected to be encrypted. Credentials that report
// RequireTransportSecurity need WithAssumeTransportSecurity, since drpc
// cannot attest the channel.
func WithPerRPCCredentials(c credentials.PerRPCCredentials) ConnOption {
	return compatOption{creds: c}
}

// WithAssumeTransportSecurity asserts that the underlying channel is
// encrypted (DTLS, WSS, WebRTC), which lets credentials that require
// transport security be sent. drpc cannot verify the claim.
func WithAssumeTransportSecurity() ConnOption {
	t := true
	return compatOption{assumeSecure: &t}
}

// WithAuthority sets the authority used to build the audience passed to
// PerRPCCredentials ("https://<authority>/<service>", the same string gRPC
// builds, since that is what audience-validating servers expect). It appears
// nowhere on the wire.
func WithAuthority(a string) ConnOption {
	return compatOption{authority: &a}
}

// sizeOr resolves an optional size limit: an explicitly configured value —
// including 0, which grpc-go reads as "reject everything" — wins over the
// default.
func sizeOr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

type compatOption struct {
	maxRecv      *int
	maxSend      *int
	creds        credentials.PerRPCCredentials
	assumeSecure *bool
	authority    *string
}

func (o compatOption) apply(c *connOption) {
	if o.maxRecv != nil {
		c.maxRecv = o.maxRecv
	}
	if o.maxSend != nil {
		c.maxSend = o.maxSend
	}
	if o.creds != nil {
		c.creds = append(c.creds, o.creds)
	}
	if o.assumeSecure != nil {
		c.assumeSecure = *o.assumeSecure
	}
	if o.authority != nil {
		c.authority = *o.authority
	}
}

func (o compatOption) applyServer(s *serverOption) {
	if o.maxRecv != nil {
		s.maxRecv = o.maxRecv
	}
	if o.maxSend != nil {
		s.maxSend = o.maxSend
	}
}
