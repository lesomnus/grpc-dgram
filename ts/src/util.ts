// Concurrency primitives translating the Go core's channels and goroutines
// into single-threaded async TypeScript. Synchronous code paths need no
// locks — an await point is the only place interleaving can happen.
//
// It also holds the two pieces of wire v1.1 both endpoints share, so the
// client and the server cannot drift apart on them: per-stream flow control
// (PROTOCOL.md §4.2.1, Go's flow.go) and message compression with the
// per-call size caps (§12.1, §16 — Go's frame.go/callinfo.go).

import { DropPolicy } from './limits'
import { Code, StatusError } from './status'
import type { Frame } from './wire'

export const noop = (): void => {}

export function nowMs(): number {
  return Date.now()
}

// nonzeroEpoch draws an incarnation nonce (PROTOCOL.md §6.1). Zero is
// excluded: it marks an absent peer_epoch echo.
export function nonzeroEpoch(): number {
  const buf = new Uint32Array(1)
  for (;;) {
    globalThis.crypto.getRandomValues(buf)
    if (buf[0] !== 0) return buf[0]!
  }
}

// unref detaches a Node timer from the event-loop lifetime when supported
// (a live Conn/Server must not pin a Node process the way it would not pin a
// browser tab); a no-op in browsers.
export function unrefTimer(t: unknown): void {
  ;(t as { unref?: () => void }).unref?.()
}

// Latch is a one-shot broadcast: Go's `close(done)`.
export class Latch {
  tripped = false
  private readonly promise: Promise<void>
  private resolve!: () => void

  constructor() {
    this.promise = new Promise((res) => {
      this.resolve = res
    })
  }

  trip(): void {
    if (this.tripped) return
    this.tripped = true
    this.resolve()
  }

  wait(): Promise<void> {
    return this.promise
  }
}

// abortListener registers fn for a signal's abort and returns the disposer;
// long-lived signals see many short waits, so cleanup matters.
export function abortListener(signal: AbortSignal, fn: () => void): () => void {
  signal.addEventListener('abort', fn, { once: true })
  return () => signal.removeEventListener('abort', fn)
}

// FrameQueue is a bounded FIFO standing in for Go's buffered rx channel,
// with the §4.2 delivery modes: non-blocking drop-policy puts for unreliable
// mode and blocking puts for reliable mode.
export class FrameQueue {
  dropped = 0
  private buf: Frame[] = []
  private readWaiters: (() => void)[] = []
  private spaceWaiters: (() => void)[] = []
  // Tail of the FIFO chain that serializes putBlocking callers (see there).
  private putTail: Promise<void> = Promise.resolve()

  constructor(readonly cap: number) {}

  get size(): number {
    return this.buf.length
  }

  tryTake(): Frame | undefined {
    const f = this.buf.shift()
    if (f !== undefined) wake(this.spaceWaiters)
    return f
  }

  tryPut(f: Frame): boolean {
    if (this.buf.length >= this.cap) return false
    this.buf.push(f)
    wake(this.readWaiters)
    return true
  }

  // putDrop delivers f under the configured drop policy (unreliable mode,
  // PROTOCOL.md §4.2): Newest discards the arrival on a full buffer; Oldest
  // evicts the oldest to admit it.
  putDrop(f: Frame, policy: DropPolicy): void {
    if (this.tryPut(f)) return
    if (policy === DropPolicy.Oldest) {
      if (this.buf.shift() !== undefined) this.dropped++
      if (this.tryPut(f)) return
    }
    this.dropped++
  }

  // putBlocking delivers f, blocking until there is room: dropping would
  // violate the reliable-mode exact-sequence contract (PROTOCOL.md §14), so a
  // slow consumer stalls delivery instead and the stall propagates into the
  // adapter's own flow control (§4.2). Bounded by the stream ending (a frame
  // for a finished call is moot) and by the rx signal (adapter teardown); it
  // returns false only for the signal bound — the frame is lost while the
  // call is still live, which on a reliable channel must fail loud.
  //
  // Putters are serialized in call order (the `putTail` chain), so a later
  // putter can never steal a freed slot from an earlier parked one: the
  // buffered channel this stands in for is a true FIFO. A conforming reliable
  // adapter delivers one frame per stream at a time (§4.2), so the chain is
  // uncontended — a single already-resolved await, effectively free — but the
  // guarantee holds even if an adapter delivers concurrently.
  async putBlocking(f: Frame, done: Latch, signal?: AbortSignal): Promise<boolean> {
    const prev = this.putTail
    let release = noop
    this.putTail = new Promise<void>((r) => {
      release = r
    })
    try {
      await prev // wait my turn: everything queued before me finishes first
      for (;;) {
        // A ready buffer always wins: a dead rx signal must not race delivery
        // (an adapter flushing its queue after transport death still delivers
        // every frame that fits).
        if (this.tryPut(f)) return true
        if (done.tripped) return true
        if (signal?.aborted) return false
        let dispose = noop
        const waits: Promise<unknown>[] = [this.space(), done.wait()]
        if (signal !== undefined) {
          waits.push(
            new Promise<void>((res) => {
              dispose = abortListener(signal, res)
            }),
          )
        }
        try {
          await Promise.race(waits)
        } finally {
          dispose()
        }
      }
    } finally {
      release()
    }
  }

  // readable resolves when the queue may have an element; callers re-check
  // with tryTake and loop.
  readable(): Promise<void> {
    if (this.buf.length > 0) return Promise.resolve()
    return new Promise((res) => this.readWaiters.push(res))
  }

  private space(): Promise<void> {
    return new Promise((res) => this.spaceWaiters.push(res))
  }
}

function wake(waiters: (() => void)[]): void {
  if (waiters.length === 0) return
  const ws = waiters.splice(0)
  for (const w of ws) w()
}

// Sweeper drives periodic work while there is any; it stops itself when idle
// and is kicked back to life by state mutations (PROTOCOL.md Appendix C).
export class Sweeper {
  private timer: ReturnType<typeof setInterval> | undefined
  private stopped = false

  kick(intervalMs: number, sweep: () => void, hasWork: () => boolean): void {
    if (this.stopped || this.timer !== undefined) return
    const t = setInterval(() => {
      sweep()
      if (this.timer === t && !hasWork()) {
        clearInterval(t)
        this.timer = undefined
      }
    }, intervalMs)
    unrefTimer(t)
    this.timer = t
  }

  // stop terminates the loop at once and prevents future kicks; Conn.close /
  // Server.stop use it instead of waiting for the last tombstone to expire.
  stop(): void {
    this.stopped = true
    if (this.timer !== undefined) {
      clearInterval(this.timer)
      this.timer = undefined
    }
  }
}

// ---------------------------------------------------------------------------
// per-stream flow control (PROTOCOL.md §4.2.1) — reliable mode only
// ---------------------------------------------------------------------------
//
// HTTP/2's per-stream windows, counted in messages. Without it the only
// back-pressure a receiver has is to stall its read loop, and a reliable
// adapter delivers every call's frames from ONE loop (§4.2), so one slow
// consumer would stall every call on the channel. In a browser that is worse
// than head-of-line blocking: the event loop the stalled delivery runs on is
// the same one that would have to produce the grant, so a blocking receive
// path is a deadlock, never a delay.

// W_INIT is the initial per-stream window a sender assumes before the peer's
// advertisement arrives — the same value as the default rx buffer, so the
// assumption is exact for a default receiver. It is also the reliable-mode rx
// buffer floor: a receiver that buffered less could be overrun before its own
// advertisement landed.
export const W_INIT = 32

// DEFAULT_STALL_MS is T_stall (§10.1): how long a send may park for credit
// before the call fails UNAVAILABLE. Unlike the other timers it runs in
// reliable mode too — that is the mode flow control exists in, and a park
// there has no other bound (no protocol timers, and it happens before the
// adapter's write path).
export const DEFAULT_STALL_MS = 30_000

// reliableRxSize raises a configured rx buffer to the flow-control floor in
// reliable mode (§4.2.1). Unreliable mode is untouched: there a full buffer
// drops by policy and no window is ever advertised.
export function reliableRxSize(size: number, reliable: boolean): number {
  return reliable && size < W_INIT ? W_INIT : size
}

// FlowAcquire says why a parked sender stopped waiting.
export type FlowAcquire =
  // Credit was taken; the message may go on the wire.
  | 'ok'
  // The call ended underneath the sender (end-of-stream, not an error).
  | 'ended'
  // The caller's signal aborted.
  | 'aborted'
  // T_stall elapsed with no grant: the call fails UNAVAILABLE.
  | 'stalled'

// FlowSender is the sending half: how much the peer has allowed, how much has
// been sent, and a parking spot for the difference.
export class FlowSender {
  private on = false
  private observed = false
  private granted = 0
  private sent = 0
  private readonly waiters: (() => void)[] = []

  // assume starts flow control on the protocol's initial window, before the
  // peer has said anything (§4.2.1). Without it a client-streaming burst
  // could empty itself onto the wire before the ack it would be paced by.
  assume(window: number): void {
    if (window <= 0 || this.observed || this.on) return
    this.on = true
    this.granted = window
  }

  // observe adopts the peer's advertised window: authoritative, replacing any
  // assumption and counted against what was already sent (a smaller window
  // simply parks the sender until the receiver drains). 0 means the peer does
  // no flow control.
  observe(window: number): void {
    if (this.observed) return
    this.observed = true
    if (window <= 0) {
      this.on = false
    } else {
      this.on = true
      this.granted = window
    }
    wake(this.waiters)
  }

  // grant adds credit and wakes anyone parked. A grant never turns flow
  // control ON by itself: only an advertisement does (assume/observe).
  // Otherwise a stray, duplicated or injected WINDOW frame could park a
  // sender that was never flow-controlled — free of charge on a datagram
  // channel (§4.2.1, §15).
  grant(n: number): void {
    if (n <= 0 || !this.on) return
    // Saturating: a hostile or buggy peer's grants must not wrap the
    // accumulator into negative credit.
    this.granted = Math.min(this.granted + n, Number.MAX_SAFE_INTEGER)
    wake(this.waiters)
  }

  // undo returns one message of credit: the frame it was taken for never
  // reached the wire (a synchronous adapter refusal, §4.4). Without it a
  // caller that ignores such errors leaks its whole window and parks forever.
  undo(): void {
    if (this.sent > 0) this.sent--
    wake(this.waiters)
  }

  // release wakes every parked sender; the call is over.
  release(): void {
    this.on = false
    wake(this.waiters)
  }

  // tryAcquire consumes one message of credit without parking; false means
  // the sender must park (acquire). It exists so the whole send path stays
  // synchronous when there IS credit: an await between here and the frame's
  // seq allocation would let a racing abort take the number first.
  tryAcquire(): boolean {
    if (this.on && this.sent >= this.granted) return false
    this.sent++
    return true
  }

  // acquire consumes one message of credit, parking until there is some. The
  // park is bounded by the call ending, by the caller's signal, and by
  // T_stall — the last one is load-bearing in reliable mode, where nothing
  // else would ever break it (§4.2.1).
  async acquire(done: Latch, stallMs: number, signal?: AbortSignal): Promise<FlowAcquire> {
    let timer: ReturnType<typeof setTimeout> | undefined
    let dispose = noop
    let expired = false
    let bounds: Promise<unknown>[] | undefined
    try {
      for (;;) {
        if (this.tryAcquire()) return 'ok'
        if (done.tripped) return 'ended'
        if (signal?.aborted) return 'aborted'
        if (expired) return 'stalled'
        if (bounds === undefined) {
          // Armed once, at the first park: T_stall measures the whole wait,
          // not the interval between two partial grants.
          bounds = [done.wait()]
          if (stallMs > 0) {
            bounds.push(
              new Promise<void>((res) => {
                const t = setTimeout(() => {
                  expired = true
                  res()
                }, stallMs)
                unrefTimer(t)
                timer = t
              }),
            )
          }
          if (signal !== undefined) {
            bounds.push(
              new Promise<void>((res) => {
                dispose = abortListener(signal, res)
              }),
            )
          }
        }
        await Promise.race([this.parked(), ...bounds])
      }
    } finally {
      if (timer !== undefined) clearTimeout(timer)
      dispose()
    }
  }

  private parked(): Promise<void> {
    return new Promise((res) => this.waiters.push(res))
  }
}

// FlowReceiver is the receiving half: it counts messages the application has
// consumed and says when to send a grant. Grants are batched at half the
// window, as HTTP/2 stacks do, so a steady stream costs one small frame per
// window/2 messages.
export class FlowReceiver {
  private on = false
  private window = 0
  private pending = 0

  enable(window: number): void {
    this.on = window > 0
    this.window = window
  }

  // active reports whether this side grants credit, i.e. whether the peer is
  // expected to respect a window — and therefore whether a full buffer is the
  // peer's contract violation rather than this side's slowness.
  get active(): boolean {
    return this.on
  }

  // consumed reports that n messages left the buffer and returns the credit
  // to grant now (0 = nothing to send yet).
  consumed(n: number): number {
    if (!this.on || n <= 0) return 0
    this.pending += n
    if (this.pending * 2 < this.window) return 0
    const grant = this.pending
    this.pending = 0
    return grant
  }
}

// ---------------------------------------------------------------------------
// per-call size caps (PROTOCOL.md §16, grpc-go parity)
// ---------------------------------------------------------------------------

// gRPC's own defaults (grpc-go rpc_util.go): 4 MiB received per message,
// effectively unlimited sent.
export const DEFAULT_MAX_RECV_MSG_SIZE = 4 * 1024 * 1024
export const DEFAULT_MAX_SEND_MSG_SIZE = 0x7fff_ffff

// sizeOr resolves an optional size limit: an explicitly configured value —
// including 0, which grpc-go reads as "reject everything" — wins over the
// default. Truthiness would turn a deliberate lockdown into an open door.
export function sizeOr(v: number | undefined, def: number): number {
  return v === undefined ? def : v
}

// checkSendSize enforces maxCallSendMsgSize with grpc-go's status and wording,
// measured on the bytes that go on the wire (i.e. after compression, §12.1).
export function checkSendSize(n: number, limit: number): void {
  if (n > limit) {
    throw new StatusError(Code.RESOURCE_EXHAUSTED, `drpc: trying to send message larger than max (${n} vs. ${limit})`)
  }
}

// checkRecvSize is the receive twin, measured on the DECOMPRESSED message.
export function checkRecvSize(n: number, limit: number): void {
  if (n > limit) {
    throw new StatusError(Code.RESOURCE_EXHAUSTED, `drpc: received message larger than max (${n} vs. ${limit})`)
  }
}

// ---------------------------------------------------------------------------
// message compression (PROTOCOL.md §12.1)
// ---------------------------------------------------------------------------
//
// Named on the OPEN, governing the whole call in both directions like the
// codec. Go plugs into grpc-go's encoding.Compressor registry; the browser has
// no registry, so the platform's CompressionStream/DecompressionStream are the
// implementation and the table below is the registry. A call that names
// something this runtime cannot provide fails loudly at creation — sending raw
// bytes under a compressor name the peer honors would corrupt every message.

// Compressor is one named message compressor. It is per message and
// stateless: a shared stream dictionary is forbidden, since in unreliable
// mode one lost message would make every later one undecodable (§12.1).
//
// Both halves may be async so the platform's CompressionStream (which has no
// synchronous form) plugs in unchanged next to a synchronous node:zlib. This
// is the same shape ServerOptions.compressors takes, so one registry entry
// serves both endpoints.
export interface Compressor {
  compress(data: Uint8Array): Uint8Array | Promise<Uint8Array>
  // decompress expands data, bounded by maxBytes: an implementation SHOULD
  // stop reading past it (that is what makes a decompression bomb cost
  // nothing) and MAY return up to maxBytes bytes. The core fails the call
  // with RESOURCE_EXHAUSTED whenever the result exceeds the call's receive
  // cap, so a truncated buffer can never be mistaken for a valid message.
  decompress(data: Uint8Array, maxBytes: number): Uint8Array | Promise<Uint8Array>
}

// WirePayload is one message as it reaches a frame: the bytes plus whether
// FlagCompressed must ride with them.
export interface WirePayload {
  bytes: Uint8Array
  compressed: boolean
}

export const rawPayload = (bytes: Uint8Array): WirePayload => ({ bytes, compressed: false })

// The compressor names this runtime can serve. "gzip" is the interop baseline
// (§12.1); "deflate" comes free with the same platform API. A name outside the
// table is unknown here — the client refuses the call, the server answers
// T{UNIMPLEMENTED}.
const FORMATS: Record<string, CompressionFormat> = { gzip: 'gzip', deflate: 'deflate' }

const compressorCache = new Map<string, Compressor | undefined>()

// getCompressor resolves a compressor name against the platform's streams, or
// undefined when this runtime cannot provide it ('' — no compression — is
// also undefined). Register the result as a Server compressor to make a
// TS server speak the same baseline:
//
//   new Server(tx, { compressors: { gzip: getCompressor('gzip')! } })
export function getCompressor(name: string): Compressor | undefined {
  if (name === '') return undefined
  const hit = compressorCache.get(name)
  if (hit !== undefined || compressorCache.has(name)) return hit
  const c = makeCompressor(name)
  compressorCache.set(name, c)
  return c
}

function makeCompressor(name: string): Compressor | undefined {
  const format = FORMATS[name]
  if (format === undefined) return undefined
  if (typeof CompressionStream === 'undefined' || typeof DecompressionStream === 'undefined') return undefined
  try {
    // Probe once: a runtime may ship the constructor without every format.
    new CompressionStream(format)
    new DecompressionStream(format)
  } catch {
    return undefined
  }
  return new StreamCompressor(format)
}

class StreamCompressor implements Compressor {
  constructor(private readonly format: CompressionFormat) {}

  async compress(data: Uint8Array): Promise<Uint8Array> {
    try {
      return await runTransform(new CompressionStream(this.format), data, Number.MAX_SAFE_INTEGER)
    } catch (err) {
      if (err instanceof StatusError) throw err
      throw new StatusError(Code.INTERNAL, `drpc: compressor: ${errMsg(err)}`)
    }
  }

  async decompress(data: Uint8Array, maxRecv: number): Promise<Uint8Array> {
    // The expansion is bounded exactly as grpc-go bounds it: one byte past
    // the cap fails with ResourceExhausted, so a bomb cannot allocate without
    // limit. A cap of 0 or less has no meaningful bound left, so the default
    // stands in (Go does the same).
    const limit = maxRecv > 0 ? maxRecv : DEFAULT_MAX_RECV_MSG_SIZE
    try {
      return await runTransform(new DecompressionStream(this.format), data, limit)
    } catch (err) {
      if (err instanceof StatusError) throw err
      throw new StatusError(Code.INTERNAL, `drpc: decompress: ${errMsg(err)}`)
    }
  }
}

// runTransform feeds data through one (de)compression transform and collects
// the output, refusing to accumulate more than limit bytes.
async function runTransform(ts: GenericTransformStream, data: Uint8Array, limit: number): Promise<Uint8Array> {
  const writer = (ts.writable as WritableStream<Uint8Array>).getWriter()
  const reader = (ts.readable as ReadableStream<Uint8Array>).getReader()
  // Write and read concurrently: the transform's internal queue is small, so
  // a large write only completes while the reader drains. The write's failure
  // is captured rather than left dangling — an unhandled rejection in a
  // browser is a console error the application cannot catch.
  let writeErr: unknown
  const written = (async () => {
    await writer.write(data)
    await writer.close()
  })().catch((err: unknown) => {
    writeErr = err
  })

  const chunks: Uint8Array[] = []
  let total = 0
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      if (value === undefined) continue
      total += value.length
      if (total > limit) {
        throw new StatusError(Code.RESOURCE_EXHAUSTED, `drpc: received message after decompression larger than max (> ${limit})`)
      }
      chunks.push(value)
    }
  } catch (err) {
    // Cancelling the readable errors the writable, which settles the pending
    // write; it is never awaited here — a runtime that failed to propagate
    // would hang the call instead of failing it.
    reader.cancel().catch(noop)
    throw err
  }
  await written
  if (writeErr !== undefined) throw writeErr

  if (chunks.length === 1) return chunks[0]!
  const out = new Uint8Array(total)
  let at = 0
  for (const c of chunks) {
    out.set(c, at)
    at += c.length
  }
  return out
}

// compressPayload prepares one message for the wire. An empty payload is never
// compressed — a 0-byte message is meaningful (§5, §7) and gains nothing from
// a codec header — and compression that would EXPAND the payload is skipped:
// it would push the message past the channel's ceiling for nothing (§4.4).
// The per-frame flag makes either decision invisible to the receiver.
export async function compressPayload(comp: Compressor, payload: Uint8Array): Promise<WirePayload> {
  if (payload.length === 0) return rawPayload(payload)
  const out = await comp.compress(payload)
  if (out.length >= payload.length) return rawPayload(payload)
  return { bytes: out, compressed: true }
}

// decompressPayload is the receive twin: the message bytes a COMPRESSED frame
// carries, expanded under the receive cap. A frame marked COMPRESSED on a call
// with no compressor is unreadable — fail rather than hand the codec garbage.
export async function decompressPayload(comp: Compressor | undefined, payload: Uint8Array, maxRecv: number): Promise<Uint8Array> {
  if (comp === undefined) {
    throw new StatusError(Code.INTERNAL, 'drpc: frame is compressed but the call has no compressor')
  }
  return comp.decompress(payload, maxRecv)
}

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}
