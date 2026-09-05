// Interceptors: the TS-native shape of grpc-go's unary/stream, client/server
// interceptor chains (conn.go, server.go). An interceptor is a function over
// the call and a `next`; it may run code before and after `next`, change what
// `next` sees, skip `next` altogether, or call it again (a retry).
//
// Order is Go's: element 0 of the array runs outermost and the last element
// is handed the real invoker/handler — the reverse of Connect-ES, which
// applies the last interceptor first. Nothing here touches the wire; the
// OPEN frame is built by the innermost invoker, after the chain has run, so
// metadata an interceptor adds still rides it (PROTOCOL.md §8, §11).

import type { CallConfig, ClientStream } from './conn'
import type { MethodDesc } from './desc'
import type { ServerContext, ServerReader, ServerWriter } from './server'

// ---------------------------------------------------------------------------
// client
// ---------------------------------------------------------------------------

// ClientCall is what a client interceptor sees: the method, and the call
// configuration with the Conn's defaultCallOptions already folded in — and,
// for a unary call in unreliable mode, T_call already applied as timeoutMs
// (PROTOCOL.md §10.2), as Go's Invoke sets the ctx deadline before its chain
// runs. opts is mutable up to `next`: what the innermost invoker reads is what
// reaches the OPEN, and it is validated there (metadata, compressor, codec).
export interface ClientCall<Req = unknown, Res = unknown> {
  readonly desc: MethodDesc<Req, Res>
  opts: CallConfig<Req, Res>
}

// UnaryInvoker performs one unary call; the innermost one is the Conn's own.
export type UnaryInvoker = (req: unknown, call: ClientCall) => Promise<unknown>
export type UnaryClientInterceptor = (req: unknown, call: ClientCall, next: UnaryInvoker) => Promise<unknown>

// Streamer starts one streaming call. It is synchronous, as Conn.newStream
// is: an interceptor that needs async work does it on the stream it returns.
// The innermost streamer creates the ClientStream and, for client-streaming
// shapes, sends the eager OPEN — so the OPEN sees the chain's final opts.
export type Streamer = (call: ClientCall) => ClientStream<unknown, unknown>
export type StreamClientInterceptor = (call: ClientCall, next: Streamer) => ClientStream<unknown, unknown>

// ---------------------------------------------------------------------------
// server
// ---------------------------------------------------------------------------

// UnaryServerHandler is the shape `next` has for a unary server interceptor:
// the registered handler, or the rest of the chain. It resolves to the
// response, which the core marshals onto the terminal frame (PROTOCOL.md §8).
export type UnaryServerHandler = (req: unknown, ctx: ServerContext) => unknown
export type UnaryServerInterceptor = (req: unknown, ctx: ServerContext, next: UnaryServerHandler) => unknown

// StreamServerHandler is `next` for the three streaming shapes at once; the
// desc on ctx (clientStreams / serverStreams, PROTOCOL.md §13) tells them
// apart. For server-streaming the innermost handler reads the request that
// rode the OPEN off the stream itself, as grpc-go's generated handler does —
// an interceptor that recv()s first eats it. For client-streaming the value
// the chain resolves to IS the response (the handler's return); an
// interceptor returns what `next` returned, or substitutes its own.
export type StreamServerHandler = (stream: ServerReader<unknown> & ServerWriter<unknown>, ctx: ServerContext) => unknown
export type StreamServerInterceptor = (stream: ServerReader<unknown> & ServerWriter<unknown>, ctx: ServerContext, next: StreamServerHandler) => unknown

// ---------------------------------------------------------------------------
// chaining
// ---------------------------------------------------------------------------

// A chain folds to one interceptor of the same shape, once, at endpoint
// construction (NewConn / NewServer parity); each call hands it the real
// invoker or handler. Element 0 wraps everything; the last element gets the
// real one (Go's getChainUnaryInvoker / getChainUnaryHandler).

type Interceptor1<X, R> = (x: X, next: (x: X) => R) => R
type Interceptor2<X, Y, R> = (x: X, y: Y, next: (x: X, y: Y) => R) => R

/** @internal */
export function chain1<X, R>(is: readonly Interceptor1<X, R>[] | undefined): Interceptor1<X, R> | undefined {
  if (is === undefined || is.length === 0) return undefined
  const snapshot = [...is]
  return (x, last) => snapshot.reduceRight<(x: X) => R>((next, h) => (x2) => h(x2, next), last)(x)
}

/** @internal */
export function chain2<X, Y, R>(is: readonly Interceptor2<X, Y, R>[] | undefined): Interceptor2<X, Y, R> | undefined {
  if (is === undefined || is.length === 0) return undefined
  const snapshot = [...is]
  return (x, y, last) => snapshot.reduceRight<(x: X, y: Y) => R>((next, h) => (x2, y2) => h(x2, y2, next), last)(x, y)
}
