package drpc

import (
	"context"
	"math"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// This file resolves gRPC CallOptions into the per-call configuration drpc
// acts on, so the option surface generated code and gRPC users already know
// behaves the same here (PROTOCOL.md §12, §16).
//
// Honored: ForceCodecV2, CallContentSubtype, MaxCallRecvMsgSize,
// MaxCallSendMsgSize, OnFinish, Peer, PerRPCCredentials, Header, Trailer.
//
// OnFinish runs on whichever goroutine ends the call — usually the adapter's
// receive loop — and before the caller observes the result, which is what
// makes grpc.Peer(&p) safe to read on return. It must not block: everything
// that endpoint receives waits behind it.
//
// Deliberately inert, because the behavior they select does not exist here:
//   - WaitForReady / FailFast — there is no connectivity state machine to
//     wait on; a datagram Conn is always "ready" (PROTOCOL.md §16).
//   - MaxRetryRPCBufferSize — transparent retry is a non-goal (§1).
//   - StaticMethod — a grpc-go-internal metrics hint.
//   - AuthorityOverride / CustomCodec / ForceCodec (v1) — no authority
//     concept; the v1 codec interface is superseded by CodecV2.
//
// Nothing else is ignored silently: an unknown-to-drpc option that changes
// call semantics does not exist in grpc-go's exported set.

const (
	// gRPC's own defaults (grpc-go rpc_util.go): 4 MiB received per message,
	// effectively unlimited sent.
	defaultMaxRecvMsgSize = 1024 * 1024 * 4
	defaultMaxSendMsgSize = math.MaxInt32
)

// callInfo is the resolved per-call configuration.
type callInfo struct {
	codec     encoding.CodecV2
	codecName string // "" = proto (the wire default, §12)

	maxRecv int
	maxSend int

	onFinish []func(error)
	peerOut  []*peer.Peer
	// creds accumulate: grpc-go applies dial-option AND call-option
	// credentials, never one instead of the other.
	creds []credentials.PerRPCCredentials
}

func (c *Conn) newCallInfo() *callInfo {
	return &callInfo{
		codec:   defaultCodec,
		maxRecv: c.maxRecv,
		maxSend: c.maxSend,
		creds:   c.creds,
	}
}

// resolveCallOptions folds opts into a callInfo. Later options win, matching
// grpc-go's "options are applied in order" contract.
func (c *Conn) resolveCallOptions(opts []grpc.CallOption) (*callInfo, error) {
	ci := c.newCallInfo()
	subtype := ""
	forced := false
	for _, o := range opts {
		switch o := o.(type) {
		case grpc.ForceCodecV2CallOption:
			if o.CodecV2 == nil {
				return nil, status.Error(codes.Internal, "drpc: ForceCodecV2 with a nil codec")
			}
			// Codecs are registered and looked up lowercase (grpc-go does
			// the same), so the name that goes on the wire must be too.
			ci.codec, ci.codecName, forced = o.CodecV2, strings.ToLower(o.CodecV2.Name()), true
		case grpc.ContentSubtypeCallOption:
			subtype = strings.ToLower(o.ContentSubtype)
		case grpc.MaxRecvMsgSizeCallOption:
			ci.maxRecv = o.MaxRecvMsgSize
		case grpc.MaxSendMsgSizeCallOption:
			ci.maxSend = o.MaxSendMsgSize
		case grpc.OnFinishCallOption:
			if o.OnFinish != nil {
				ci.onFinish = append(ci.onFinish, o.OnFinish)
			}
		case grpc.PeerCallOption:
			if o.PeerAddr != nil {
				ci.peerOut = append(ci.peerOut, o.PeerAddr)
			}
		case grpc.PerRPCCredsCallOption:
			if o.Creds != nil {
				ci.creds = append(ci.creds, o.Creds)
			}
		}
	}
	// A forced codec marshals; an explicit content subtype still decides what
	// the wire advertises, as in grpc-go — that combination is how a
	// passthrough proxy re-encodes without lying about the format (§12).
	if subtype != "" {
		if !forced {
			codec := encoding.GetCodecV2(subtype)
			if codec == nil {
				return nil, status.Errorf(codes.Internal, "drpc: no codec registered for content-subtype %q", subtype)
			}
			ci.codec = codec
		}
		ci.codecName = subtype
	}
	return ci, nil
}

// applyPerRPCCredentials merges the credential metadata into md
// (PROTOCOL.md §15: drpc authenticates nothing itself; per-RPC credentials
// are a metadata producer, and the channel is expected to be encrypted).
func (c *Conn) applyPerRPCCredentials(ctx context.Context, ci *callInfo, method string, md metadata.MD) (metadata.MD, error) {
	if len(ci.creds) == 0 {
		return md, nil
	}
	// Stock gRPC credentials read credentials.RequestInfo out of the ctx and
	// refuse to hand over a token when its AuthInfo does not report a private
	// channel. drpc cannot attest the channel, so the assertion the
	// application made with WithAssumeTransportSecurity is what fills it in —
	// and without it, credentials that demand security correctly refuse.
	level := credentials.InvalidSecurityLevel
	if c.assumeSecure {
		level = credentials.PrivacyAndIntegrity
	}
	ctx = credentials.NewContextWithRequestInfo(ctx, credentials.RequestInfo{
		Method:   method,
		AuthInfo: assumedAuthInfo{level: level},
	})

	out := md.Copy()
	if out == nil {
		out = metadata.MD{}
	}
	for _, cr := range ci.creds {
		if cr.RequireTransportSecurity() && !c.assumeSecure {
			return nil, status.Error(codes.Unauthenticated,
				"drpc: these credentials require transport security, which drpc cannot attest; "+
					"pass drpc.WithAssumeTransportSecurity() when the channel is encrypted (DTLS/WSS/WebRTC)")
		}
		data, err := cr.GetRequestMetadata(ctx, credentialsAudience(c.authority, method))
		if err != nil {
			return nil, credsError(err)
		}
		for k, v := range data {
			k = strings.ToLower(k) // gRPC keys are lowercase (§11)
			out[k] = append(out[k], v)
		}
	}
	// Credential-produced metadata goes through the same gate as the
	// application's: values become bytes on the wire, so nothing downstream
	// would catch an illegal key or value any more (§11).
	if err := validateMD(out); err != nil {
		return nil, mdStatusErr(err)
	}
	return out, nil
}

// assumedAuthInfo is the AuthInfo drpc hands to credentials: it reports the
// security level the application asserted, since the library itself cannot
// inspect the channel (PROTOCOL.md §15).
type assumedAuthInfo struct{ level credentials.SecurityLevel }

func (a assumedAuthInfo) AuthType() string { return "drpc-assumed" }

func (a assumedAuthInfo) GetCommonAuthInfo() credentials.CommonAuthInfo {
	return credentials.CommonAuthInfo{SecurityLevel: a.level}
}

// credsError maps a credential failure the way grpc-go does, including
// gRFC A54's rewrite of control-plane-restricted codes: a credential provider
// must not be able to make a call look like an application-level failure.
func credsError(err error) error {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists,
			codes.FailedPrecondition, codes.Aborted, codes.OutOfRange, codes.DataLoss:
			return status.Errorf(codes.Internal, "drpc: received per-RPC creds error with illegal status: %v", err)
		}
		return err
	}
	return status.Errorf(codes.Internal, "drpc: per-RPC creds failed due to error: %v", err)
}

// credentialsAudience mirrors grpc-go's createAudience exactly — scheme
// included. Credential providers mint audience claims from this string
// (jwtAccess derives the JWT "aud" from it), and servers validate them
// against the https:// form, so a drpc:// scheme would produce tokens no
// audience-checking server accepts.
func credentialsAudience(authority, method string) string {
	pos := strings.LastIndex(method, "/")
	if pos == -1 {
		pos = len(method)
	}
	return "https://" + authority + method[:pos]
}

// endOfCall runs the OnFinish callbacks for a call that never started. A
// created call reports through clientStream.reportFinish instead, exactly
// once.
func endOfCall(ci *callInfo, err error) {
	if ci == nil {
		return
	}
	for _, f := range ci.onFinish {
		f(err)
	}
}

// checkSendSize enforces MaxCallSendMsgSize with grpc-go's status and
// wording. A limit of 0 rejects everything, as it does on gRPC — reading it
// as "unlimited" would turn a deliberate lockdown into an open door.
func checkSendSize(n, limit int) error {
	if n > limit {
		return status.Errorf(codes.ResourceExhausted, "drpc: trying to send message larger than max (%d vs. %d)", n, limit)
	}
	return nil
}

// checkRecvSize enforces MaxCallRecvMsgSize with grpc-go's status and
// wording. As with checkSendSize, 0 rejects everything.
func checkRecvSize(n, limit int) error {
	if n > limit {
		return status.Errorf(codes.ResourceExhausted, "drpc: received message larger than max (%d vs. %d)", n, limit)
	}
	return nil
}
