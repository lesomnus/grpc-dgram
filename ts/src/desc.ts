// Method descriptors, payload codecs, and call options.
//
// The Go implementation plugs into grpc-go's generated code (G2); this port
// has no single canonical codegen to bind to, so methods are described
// explicitly. A descriptor's marshaller pair is the call's default codec
// ('' = proto on the wire, §12); protobuf-es or any other serializer plugs in
// as a three-line PayloadCodec.

import type { Metadata } from './metadata'

export interface PayloadCodec<T> {
  marshal(value: T): Uint8Array
  unmarshal(data: Uint8Array): T
}

export interface MethodDesc<Req = unknown, Res = unknown> {
  // Full method name, e.g. '/echo.EchoService/Once' (PROTOCOL.md §13).
  readonly path: string
  readonly clientStreams: boolean
  readonly serverStreams: boolean
  readonly request: PayloadCodec<Req>
  readonly response: PayloadCodec<Res>
  // Opaque per-message schemas for named codecs (see Server codecs option);
  // the core never reads them.
  readonly requestSchema?: unknown
  readonly responseSchema?: unknown
}

export type UnaryDesc<Req, Res> = MethodDesc<Req, Res> & { clientStreams: false; serverStreams: false }
export type ServerStreamingDesc<Req, Res> = MethodDesc<Req, Res> & { clientStreams: false; serverStreams: true }
export type ClientStreamingDesc<Req, Res> = MethodDesc<Req, Res> & { clientStreams: true; serverStreams: false }
export type BidiDesc<Req, Res> = MethodDesc<Req, Res> & { clientStreams: true; serverStreams: true }

type DescInit<Req, Res> = {
  request: PayloadCodec<Req>
  response: PayloadCodec<Res>
  requestSchema?: unknown
  responseSchema?: unknown
}

export function unaryMethod<Req, Res>(path: string, init: DescInit<Req, Res>): UnaryDesc<Req, Res> {
  return { path, clientStreams: false, serverStreams: false, ...init }
}

export function serverStreamingMethod<Req, Res>(path: string, init: DescInit<Req, Res>): ServerStreamingDesc<Req, Res> {
  return { path, clientStreams: false, serverStreams: true, ...init }
}

export function clientStreamingMethod<Req, Res>(path: string, init: DescInit<Req, Res>): ClientStreamingDesc<Req, Res> {
  return { path, clientStreams: true, serverStreams: false, ...init }
}

export function bidiMethod<Req, Res>(path: string, init: DescInit<Req, Res>): BidiDesc<Req, Res> {
  return { path, clientStreams: true, serverStreams: true, ...init }
}

export function isUnary(desc: MethodDesc<unknown, unknown>): boolean {
  return !desc.clientStreams && !desc.serverStreams
}

// NamedCodec backs a non-default wire codec name (PROTOCOL.md §12): the
// server resolves the OPEN's codec name against its registry and asks the
// codec for the method's marshaller pair; an unregistered name draws
// T{UNIMPLEMENTED}. The client forces one per call (ForceCodec parity).
export interface NamedCodec {
  resolve(desc: MethodDesc<unknown, unknown>): {
    request: PayloadCodec<unknown>
    response: PayloadCodec<unknown>
  }
}

// ForcedCodec replaces a call's codec on the client and names it on the OPEN
// frame (PROTOCOL.md §12). The client always knows its own codec locally, so
// terminal-payload decoding is well-defined on both sides.
export interface ForcedCodec<Req = unknown, Res = unknown> {
  name: string
  request: PayloadCodec<Req>
  response: PayloadCodec<Res>
}

export interface CallOptions<Req = unknown, Res = unknown> {
  // Cancels the call (gRPC ctx cancellation). An abort whose reason is a
  // StatusError propagates that status to the caller.
  signal?: AbortSignal
  // Call deadline as a budget from now. For unary calls in unreliable mode,
  // an absent value selects T_call (PROTOCOL.md §10.2); streaming calls have
  // no default deadline. The remaining budget travels on OPEN.
  timeoutMs?: number
  // Outgoing request metadata; rides the OPEN frame only (§11).
  metadata?: Metadata
  codec?: ForcedCodec<Req, Res>
  // Populated when the call finishes, regardless of its status (grpc-go
  // Header/Trailer call-option parity).
  onHeader?: (md: Metadata | undefined) => void
  onTrailer?: (md: Metadata | undefined) => void
}
