// Connect-ES transport backed by a drpc Conn: use the standard Connect client
// ergonomics (`createClient(Service, transport)`) while the wire traffic runs
// over drpc (datagram RPC) to a drpc server — Go or TS. The bridge is thin
// because Connect's Transport receives protobuf-es method descriptors, exactly
// what `fromMethod` (src/transport/protobuf-es/index.ts) turns into a drpc MethodDesc, and the
// drpc Conn already implements all four RPC shapes.
//
// Entry: `@lesomnus/grpc-dgram/transport/connect`. `@connectrpc/connect` and
// `@bufbuild/protobuf` are optional peer dependencies — the core stays
// dependency-free; only this entry pulls them in.
//
//   import { createClient } from '@connectrpc/connect'
//   import { createDrpcTransport } from '@lesomnus/grpc-dgram/transport/connect'
//   import { EchoService } from './echo_pb'
//   const client = createClient(EchoService, createDrpcTransport(conn))
//   const res = await client.once({ message: 'hi' })       // unary
//   for await (const m of client.many({ message: 'x' })) …  // server streaming

import { create, type DescMessage, type DescMethodStreaming, type DescMethodUnary, type MessageInitShape, type MessageShape } from '@bufbuild/protobuf'
import { Code as ConnectCode, ConnectError, type ContextValues, type StreamResponse, type Transport, type UnaryResponse } from '@connectrpc/connect'
import type { ClientStream, Conn } from '../../conn'
import type { CallOptions } from '../../desc'
import type { Metadata } from '../../metadata'
import { fromMethod } from '../protobuf-es'
import { StatusError } from '../../status'

// headersToMetadata converts Connect's Fetch Headers into drpc Metadata. The
// Headers API combines same-name values with ", " (except set-cookie), so a
// multi-value entry round-trips as one comma-joined value — the same fidelity
// gRPC-over-HTTP has; single-value metadata (auth tokens, etc.) is exact.
function headersToMetadata(h: HeadersInit | undefined): Metadata | undefined {
  if (h === undefined) return undefined
  const headers = h instanceof Headers ? h : new Headers(h)
  let md: Metadata | undefined
  headers.forEach((value, key) => {
    ;(md ??= {})[key] = [value]
  })
  return md
}

// safeAppend adds one metadata entry to Headers, skipping any key/value the
// WHATWG Headers API rejects. drpc metadata is arbitrary strings (proto §11 —
// a value may hold a newline, control char, or non-latin1 codepoint like an
// emoji), but Headers.append throws a TypeError on those. Dropping the entry
// keeps the conversion total: a spec-legal server response must surface its
// message and status, never crash the call with a raw TypeError. Only entries
// HTTP headers cannot represent are lost.
function safeAppend(headers: Headers, key: string, value: string): void {
  try {
    headers.append(key, value)
  } catch {
    // unrepresentable as an HTTP header — drop rather than fail the call
  }
}

function appendMetadata(headers: Headers, md: Metadata | undefined): void {
  if (md === undefined) return
  for (const [key, values] of Object.entries(md)) {
    for (const v of values) safeAppend(headers, key, v)
  }
}

// metadataToHeaders converts drpc Metadata into Connect Headers, appending each
// value so multi-value entries are preserved on the wire (Headers re-combines
// them on read).
function metadataToHeaders(md: Metadata | undefined): Headers {
  const headers = new Headers()
  appendMetadata(headers, md)
  return headers
}

// toConnectError maps a drpc failure to a ConnectError. The gRPC status codes
// are numerically identical between the two Code enums, so the code carries
// straight over; header + trailer metadata are attached to ConnectError.metadata.
function toConnectError(err: unknown, header?: Metadata, trailer?: Metadata): ConnectError {
  const meta = metadataToHeaders(header)
  appendMetadata(meta, trailer)
  if (err instanceof StatusError) {
    return new ConnectError(err.desc, err.code as unknown as ConnectCode, meta, undefined, err)
  }
  const ce = ConnectError.from(err)
  // Preserve any metadata we gathered (meta already holds only valid entries).
  meta.forEach((v, k) => safeAppend(ce.metadata, k, v))
  return ce
}

function callOptions(signal: AbortSignal | undefined, timeoutMs: number | undefined, header: HeadersInit | undefined): CallOptions {
  const opts: CallOptions = {}
  if (signal !== undefined) opts.signal = signal
  if (timeoutMs !== undefined) opts.timeoutMs = timeoutMs
  const md = headersToMetadata(header)
  if (md !== undefined) opts.metadata = md
  return opts
}

// createDrpcTransport returns a Connect Transport that dispatches calls over
// conn. Pass it to Connect's createClient with any generated service.
export function createDrpcTransport(conn: Conn): Transport {
  return {
    async unary<I extends DescMessage, O extends DescMessage>(
      method: DescMethodUnary<I, O>,
      signal: AbortSignal | undefined,
      timeoutMs: number | undefined,
      header: HeadersInit | undefined,
      input: MessageInitShape<I>,
    ): Promise<UnaryResponse<I, O>> {
      const desc = fromMethod(method)
      const opts = callOptions(signal, timeoutMs, header)
      let respHeader: Metadata | undefined
      let respTrailer: Metadata | undefined
      opts.onHeader = (md) => {
        respHeader = md
      }
      opts.onTrailer = (md) => {
        respTrailer = md
      }
      try {
        const message = await conn.invoke(desc, create(method.input, input), opts)
        return {
          stream: false,
          service: method.parent,
          method,
          header: metadataToHeaders(respHeader),
          message: message as MessageShape<O>,
          trailer: metadataToHeaders(respTrailer),
        }
      } catch (err) {
        throw toConnectError(err, respHeader, respTrailer)
      }
    },

    async stream<I extends DescMessage, O extends DescMessage>(
      method: DescMethodStreaming<I, O>,
      signal: AbortSignal | undefined,
      timeoutMs: number | undefined,
      header: HeadersInit | undefined,
      input: AsyncIterable<MessageInitShape<I>>,
    ): Promise<StreamResponse<I, O>> {
      const desc = fromMethod(method)
      const s = conn.newStream(desc, callOptions(signal, timeoutMs, header)) as ClientStream<MessageShape<I>, MessageShape<O>>

      // Pump the input concurrently: the server may respond before the client
      // is done sending (bidi), and client-streaming must finish sending before
      // its single response arrives. Errors surface to the consumer via recv().
      const pump = (async () => {
        try {
          for await (const raw of input) {
            await s.send(create(method.input, raw))
          }
        } catch {
          s.cancel()
          return
        }
        s.closeSend() // half-close; a no-op for server-streaming (§8)
      })()
      void pump.catch(() => {})

      // Peek the first response frame so the header (if any) has latched before
      // we build the response — Connect reads response.header before iterating
      // (handleStreamResponse / client-streaming), and a no-header drpc stream
      // would otherwise not surface a header until the call ends.
      let first: MessageShape<O> | undefined
      let firstIsEnd = false
      try {
        const r = await s.recv()
        if (r === undefined) firstIsEnd = true
        else first = r
      } catch (err) {
        throw toConnectError(err, s.latchedHeader(), s.trailer())
      }

      const trailer = new Headers()
      const message = (async function* () {
        if (!firstIsEnd) {
          yield first as MessageShape<O>
          for (;;) {
            let m: MessageShape<O> | undefined
            try {
              m = await s.recv()
            } catch (err) {
              throw toConnectError(err, s.latchedHeader(), s.trailer())
            }
            if (m === undefined) break
            yield m
          }
        }
        // The stream ended cleanly: publish the trailer for the post-iteration
        // onTrailer read (Connect reads response.trailer only after the message
        // iterable is exhausted).
        appendMetadata(trailer, s.trailer())
      })()

      return {
        stream: true,
        service: method.parent,
        method,
        header: metadataToHeaders(s.latchedHeader()),
        message,
        trailer,
      }
    },
  }
}
