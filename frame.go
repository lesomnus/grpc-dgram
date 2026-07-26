package drpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// ErrMessageTooLarge is returned (or wrapped) by an adapter's Handle when a
// marshaled envelop cannot fit the transport's message limit. The core never
// fragments; it maps this error to the gRPC status ResourceExhausted on the
// owning call. See PROTOCOL.md §4.4.
var ErrMessageTooLarge = errors.New("drpc: message too large for the transport")

// nonzeroEpoch draws an incarnation nonce (PROTOCOL.md §6.1). Zero is
// excluded: it marks an absent peer_epoch echo, so an incarnation named 0
// could be addressed by frames that echo nothing.
func nonzeroEpoch() uint32 {
	for {
		if v := rand.Uint32(); v != 0 {
			return v
		}
	}
}

// FrameHandler is the core-facing seam: Conn and Server emit and consume
// individual frames. See PROTOCOL.md §3.
type FrameHandler interface {
	Handle(ctx context.Context, f *Frame) error
}

// ConnAttacher is discovered on the tx by NewConn the way TransportInfo is:
// the transport receives the Conn it serves and starts its own receive
// machinery, so the client needs no user-managed goroutine — matching gRPC.
// Conn.Close closes a tx that implements io.Closer, so one Close tears the
// whole endpoint down; the transport's Close must be idempotent.
//
// Servers deliberately have no equivalent: service registration must precede
// the first received frame (the registry freezes when serving starts), so a
// server transport is started explicitly — Serve/ServePeer — after
// RegisterService, the shape of grpc's Server.Serve(lis).
type ConnAttacher interface {
	AttachConn(c *Conn)
}

type FrameHandlerFunc func(ctx context.Context, f *Frame) error

func (f FrameHandlerFunc) Handle(ctx context.Context, frame *Frame) error {
	return f(ctx, frame)
}

// EnvelopHandler is the adapter-facing seam: the wire unit is always one
// Envelop holding 1..n frames. See PROTOCOL.md §3, §4.1.
type EnvelopHandler interface {
	Handle(ctx context.Context, e *Envelop) error
}

type EnvelopHandlerFunc func(ctx context.Context, e *Envelop) error

func (f EnvelopHandlerFunc) Handle(ctx context.Context, e *Envelop) error {
	return f(ctx, e)
}

// Wrap1 adapts an EnvelopHandler to a FrameHandler by wrapping each frame in
// a single-frame envelop (the no-batching default).
//
// The returned handler re-exposes nothing: if h also implements
// TransportInfo, the wrapper hides it from NewConn/NewServer discovery
// (PROTOCOL.md §3). Single-mode adapters should implement FrameHandler and
// TransportInfo on one type instead, mixed-mode gateways annotate per peer
// (NewReliableContext), or pass WithReliable explicitly.
func Wrap1(h EnvelopHandler) FrameHandler {
	return FrameHandlerFunc(func(ctx context.Context, f *Frame) error {
		e := &Envelop{}
		e.SetFrames([]*Frame{f})
		return h.Handle(ctx, e)
	})
}

// Unpack delivers each frame of e to h in order (PROTOCOL.md §4.1).
// Adapters use this on the receive path.
func Unpack(ctx context.Context, e *Envelop, h FrameHandler) error {
	var errs []error
	for _, f := range e.GetFrames() {
		if err := h.Handle(ctx, f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Status rebuilds the gRPC status the frame carries, including the rich
// details of google.rpc.Status when the terminal carried them (§5).
func (x *Frame) Status() *status.Status {
	if d := x.GetDetails(); len(d) > 0 {
		return status.FromProto(&spb.Status{
			Code:    int32(x.GetCode()),
			Message: x.GetDesc(),
			Details: d,
		})
	}
	return status.New(codes.Code(x.GetCode()), x.GetDesc())
}

func (x *Frame) Err() error {
	return x.Status().Err()
}

func (x *Frame) unmarshal(m any, codec encoding.CodecV2) error {
	buf := mem.SliceBuffer(x.GetPayload())
	return codec.Unmarshal(mem.BufferSlice{buf}, m)
}

// setPayload attaches payload to x, compressing it first when the call has a
// compressor and there is something to compress. A compressed frame carries
// FlagCompressed (PROTOCOL.md §7, §12.1); an empty message never does — a
// 0-byte payload is meaningful (§5) and gains nothing from a codec header.
// It returns the bytes that actually reached the frame (the wire length).
func setPayload(f *Frame, comp encoding.Compressor, payload []byte) ([]byte, error) {
	if comp == nil || len(payload) == 0 {
		f.SetPayload(payload)
		return payload, nil
	}
	var buf bytes.Buffer
	w, err := comp.Compress(&buf)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "drpc: compressor: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		return nil, status.Errorf(codes.Internal, "drpc: compressor: %v", err)
	}
	if err := w.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "drpc: compressor: %v", err)
	}
	out := buf.Bytes()
	if len(out) >= len(payload) {
		// Compression that expands (already-compressed or tiny payloads) would
		// push a message past the channel's ceiling for nothing (§4.4): send
		// it as-is. The per-frame flag makes that decision invisible to the
		// receiver.
		f.SetPayload(payload)
		return payload, nil
	}
	f.SetPayload(out)
	f.SetFlags(f.GetFlags() | FlagCompressed)
	return out, nil
}

// decodePayload returns f's message bytes, decompressing when the frame is
// marked. The expansion is bounded by maxRecv the way grpc-go bounds it: read
// one byte past the cap and fail with ResourceExhausted, so a decompression
// bomb cannot allocate without limit.
func decodePayload(f *Frame, comp encoding.Compressor, maxRecv int) ([]byte, error) {
	payload := f.GetPayload()
	if !f.isCompressed() {
		return payload, nil
	}
	if comp == nil {
		return nil, status.Error(codes.Internal, "drpc: frame is compressed but the call has no compressor")
	}
	r, err := comp.Decompress(bytes.NewReader(payload))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "drpc: decompress: %v", err)
	}
	limit := int64(maxRecv)
	if limit <= 0 {
		limit = defaultMaxRecvMsgSize
	}
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "drpc: decompress: %v", err)
	}
	if int64(len(out)) > limit {
		return nil, status.Errorf(codes.ResourceExhausted, "drpc: received message after decompression larger than max (> %d)", limit)
	}
	return out, nil
}

// unmarshalBytes decodes b into m with codec.
func unmarshalBytes(b []byte, m any, codec encoding.CodecV2) error {
	return codec.Unmarshal(mem.BufferSlice{mem.SliceBuffer(b)}, m)
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
		// status.WithDetails travels too (§5). It is a passenger: a terminal
		// the channel cannot carry is re-sent without it (transmitTerminal).
		if d := st.Proto().GetDetails(); len(d) > 0 {
			x.SetDetails(d)
		}
	} else {
		x.SetCode(uint32(codes.Unknown))
		x.SetDesc(err.Error())
	}
}

// resetFor builds a RESET answering f. The epoch echoes the offending frame —
// the one exception to the sender-epoch rule — so the receiver can match it
// against its own epoch. The peer_epoch is echoed too: on a client→server
// RESET it names the client incarnation of the offending call, so the server
// resets exactly that call (PROTOCOL.md §9.3).
func resetFor(f *Frame) *Frame {
	r := &Frame{}
	r.SetFlags(FlagReset)
	r.SetEpoch(f.GetEpoch())
	r.SetSid(f.GetSid())
	r.SetPeerEpoch(f.GetPeerEpoch())
	return r
}
