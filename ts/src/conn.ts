// The client endpoint: Conn and its streams, with the client half of the
// unreliable-mode machinery — control-frame retransmission, peer liveness,
// stream probes, and client tombstones (PROTOCOL.md §8–§10).
//
// Concurrency note: the Go original guards state with mutexes; here every
// state transition runs synchronously between await points, which gives the
// same atomicity for free. The places Go re-checks under a lock after a
// blocking region are marked where the translation still needs them.

import type { CallOptions, ForcedCodec, MethodDesc, PayloadCodec } from './desc'
import { chain1, chain2, type ClientCall, type StreamClientInterceptor, type UnaryClientInterceptor, type UnaryInvoker } from './interceptor'
import { DropPolicy, resolveLimits, resolveRxConfig, type Limits, type ResolvedLimits, type ResolvedRxConfig, type RxBufferConfig } from './limits'
import { validateMetadata, type Metadata } from './metadata'
import { RxVerdict, RxWindow, TxSeq } from './seq'
import { emit, statsSink, type ProtocolEventKind, type ProtocolStats } from './stats'
import { abortCause, Code, isMessageTooLarge, StatusError, statusError, toStatusError } from './status'
import { resolveTiming, type Mode, type Timing } from './timing'
import { hasConnAttacher, hasTransportInfo, type FrameContext, type FrameHandler } from './seam'
import {
  abortListener,
  checkRecvSize,
  checkSendSize,
  compressPayload,
  decompressPayload,
  DEFAULT_MAX_RECV_MSG_SIZE,
  DEFAULT_MAX_SEND_MSG_SIZE,
  DEFAULT_STALL_MS,
  FlowReceiver,
  FlowSender,
  FrameQueue,
  getCompressor,
  Latch,
  nonzeroEpoch,
  noop,
  nowMs,
  rawPayload,
  reliableRxSize,
  sizeOr,
  Sweeper,
  unrefTimer,
  W_INIT,
  type Compressor,
} from './util'
import {
  FlagClose,
  FlagCompressed,
  FlagOpen,
  FlagPing,
  FlagWindow,
  frame,
  frameStatus,
  hasUnknownFlags,
  isCompressed,
  isData,
  isHeaderFrame,
  isPing,
  isReset,
  isTerminal,
  legalShape,
  resetFor,
  shapeOf,
  type Any,
  type Frame,
} from './wire'

// EndOfStreamError is thrown by send when the call already ended (a racing
// abort, terminal, or teardown) — the io.EOF of the grpc-go contract. The
// call's actual status surfaces via recv.
export class EndOfStreamError extends Error {
  constructor() {
    super('stream ended')
    this.name = 'EndOfStreamError'
  }
}

// FlowTiming adds T_stall to the §10.1 protocol timers. It is declared here
// rather than in timing.ts because T_stall is the one timer that also runs in
// RELIABLE mode: it bounds a sender parked on flow-control credit, which no
// other timer could break (§4.2.1). Fold it into Timing when timing.ts is next
// touched.
export interface FlowTiming extends Timing {
  // T_stall: how long a send may wait for flow-control credit before the call
  // fails UNAVAILABLE. Default 30 s.
  stallMs?: number
}

// CallConfig is CallOptions plus the wire v1.1 per-call knobs. They live here
// for the same reason as FlowTiming — desc.ts is shared and unchanged — and
// mirror grpc-go's UseCompressor / MaxCallRecvMsgSize / MaxCallSendMsgSize
// call options (§12.1, §16).
export interface CallConfig<Req = unknown, Res = unknown> extends CallOptions<Req, Res> {
  // Message compressor for the whole call, both directions; '' = none. The
  // name rides the OPEN. A name this runtime cannot provide fails the call
  // with INTERNAL before it starts — never a silent raw send (§12.1).
  compressor?: string
  // Caps one received message AFTER decompression (default 4 MiB) and one
  // sent message AFTER compression (default effectively unlimited), failing
  // with RESOURCE_EXHAUSTED exactly as gRPC does. 0 rejects everything.
  maxRecvMsgSize?: number
  maxSendMsgSize?: number
}

export interface ConnOptions {
  // Overrides transport discovery (PROTOCOL.md §4.3).
  reliable?: boolean
  // Protocol timers (unreliable mode only, §10.1 — except stallMs, §4.2.1).
  timing?: FlowTiming
  // Per-stream rx buffer size and drop policy (§4.2). In reliable mode the
  // size is also the advertised flow-control window, floored at W_init.
  rxBuffer?: RxBufferConfig
  // Resource caps (§15); only maxPendingResets applies to a Conn.
  limits?: Limits
  // Endpoint-wide call defaults; per-call options override them.
  compressor?: string
  maxRecvMsgSize?: number
  maxSendMsgSize?: number
  // Message compressors this Conn can use, by name (§12.1) — the client twin
  // of ServerOptions.compressors, and the way to plug in node:zlib or a
  // custom codec. A name absent here falls back to the platform's
  // CompressionStream ('gzip', 'deflate'); a name neither can serve fails the
  // call with INTERNAL before it starts.
  compressors?: Record<string, Compressor>
  defaultCallOptions?: CallConfig
  // Interceptor chains (interceptor.ts). Element 0 runs outermost; the last
  // element is handed the Conn's own invoker/streamer — grpc-go's order
  // (WithChainUnaryInterceptor / WithChainStreamInterceptor), the reverse of
  // Connect-ES. Folded once here, not per call.
  unaryInterceptors?: UnaryClientInterceptor[]
  streamInterceptors?: StreamClientInterceptor[]
  // Observers of the protocol events gRPC has no concept of — skipped
  // messages, rx drops, RESETs, retransmissions, probes, liveness expiry,
  // tombstone replays, flow-control stalls (PROTOCOL.md §14; stats.ts). One
  // or several; each is called synchronously on the receive path and the
  // sweep and must not block. A throw is contained and costs that observer
  // the event, never the endpoint the step it was reporting.
  protocolStats?: ProtocolStats | ProtocolStats[]
}

// CallInfo is the resolved per-call configuration (Go's callInfo).
interface CallInfo {
  compressorName: string
  compressor: Compressor | undefined
  maxRecv: number
  maxSend: number
  stallMs: number
}

// DetailedStatusError is the status a terminal frame carries when it also
// carried google.rpc.Status.details (§5). StatusError itself models code and
// description only — status.ts owns the error model and stays untouched — so
// the details ride on the instance and are read back with statusDetails().
export type DetailedStatusError = StatusError & { details?: readonly Any[] }

// statusDetails returns the rich details a failed call's status carried, if
// any: the google.protobuf.Any values of google.rpc.Status.details, exactly as
// the server sent them (unmarshaling them is the application's business — the
// core carries no protobuf runtime).
export function statusDetails(err: unknown): readonly Any[] | undefined {
  return err instanceof StatusError ? (err as DetailedStatusError).details : undefined
}

// EMPTY stands in for an absent payload: a frame without one decodes as the
// zero-length message, never as garbage.
const EMPTY = new Uint8Array(0)

// terminalStatus reads a terminal frame's status, details included.
function terminalStatus(f: Frame): StatusError {
  const st = frameStatus(f) as DetailedStatusError
  if (f.details !== undefined && f.details.length > 0) st.details = f.details
  return st
}

// clientTomb remembers a finished call for TTL_tomb: stragglers for it are
// dropped, and a pending abort keeps retransmitting under its obligation
// until a matching T, a RESET, or expiry (PROTOCOL.md §9.2, §10.3).
interface ClientTomb {
  expireAt: number
  abort: Frame | undefined
  retxAt: number
  ivalMs: number
}

export class Conn {
  // This Conn incarnation's nonce (PROTOCOL.md §6.1).
  readonly epoch: number

  private readonly tx: FrameHandler
  private readonly mode: Mode
  private readonly rxCfg: ResolvedRxConfig
  private readonly limits: ResolvedLimits
  private readonly defaults: CallConfig
  // Endpoint-wide call defaults (§12.1, §16) and T_stall (§4.2.1).
  private readonly compressor: string
  private readonly compressors: Map<string, Compressor>
  private readonly maxRecv: number
  private readonly maxSend: number
  private readonly stallMs: number
  /** @internal */ readonly pstats: readonly ProtocolStats[]

  private readonly ss = new Map<number, ClientStream<unknown, unknown>>()
  private readonly tombs = new Map<number, ClientTomb>()
  private readonly resetAt = new Map<number, number>()
  private sidNext = 0
  private exhausted = false
  private closed = false

  // Peer-liveness clocks (unreliable mode, PROTOCOL.md §10.4).
  private lastRx = 0
  private lastTx = 0
  private lastPing = 0
  private readonly sw = new Sweeper()

  // The folded interceptor chains; undefined = none, and the call goes
  // straight to the invoker (Go's pass-through, conn.go NewConn).
  private readonly unaryInt: UnaryClientInterceptor | undefined
  private readonly streamInt: StreamClientInterceptor | undefined

  constructor(tx: FrameHandler, opts: ConnOptions = {}) {
    this.epoch = nonzeroEpoch()
    this.tx = tx
    this.mode = {
      reliable: opts.reliable ?? (hasTransportInfo(tx) ? tx.reliable() : false),
      timing: resolveTiming(opts.timing),
    }
    // In reliable mode the rx buffer is also the advertised window, floored
    // at W_init so the window a peer assumes before our OPEN lands is always
    // safe (PROTOCOL.md §4.2.1).
    const rx = resolveRxConfig(opts.rxBuffer)
    this.rxCfg = { size: reliableRxSize(rx.size, this.mode.reliable), policy: rx.policy }
    this.limits = resolveLimits(opts.limits)
    this.defaults = opts.defaultCallOptions ?? {}
    this.compressor = opts.compressor ?? ''
    this.compressors = new Map(Object.entries(opts.compressors ?? {}))
    this.maxRecv = sizeOr(opts.maxRecvMsgSize, DEFAULT_MAX_RECV_MSG_SIZE)
    this.maxSend = sizeOr(opts.maxSendMsgSize, DEFAULT_MAX_SEND_MSG_SIZE)
    this.stallMs = opts.timing?.stallMs ?? DEFAULT_STALL_MS
    this.pstats = statsSink(opts.protocolStats)
    this.unaryInt = chain2(opts.unaryInterceptors)
    this.streamInt = chain1(opts.streamInterceptors)

    // Last, with the Conn fully usable: the transport may start delivering
    // frames from inside attachConn.
    if (hasConnAttacher(tx)) tx.attachConn(this)
  }

  get reliable(): boolean {
    return this.mode.reliable
  }

  // protoEvent reports one peer-scope protocol event (stats.ts). A Conn is one
  // channel to one peer, so none is named; sid is set where the event concerns
  // a call this Conn no longer holds (a RESET for a tombstone).
  private protoEvent(kind: ProtocolEventKind, sid = 0): void {
    if (this.pstats.length === 0) return
    emit(this.pstats, { kind, sid, method: '', count: 0 })
  }

  // handle delivers one server frame to this Conn. Adapters call it for each
  // frame of a received envelop, in order, awaiting each (PROTOCOL.md §9.1).
  async handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    const sid = f.sid

    if (isReset(f)) {
      // Act only if the echoed epoch is ours; RESET never refreshes
      // liveness (PROTOCOL.md §9.1, §9.3).
      if (f.epoch !== this.epoch) return
      this.protoEvent('reset-received', sid)
      const s = this.ss.get(sid)
      if (s !== undefined) {
        s.finishReset()
        return
      }
      // Obligation-clear at tombstones (PROTOCOL.md §10.3).
      this.clearTombAbort(sid)
      return
    }
    // Every other server frame echoes the client incarnation it addresses
    // (PROTOCOL.md §6.1). One that names another — a dead incarnation
    // coexisting behind this address, or an injection — must not touch this
    // Conn's calls or clocks: sids restart at 1 across restarts, so a sid
    // match means nothing without the epoch echo.
    if (f.peerEpoch !== this.epoch) {
      if (isPing(f) && sid === 0) return // another incarnation's keepalive: not ours to answer
      // Tell the desynced server to stop (§9.3): the RESET echoes the
      // offending frame's peer_epoch, so exactly that incarnation's call
      // dies at the server.
      await this.sendReset(f)
      return
    }

    if (shapeOf(f) === FlagWindow) {
      // A flow-control grant is advisory and stateless: for a live call it
      // credits the sender, for anything else it is dropped in silence — a
      // grant legitimately races the call's end, and answering it with a
      // RESET would turn every well-behaved stream into a RESET exchange
      // (§4.2.1, §9.3). It refreshes no clock either: it is not a validated
      // frame of any stream (§9.1).
      const s = this.ss.get(sid)
      if (s !== undefined) await s.handleRx(f, ctx)
      return
    }

    if (isPing(f)) {
      // Well-formed PINGs are validated: refresh peer liveness
      // (PROTOCOL.md §9.1, §10.4).
      this.lastRx = nowMs()
      if (sid === 0) return
      // Stream probe (§10.5): live stream → no-op; tombstoned or unknown →
      // RESET so the prober fails fast.
      if (this.ss.has(sid)) return
      await this.sendReset(f)
      return
    }

    const s = this.ss.get(sid)
    if (s !== undefined) {
      await s.handleRx(f, ctx)
      return
    }

    const tomb = this.tombs.get(sid)
    if (tomb !== undefined) {
      // Straggler for a finished call: validated, dropped. A matching
      // terminal clears the pending abort (PROTOCOL.md §9.1-5b, §10.3).
      this.lastRx = nowMs()
      if (isTerminal(f)) this.clearTombAbort(sid)
      return
    }

    // Unknown sid: tell the desynced server to stop — no OPEN can ever
    // arrive at a client (PROTOCOL.md §9.3).
    await this.sendReset(f)
  }

  // close fails every live call with UNAVAILABLE. Adapters call it when the
  // transport dies (PROTOCOL.md §4.5). It also calls a close() the tx
  // exposes, so closing the Conn tears the whole endpoint down. Idempotent
  // (a self-closing tx must be too: its death path calls back in here).
  close(err?: unknown): void {
    const st =
      err === undefined || err === null
        ? statusError(Code.UNAVAILABLE, 'transport closed')
        : statusError(Code.UNAVAILABLE, `transport closed: ${err instanceof Error ? err.message : String(err)}`)
    // Latch before failing: a stream inserted before the latch is caught by
    // failAll's snapshot, one attempted after it is refused by createStream.
    this.closed = true
    this.failAll(st)
    this.sw.stop()
    const cl = (this.tx as { close?: () => void }).close
    if (typeof cl === 'function') cl.call(this.tx)
  }

  // invoke performs a unary call (grpc-go Invoke parity). The endpoint
  // defaults and T_call are folded in before the interceptor chain runs, as
  // Go sets the ctx deadline before its chain: an interceptor sees the
  // effective timeoutMs and may still replace it.
  async invoke<Req, Res>(desc: MethodDesc<Req, Res>, req: Req, opts: CallConfig<Req, Res> = {}): Promise<Res> {
    const merged: CallConfig<Req, Res> = { ...(this.defaults as CallConfig<Req, Res>), ...opts }
    let timeoutCause: StatusError | undefined
    if (!this.mode.reliable && merged.timeoutMs === undefined) {
      // T_call: the default unary deadline (PROTOCOL.md §10.2).
      merged.timeoutMs = this.mode.timing.callMs
      timeoutCause = statusError(Code.DEADLINE_EXCEEDED, 'drpc: default call timeout')
    }
    const call: ClientCall<Req, Res> = { desc, opts: merged }
    // The budget is absolute across the chain, as a ctx deadline is in Go: a
    // retrying interceptor's later attempts get the remainder, not a fresh
    // budget — unless the chain replaced timeoutMs, which starts a new one
    // from that point (a new ctx deadline). The default's cause survives
    // only with the budget it named. A remainder that has run out fails the
    // attempt at arm time, before anything reaches the wire.
    const budget = merged.timeoutMs
    const deadlineAt = budget === undefined ? undefined : nowMs() + budget
    const last: UnaryInvoker = (r, c) => {
      if (deadlineAt === undefined || c.opts.timeoutMs !== budget) return this.doInvoke(r, c, undefined)
      return this.doInvoke(r, { desc: c.desc, opts: { ...c.opts, timeoutMs: deadlineAt - nowMs() } }, timeoutCause)
    }
    const out: unknown = this.unaryInt === undefined ? await last(req, call) : await this.unaryInt(req, call, last)
    // The types demand a response; a JS caller can still resolve to nothing.
    if (out === undefined) throw statusError(Code.INTERNAL, 'drpc: the interceptor chain resolved to no response')
    return out as Res
  }

  // doInvoke is the innermost unary invoker: the stream is created here, after
  // the chain, so the OPEN carries the interceptor-final options (§8, §11).
  private async doInvoke(req: unknown, call: ClientCall, timeoutCause: StatusError | undefined): Promise<NonNullable<unknown>> {
    const { desc, opts: merged } = call
    const s = this.createStream(desc, merged, timeoutCause)
    let err: StatusError | undefined
    let out: unknown
    try {
      try {
        await s.sendRaw(req)
      } catch (e) {
        // EndOfStream means the call already ended (a racing abort or
        // teardown); the terminal outcome surfaces via recv below.
        if (!(e instanceof EndOfStreamError)) err = toStatusError(e)
      }
      if (err === undefined) {
        try {
          out = await s.recv()
          if (out === undefined) {
            // A unary terminal without a payload is a protocol anomaly.
            err = statusError(Code.INTERNAL, 'unary call ended without a response')
          }
        } catch (e) {
          err = toStatusError(e)
        }
      }
    } finally {
      s.abandon()
    }
    // Header/trailer are populated on finish regardless of the status
    // (grpc-go call-option parity); abandon has tripped both latches.
    if (merged.onHeader !== undefined) merged.onHeader(await s.header())
    if (merged.onTrailer !== undefined) merged.onTrailer(s.trailer())
    if (err !== undefined) throw err
    return out as NonNullable<unknown>
  }

  // newStream starts a streaming call. The stream is created by the innermost
  // streamer, after the interceptor chain, so the OPEN sees the chain's final
  // options (PROTOCOL.md §8; conn.go NewStream).
  newStream<Req, Res>(desc: MethodDesc<Req, Res>, opts: CallConfig<Req, Res> = {}): ClientStream<Req, Res> {
    const merged: CallConfig<Req, Res> = { ...(this.defaults as CallConfig<Req, Res>), ...opts }
    const call: ClientCall<Req, Res> = { desc, opts: merged }
    const s = this.streamInt === undefined ? this.doNewStream(call) : this.streamInt(call, (c) => this.doNewStream(c))
    return s as ClientStream<Req, Res>
  }

  // doNewStream is the innermost streamer. For client-streaming and bidi the
  // eager OPEN is sent at stream creation, so the server can start the
  // handler and push even if the client never sends (PROTOCOL.md §8).
  private doNewStream(call: ClientCall): ClientStream<unknown, unknown> {
    const s = this.createStream(call.desc, call.opts, undefined)
    if (call.desc.clientStreams) void s.sendOpen()
    return s
  }

  // resolveCall folds the endpoint defaults and the call options into the
  // per-call configuration, mirroring Go's resolveCallOptions: it runs before
  // the call exists, so a compressor this runtime cannot provide fails as
  // INTERNAL at the API boundary instead of corrupting the wire.
  private resolveCall(opts: CallConfig): CallInfo {
    const name = opts.compressor ?? this.compressor
    let compressor: Compressor | undefined
    if (name !== '') {
      // Registered first, platform second: an application that plugged in its
      // own gzip keeps it, and a runtime without CompressionStream still
      // works. Neither → the call fails here rather than putting raw bytes on
      // the wire under a compressor name the peer will honor (§12.1).
      compressor = this.compressors.get(name) ?? getCompressor(name)
      if (compressor === undefined) {
        throw statusError(Code.INTERNAL, `drpc: no compressor registered for ${JSON.stringify(name)}`)
      }
    }
    return {
      compressorName: name,
      compressor,
      maxRecv: sizeOr(opts.maxRecvMsgSize, this.maxRecv),
      maxSend: sizeOr(opts.maxSendMsgSize, this.maxSend),
      stallMs: this.stallMs,
    }
  }

  private createStream<Req, Res>(desc: MethodDesc<Req, Res>, opts: CallConfig<Req, Res>, timeoutCause: StatusError | undefined): ClientStream<Req, Res> {
    const ci = this.resolveCall(opts)
    // Outgoing metadata is validated before the call exists, as grpc-go does:
    // an illegal key or a non-printable value in a text key must surface as
    // INTERNAL here, not as an opaque failure inside an adapter's encoder —
    // values are bytes on the wire, so nothing downstream would catch it
    // (§11).
    validateMetadata(opts.metadata)
    if (this.closed) {
      // With the pump gone and the sweeper stopped, nothing could ever
      // terminate a call admitted now.
      throw statusError(Code.UNAVAILABLE, 'drpc: the connection is closed')
    }
    if (this.exhausted) {
      throw statusError(Code.RESOURCE_EXHAUSTED, 'sid space exhausted; create a new Conn')
    }
    this.sidNext = (this.sidNext + 1) >>> 0
    if (this.sidNext === 0) {
      // The sid space is never recycled within an epoch (PROTOCOL.md §6.2).
      this.exhausted = true
      throw statusError(Code.RESOURCE_EXHAUSTED, 'sid space exhausted; create a new Conn')
    }

    if (!this.mode.reliable && this.ss.size === 0) {
      // Arm the peer-liveness clocks with the first live call (§10.4).
      const n = nowMs()
      this.lastRx = n
      this.lastTx = n
    }
    const s = new ClientStream<Req, Res>(this, desc, this.sidNext, opts, ci, timeoutCause)
    this.ss.set(s.sid, s as ClientStream<unknown, unknown>)
    // Cancellation sources arm only after registration, so an
    // already-aborted signal cannot retire the stream before it exists in
    // the live map.
    s.arm(opts)
    this.kickSweep()
    return s
  }

  // ------------------------------------------------------------------
  // internals shared with ClientStream
  // ------------------------------------------------------------------

  /** @internal */
  get timing() {
    return this.mode.timing
  }

  /** @internal */
  get isReliable(): boolean {
    return this.mode.reliable
  }

  /** @internal */
  get streamRxCfg(): ResolvedRxConfig {
    return this.rxCfg
  }

  /** @internal */
  transmit(f: Frame): Promise<void> {
    this.lastTx = nowMs()
    return Promise.resolve(this.tx.handle(f))
  }

  /** @internal */
  noteRx(): void {
    this.lastRx = nowMs()
  }

  // retire removes a finished stream from the live map and installs its
  // tombstone; a pending abort keeps retransmitting under it (PROTOCOL.md
  // §9.2).
  /** @internal */
  retire(s: ClientStream<unknown, unknown>): void {
    this.ss.delete(s.sid)
    if (!this.mode.reliable) {
      const now = nowMs()
      let ttl = this.mode.timing.tombstoneMs
      if (s.deadlineAt !== undefined) {
        // TTL floor: the call's propagated timeout remainder (§9.2).
        ttl = Math.max(ttl, s.deadlineAt - now)
      }
      const tb: ClientTomb = { expireAt: now + ttl, abort: s.abortFrame, retxAt: 0, ivalMs: 0 }
      if (tb.abort !== undefined) {
        tb.ivalMs = this.mode.timing.retransmitMs
        tb.retxAt = now + tb.ivalMs
      }
      this.tombs.set(s.sid, tb)
      // The tombstone owns the abort from here; the stream's own obligations
      // are over, and a sweep that snapshotted it moments ago must find
      // nothing left to retransmit under its name (as Go's retire clears).
      s.dropRetx()
    }
    this.kickSweep()
  }

  // clearTombAbort clears a tombstone's pending abort obligation: a matching
  // terminal or a RESET arrived (PROTOCOL.md §10.3).
  private clearTombAbort(sid: number): void {
    const tb = this.tombs.get(sid)
    if (tb !== undefined) {
      tb.abort = undefined
      tb.retxAt = 0
    }
  }

  // sendReset answers a frame for an unknown call, rate-limited per sid
  // (PROTOCOL.md §9.3; clients RESET immediately — no OPEN can arrive here).
  private async sendReset(f: Frame): Promise<void> {
    if (!this.mode.reliable) {
      const sid = f.sid
      const n = nowMs()
      const last = this.resetAt.get(sid)
      if (last !== undefined) {
        if (n - last < this.mode.timing.retransmitMs) return
      } else if (this.resetAt.size >= this.limits.maxPendingResets) {
        // Bounded: drop rather than grow (anti-amplification, §15).
        return
      }
      this.resetAt.set(sid, n)
      this.kickSweep()
    }
    this.protoEvent('reset-sent', f.sid)
    try {
      await this.tx.handle(resetFor(f))
    } catch {
      // A refused RESET is loss; the peer retries (§9.3 is rate-limited
      // best-effort either way).
    }
  }

  // failAll ends every live call with err and drops all retransmission
  // obligations (used by liveness expiry and adapter teardown).
  private failAll(err: StatusError): void {
    const ss = [...this.ss.values()]
    for (const tb of this.tombs.values()) {
      tb.abort = undefined
      tb.retxAt = 0
    }
    for (const s of ss) s.finishLocal(err)
  }

  /** @internal */
  kickSweep(): void {
    if (this.mode.reliable) return
    this.sw.kick(
      this.mode.timing.tickMs,
      () => this.sweep(nowMs()),
      () => this.hasWork(),
    )
  }

  private hasWork(): boolean {
    return this.ss.size > 0 || this.tombs.size > 0 || this.resetAt.size > 0
  }

  private sweep(now: number): void {
    const t = this.mode.timing

    const streams = [...this.ss.values()]
    const tombRetx: Frame[] = []
    for (const [sid, tb] of this.tombs) {
      if (now > tb.expireAt) {
        this.tombs.delete(sid)
        continue
      }
      if (tb.abort !== undefined && tb.retxAt !== 0 && now > tb.retxAt) {
        tombRetx.push(tb.abort)
        tb.ivalMs = Math.min(tb.ivalMs * 2, t.probeMs)
        tb.retxAt = now + tb.ivalMs
      }
    }
    for (const [sid, at] of this.resetAt) {
      if (now - at > t.tombstoneMs) this.resetAt.delete(sid)
    }
    const live = this.ss.size > 0

    // Peer liveness (PROTOCOL.md §10.4): one peer per Conn.
    if (live) {
      if (now - this.lastRx >= t.livenessMs) {
        this.protoEvent('liveness-expired')
        this.failAll(statusError(Code.UNAVAILABLE, 'peer lost'))
        return
      }
      if (now - this.lastTx >= t.probeMs && now - this.lastPing >= t.probeMs) {
        this.lastPing = now
        this.lastTx = now
        this.protoEvent('keepalive-sent')
        void Promise.resolve(this.tx.handle(frame({ epoch: this.epoch, flags: FlagPing }))).catch(noop)
      }
    }

    // Per-stream retransmissions and probes.
    for (const s of streams) {
      for (const f of s.sweepRetx(now, t.probeMs)) {
        s.protoEvent('retransmit')
        void s.transmit(f).catch(noop)
      }
      const p = s.probeDue(now, t.probeMs)
      if (p !== undefined) {
        s.protoEvent('probe-sent')
        // Probes feed the peer-keepalive cadence but not the stream's own
        // idle clocks (PROTOCOL.md §10.5).
        this.lastTx = now
        void Promise.resolve(this.tx.handle(p)).catch(noop)
      }
    }
    // Tombstoned aborts.
    for (const f of tombRetx) {
      this.protoEvent('retransmit', f.sid)
      this.lastTx = now
      void Promise.resolve(this.tx.handle(f)).catch(noop)
    }
  }
}

// ---------------------------------------------------------------------------
// client stream
// ---------------------------------------------------------------------------

export class ClientStream<Req, Res> {
  readonly sid: number

  private readonly conn: Conn
  private readonly desc: MethodDesc<Req, Res>
  private readonly reqCodec: PayloadCodec<Req>
  private readonly resCodec: PayloadCodec<Res>
  private readonly codecName: string
  private readonly openHdr: Metadata | undefined
  private readonly ci: CallInfo

  // Per-stream flow control (reliable mode, §4.2.1): flowTx is credit for what
  // this side sends, flowRx accounts what the application has consumed.
  private readonly flowTx = new FlowSender()
  private readonly flowRx = new FlowReceiver()
  // The advertised receive window == the rx buffer size (floored at W_init).
  private readonly rxSize: number

  // Cancellation sources (the caller's ctx).
  private readonly callerSignal: AbortSignal | undefined
  private signalDispose: () => void = noop
  private deadlineTimer: ReturnType<typeof setTimeout> | undefined
  private deadlineFired = false
  private readonly timeoutCause: StatusError | undefined
  /** @internal */
  deadlineAt: number | undefined

  // tx state.
  private readonly txSeq = new TxSeq()
  private txOpened = false
  private txClosed = false
  /** @internal */
  abortFrame: Frame | undefined

  // Retransmission obligations (unreliable mode, PROTOCOL.md §10.3); frames
  // are stored for byte-identical resends.
  private retxOpen: Frame | undefined
  private retxClose: Frame | undefined
  private retxAt = 0
  private retxIval = 0

  // Idle clocks (unreliable mode, PROTOCOL.md §10.5).
  private lastRx: number
  private lastTx: number
  private lastProbe = 0

  // rx sequencing.
  private readonly rxWin = new RxWindow()
  private srvEpoch = 0 // server incarnation this stream is locked to
  private srvEpochSet = false

  private readonly rxq: FrameQueue
  private readonly rxPolicy: DropPolicy
  private rxDropped = 0

  // header/trailer state.
  private rxHeader: Metadata | undefined
  private trailerMd: Metadata | undefined
  private readonly hdrLatch = new Latch()

  // Terminal state: term/termErr are written exactly once before done trips.
  private readonly done = new Latch()
  private term: Frame | undefined // server terminal frame, if that ended the call
  private termErr: StatusError | undefined // local termination (cancel, RESET, DATA_LOSS)
  private termPayloadDelivered = false

  /** @internal */
  constructor(conn: Conn, desc: MethodDesc<Req, Res>, sid: number, opts: CallConfig<Req, Res>, ci: CallInfo, timeoutCause: StatusError | undefined) {
    this.conn = conn
    this.desc = desc
    this.sid = sid
    this.ci = ci
    const forced: ForcedCodec<Req, Res> | undefined = opts.codec
    this.reqCodec = forced?.request ?? desc.request
    this.resCodec = forced?.response ?? desc.response
    this.codecName = forced?.name ?? ''
    this.openHdr = opts.metadata
    this.callerSignal = opts.signal
    this.timeoutCause = timeoutCause
    if (opts.timeoutMs !== undefined) this.deadlineAt = nowMs() + opts.timeoutMs

    const cfg = conn.streamRxCfg
    this.rxSize = cfg.size
    this.rxq = new FrameQueue(cfg.size)
    this.rxPolicy = cfg.policy
    this.rxWin.strict = conn.isReliable
    if (conn.isReliable) {
      // Advertise this side's buffer as the server's initial send window
      // (§4.2.1); it rides the OPEN. Until the server advertises its own on
      // the creation-ack H, this side paces itself by W_init.
      this.flowRx.enable(cfg.size)
      this.flowTx.assume(W_INIT)
    }

    const n = nowMs()
    this.lastRx = n
    this.lastTx = n
  }

  // arm wires the caller's cancellation sources; called by the Conn after
  // the stream is registered (an already-dead source aborts at once).
  /** @internal */
  arm(opts: CallOptions<Req, Res>): void {
    if (opts.timeoutMs !== undefined) {
      if (opts.timeoutMs <= 0) {
        this.deadlineFired = true
        this.abortFromCtx()
        return
      }
      const t = setTimeout(() => {
        this.deadlineFired = true
        this.abortFromCtx()
      }, opts.timeoutMs)
      unrefTimer(t)
      this.deadlineTimer = t
    }
    if (this.callerSignal !== undefined) {
      if (this.callerSignal.aborted) {
        this.abortFromCtx()
        return
      }
      this.signalDispose = abortListener(this.callerSignal, () => this.abortFromCtx())
    }
  }

  // ------------------------------------------------------------------
  // receive path
  // ------------------------------------------------------------------

  // handleRx processes one server frame for this stream. Called by
  // Conn.handle. In reliable mode it may block on a full buffer, bounded by
  // the rx signal (PROTOCOL.md §4.2).

  // protoEvent reports one protocol event for this call (stats.ts), named by
  // its sid and method. /** @internal */: the Conn's sweep reports the
  // retransmissions and probes it sends on the stream's behalf.
  /** @internal */
  protoEvent(kind: ProtocolEventKind, count = 0): void {
    const sink = this.conn.pstats
    if (sink.length === 0) return
    emit(sink, { kind, sid: this.sid, method: this.desc.path, count })
  }

  /** @internal */
  async handleRx(f: Frame, ctx: FrameContext): Promise<void> {
    if (this.done.tripped) return

    if (hasUnknownFlags(f) || !legalShape(shapeOf(f))) {
      // A modifier bit from a newer peer changes something about this frame
      // that we cannot honor, and an illegal shape combination is not a frame
      // we can route: delivering either would be a silent corruption,
      // dropping it a silent gap (§7.1, §8).
      const err = statusError(Code.INTERNAL, `drpc: frame carries unsupported flags 0x${(f.flags >>> 0).toString(16)}`)
      this.sendAbort(Code.INTERNAL)
      this.finishLocal(err)
      return
    }

    if (this.srvEpochSet && f.epoch !== this.srvEpoch) {
      // A frame from a different server incarnation (stale straggler after a
      // restart, or a raw-UDP injection) must not touch this live call.
      this.rxDropped++
      return
    }

    if (shapeOf(f) === FlagWindow) {
      // Stateless flow-control grant: no seq, no delivery, no clock — and
      // only where flow control exists at all, so a stray or injected WINDOW
      // can never park an unreliable-mode sender (§4.2.1, §7, §15).
      if (this.conn.isReliable) this.flowTx.grant(f.window)
      return
    }

    const v = this.rxWin.check(f.seq)
    if (v === RxVerdict.Accept && !this.srvEpochSet) {
      this.srvEpoch = f.epoch
      this.srvEpochSet = true
    }

    switch (v) {
      case RxVerdict.Dup:
        // Validated: any server frame for the sid clears the OPEN
        // retransmission obligation (PROTOCOL.md §10.3).
        this.noteValidatedRx()
        return
      case RxVerdict.Beyond:
        return
      case RxVerdict.DataLoss: {
        // Window overrun on a live stream: fail loudly (PROTOCOL.md §6.3)
        // and abort so the server stops.
        this.protoEvent('data-loss')
        const err = statusError(Code.DATA_LOSS, 'seq window overrun: >W_fwd consecutive frames lost')
        this.sendAbort(Code.DATA_LOSS)
        this.finishLocal(err)
        return
      }
      case RxVerdict.ProtocolError: {
        // Reliable-mode gap/duplicate: the transport is broken (§10.6).
        const err = statusError(Code.INTERNAL, 'reliable transport lost or reordered a frame')
        this.sendAbort(Code.INTERNAL)
        this.finishLocal(err)
        return
      }
      case RxVerdict.Accept:
        break
    }
    const gap = this.rxWin.takeGap()
    if (gap > 0) {
      // The §14 skipped-message counter: how many messages the gap ate.
      this.protoEvent('skipped', gap)
    }
    this.noteValidatedRx()
    if (this.conn.isReliable) {
      // The first server frame — the creation ack for a streaming call —
      // advertises the server's receive window and replaces the assumed one
      // (§4.2.1). Absent means the peer does no flow control.
      this.flowTx.observe(f.window)
    }

    if (isTerminal(f)) {
      this.latchHeader(f)
      if (f.trailer !== undefined) this.trailerMd = f.trailer
      this.finishTerm(f)
    } else if (isHeaderFrame(f)) {
      this.latchHeader(f)
    } else if (isData(f)) {
      if (!this.desc.serverStreams) {
        // Off-shape: unary/client-streaming has no server data frames.
        this.rxDropped++
        this.protoEvent('off-shape', 1)
        return
      }
      this.latchHeader(f)
      if (this.conn.isReliable) {
        if (this.flowRx.active) {
          // Flow-controlled: a conforming peer never exceeds the window it
          // was granted, so a full buffer is a contract violation. Blocking
          // here is what flow control exists to remove and would deadlock —
          // the grant that would unpark the peer travels the very event loop
          // the block stalls (§4.2.1).
          if (!this.rxq.tryPut(f)) {
            const err = statusError(Code.INTERNAL, 'drpc: peer exceeded the advertised flow-control window')
            this.sendAbort(Code.INTERNAL)
            this.finishLocal(err)
          }
        } else if (!(await this.rxq.putBlocking(f, this.done, ctx.signal))) {
          // A peer that advertised no window is not paced, so delivery
          // blocks. The rx signal died mid-delivery: the transport is tearing
          // down (§4.5). The frame is gone and the window advanced — end the
          // call rather than leave a silent gap (§14).
          this.rxDropped++
          this.finishLocal(statusError(Code.UNAVAILABLE, 'transport closed during delivery'))
        }
      } else if (this.rxq.putDrop(f, this.rxPolicy) > 0) {
        this.protoEvent('dropped', 1)
      }
    } else {
      this.rxDropped++
    }
  }

  // grantWindow reports messages the application consumed and sends the
  // resulting credit (§4.2.1). Called only for buffered data frames — a
  // terminal payload never occupied a buffer slot.
  private grantWindow(n: number): void {
    // Never after the call ended: the peer has forgotten this sid, and a
    // grant for it would draw a RESET.
    if (this.done.tripped) return
    const g = this.flowRx.consumed(n)
    if (g === 0) return
    const f = frame({ epoch: this.conn.epoch, sid: this.sid, flags: FlagWindow, window: g })
    void this.transmit(f).catch(noop)
  }

  // recvInto decodes one received frame into a message: decompressed (bounded
  // by the receive cap), size-capped on the DECOMPRESSED bytes, then
  // unmarshaled. An oversize or undecodable message fails the call, as it
  // does on gRPC; a codec failure is the caller's to see, not the call's end.
  private async recvInto(f: Frame): Promise<Res> {
    let payload: Uint8Array
    try {
      payload = f.payload ?? EMPTY
      if (isCompressed(f)) payload = await decompressPayload(this.ci.compressor, payload, this.ci.maxRecv)
      checkRecvSize(payload.length, this.ci.maxRecv)
    } catch (e) {
      const err = toStatusError(e)
      this.sendAbort(Code.RESOURCE_EXHAUSTED)
      this.finishLocal(err)
      throw err
    }
    return this.resCodec.unmarshal(payload)
  }

  // latchHeader records the first header MD present on an accepted frame;
  // frames without the header field never latch (PROTOCOL.md §7, §11).
  private latchHeader(f: Frame): void {
    if (f.header === undefined) return
    if (this.rxHeader === undefined) this.rxHeader = f.header
    this.hdrLatch.trip()
  }

  // noteValidatedRx runs for every validated server frame of this stream:
  // accepted or dedup-dropped (PROTOCOL.md §9.1). It refreshes the idle
  // clocks and clears the OPEN retransmission obligation — any server frame
  // for the sid is the "first server frame" of §10.3.
  private noteValidatedRx(): void {
    this.lastRx = nowMs()
    this.conn.noteRx()
    this.retxOpen = undefined
  }

  // ------------------------------------------------------------------
  // send path
  // ------------------------------------------------------------------

  private openFrame(): Frame {
    this.txOpened = true
    const f = frame({
      epoch: this.conn.epoch,
      sid: this.sid,
      seq: this.txSeq.next(), // 1
      flags: FlagOpen,
      method: this.desc.path,
    })
    if (this.codecName !== '') f.codec = this.codecName
    // Like the codec, the compressor is named on the OPEN and governs the
    // whole call in both directions (§12.1).
    if (this.ci.compressorName !== '') f.compressor = this.ci.compressorName
    if (this.conn.isReliable) {
      // Advertise this side's receive window (§4.2.1); unreliable mode does
      // no flow control and leaves the field absent.
      f.window = this.rxSize
    }
    if (this.openHdr !== undefined) f.header = this.openHdr
    if (this.deadlineAt !== undefined) {
      // The remaining call budget travels on OPEN (PROTOCOL.md §10.2).
      f.timeoutMs = this.deadlineAt - nowMs()
    }
    if (!this.conn.isReliable) {
      this.retxOpen = f
      this.scheduleRetx()
    }
    return f
  }

  private nextFrame(): Frame {
    return frame({ epoch: this.conn.epoch, sid: this.sid, seq: this.txSeq.next() })
  }

  // sendOpen emits the eager OPEN for client-streaming/bidi calls
  // (PROTOCOL.md §8).
  /** @internal */
  async sendOpen(): Promise<void> {
    if (this.txOpened || this.txClosed) {
      // A racing abort already closed the call; an OPEN now would be a
      // protocol violation (its seq would not be 1).
      return
    }
    const f = this.openFrame()
    try {
      await this.transmit(f)
    } catch (e) {
      this.finishLocal(toStatusError(e))
    }
  }

  // sendRaw marshals and transmits one message, throwing any error. The
  // public send wraps it with the grpc-go swallowing contract.
  /** @internal */
  async sendRaw(m: Req): Promise<void> {
    if (this.done.tripped) throw new EndOfStreamError()
    if (this.txClosed) {
      if (this.abortFrame !== undefined) {
        // Closed by an abort, not by the user: not a contract violation.
        throw new EndOfStreamError()
      }
      throw statusError(Code.INTERNAL, 'send called after closeSend')
    }

    const raw = this.reqCodec.marshal(m)
    const opening = !this.txOpened

    // Flow control (§4.2.1): the OPEN creates the call and is never credited;
    // every later message waits for the peer's window. Parking here — not in
    // the receiver's delivery path — is what keeps one slow consumer from
    // stalling every call on the channel.
    if (!opening && !this.flowTx.tryAcquire()) {
      this.protoEvent('flow-stall')
      switch (await this.flowTx.acquire(this.done, this.ci.stallMs, this.callerSignal)) {
        case 'ok':
          this.protoEvent('flow-resume')
          break
        case 'ended':
          throw new EndOfStreamError()
        case 'aborted':
          throw this.callerSignal === undefined ? statusError(Code.CANCELLED, 'call cancelled') : abortCause(this.callerSignal)
        case 'stalled':
          throw statusError(Code.UNAVAILABLE, `drpc: flow-control stall: the peer granted no credit for ${this.ci.stallMs}ms`)
      }
    }

    // Compress BEFORE the frame exists. Compression is asynchronous here (the
    // platform's CompressionStream is a stream), and an await between a seq
    // allocation and the transmit would let a racing abort put ITS frame on
    // the wire first — a gap the peer's strict window would fail the call on.
    // Everything from openFrame() to transmit() below is synchronous.
    let enc = rawPayload(raw)
    if (this.ci.compressor !== undefined) {
      try {
        enc = await compressPayload(this.ci.compressor, raw)
      } catch (e) {
        if (!opening) this.flowTx.undo() // the message never reached the wire
        throw e
      }
    }
    try {
      // gRPC's send cap measures the bytes that go on the wire, i.e. after
      // compression (§12.1, §16).
      checkSendSize(enc.bytes.length, this.ci.maxSend)
    } catch (e) {
      if (!opening) this.flowTx.undo()
      throw e
    }
    // Re-check after the awaits above, as Go re-checks under txMu: an abort
    // may have ended the call while this send was parked or compressing.
    if (this.done.tripped) throw new EndOfStreamError()
    if (!opening && this.txClosed) throw new EndOfStreamError()

    let f: Frame
    if (!this.txOpened) {
      f = this.openFrame()
      if (!this.desc.clientStreams) {
        // Unary/server-streaming: the request piggybacks OPEN|CLOSE.
        // No code — this is the client's half-close (PROTOCOL.md §8).
        f.flags |= FlagClose
        this.txClosed = true
      }
    } else {
      f = this.nextFrame()
    }
    f.payload = enc.bytes
    if (enc.compressed) f.flags |= FlagCompressed

    try {
      await this.transmit(f)
    } catch (e) {
      this.undoRefused(f, e)
      throw e
    }
  }

  // undoRefused reclaims f's seq when the adapter refused the send
  // synchronously — the frame never reached the wire, so the next frame (the
  // abort, or a later message) must reuse the number (see TxSeq.undo).
  private undoRefused(f: Frame, err: unknown): void {
    if (!isMessageTooLarge(err)) return
    this.txSeq.undo(f.seq)
    if ((f.flags & FlagOpen) !== 0) {
      // The OPEN never reached the wire: nothing to retransmit, and the call
      // was never created at the server — so a later abort must not be sent
      // either (sendAbort checks txOpened).
      this.txOpened = false
      this.txClosed = false
      this.retxOpen = undefined
      this.retxAt = 0
    }
    if (isData(f)) this.flowTx.undo() // the credit was spent on nothing (§4.2.1)
  }

  // send transmits one message (grpc-go SendMsg parity): on a
  // clientStreams=false call it never throws — the status surfaces via
  // recv — and after the call ended it throws EndOfStreamError.
  async send(m: Req): Promise<void> {
    let err: unknown
    try {
      await this.sendRaw(m)
    } catch (e) {
      err = e
      if (!(e instanceof EndOfStreamError)) {
        // Marshal or transport failure: end the call (grpc-go does the
        // same), or a swallowed error would leave recv waiting for a
        // response that can never come.
        err = toStatusError(e)
        this.sendAbort(Code.CANCELLED)
        this.finishLocal(err as StatusError)
      }
    }
    if (!this.desc.clientStreams) return
    if (err !== undefined) throw err
  }

  // closeSend half-closes the send direction (grpc-go CloseSend parity: it
  // never fails; on a server-streaming call the request frame already
  // half-closed and this is a no-op).
  closeSend(): void {
    if (!this.desc.clientStreams) return
    if (this.txClosed || this.done.tripped) return
    this.txClosed = true
    const f = this.nextFrame()
    f.flags = FlagClose // no code: half-close
    if (!this.conn.isReliable) {
      // Retransmit until the terminal or a RESET (PROTOCOL.md §10.3).
      this.retxClose = f
      this.scheduleRetx()
    }
    void this.transmit(f).catch(noop)
  }

  // recv returns the next response message, or undefined at a clean end of
  // stream; a failed call throws its StatusError. Queued data is preferred,
  // so frames enqueued before the terminal are delivered in order even after
  // the call ended.
  async recv(): Promise<Res | undefined> {
    for (;;) {
      const f = this.rxq.tryTake()
      if (f !== undefined) return this.recvBuffered(f)
      if (this.done.tripped) return this.terminalRecv()
      await Promise.race([this.rxq.readable(), this.done.wait()])
    }
  }

  // recvBuffered delivers a frame taken out of the rx buffer and returns the
  // slot to the peer as flow-control credit.
  private async recvBuffered(f: Frame): Promise<Res> {
    // A failed delivery ended the call (recvInto); granting then would only
    // draw a RESET for a sid the peer has already forgotten (§4.2.1).
    const m = await this.recvInto(f)
    this.grantWindow(1)
    return m
  }

  private async terminalRecv(): Promise<Res | undefined> {
    if (this.termErr !== undefined) throw this.termErr
    const f = this.term as Frame
    if ((f.code ?? 0) !== Code.OK) throw terminalStatus(f)
    if (f.payload !== undefined && !this.termPayloadDelivered) {
      // Terminal payload (unary response, sendAndClose result) is delivered
      // once; the next recv reports end-of-stream. It never occupied a buffer
      // slot, so it is not credited back (§4.2.1).
      this.termPayloadDelivered = true
      return this.recvInto(f)
    }
    return undefined
  }

  async *[Symbol.asyncIterator](): AsyncIterator<Res> {
    for (;;) {
      const m = await this.recv()
      if (m === undefined) return
      yield m
    }
  }

  // header resolves with the server header metadata once the first accepted
  // frame carries one, or with the latched value (possibly undefined) when
  // the call ends first. Per the grpc-go contract it never rejects with the
  // call's status.
  async header(): Promise<Metadata | undefined> {
    await Promise.race([this.hdrLatch.wait(), this.done.wait()])
    return this.rxHeader
  }

  // trailer returns the trailer metadata; valid after the call ended.
  trailer(): Metadata | undefined {
    return this.trailerMd
  }

  // latchedHeader returns the server header MD latched so far WITHOUT blocking
  // (unlike header(), which awaits the first header-bearing frame). The Connect
  // adapter reads it after peeking the first response frame — by then any
  // header on an early frame has latched, and a no-header stream reads
  // undefined instead of blocking until the call ends.
  latchedHeader(): Metadata | undefined {
    return this.rxHeader
  }

  // cancel aborts the call locally and tells the server to stop
  // (PROTOCOL.md §10.3 abort path). No-op after the call ended.
  cancel(): void {
    if (this.done.tripped) return
    this.sendAbort(Code.CANCELLED)
    this.finishLocal(statusError(Code.CANCELLED, 'call cancelled'))
  }

  // ------------------------------------------------------------------
  // termination
  // ------------------------------------------------------------------

  // abortFromCtx runs when a caller cancellation source fires: send a
  // terminal CLOSE with the mapped code and finish locally (abort is
  // local-immediate, PROTOCOL.md §10.3).
  private abortFromCtx(): void {
    if (this.done.tripped) return
    let cause: StatusError
    if (this.deadlineFired) {
      cause = this.timeoutCause ?? statusError(Code.DEADLINE_EXCEEDED, 'deadline exceeded')
    } else if (this.callerSignal !== undefined && this.callerSignal.aborted) {
      cause = abortCause(this.callerSignal)
    } else {
      cause = statusError(Code.CANCELLED, 'call cancelled')
    }
    const code = cause.code === Code.DEADLINE_EXCEEDED ? Code.DEADLINE_EXCEEDED : Code.CANCELLED
    this.sendAbort(code)
    this.finishLocal(cause)
  }

  private sendAbort(code: Code): void {
    if (!this.txOpened) {
      // The OPEN never reached the wire (a local refusal, or an abort that
      // raced the first send): there is no call at the server to abort, and a
      // bare CLOSE would only draw a delayed RESET for a sid it has never
      // seen (§9.3).
      this.txClosed = true
      return
    }
    if (this.abortFrame !== undefined) {
      // Several paths can race here; one abort obligation is enough (§10.3)
      // — the first one stands.
      return
    }
    this.txClosed = true
    const f = this.nextFrame()
    f.flags = FlagClose
    f.code = code
    // The abort obligation outlives the call on its tombstone (PROTOCOL.md
    // §10.3); retire() picks it up.
    this.abortFrame = f
    void this.transmit(f).catch(noop)
  }

  // finishTerm ends the call with the server's terminal frame.
  private finishTerm(f: Frame): void {
    if (this.done.tripped) return
    this.term = f
    this.done.trip()
    this.release()
  }

  // finishLocal ends the call with a local error (cancel, RESET, DATA_LOSS).
  /** @internal */
  finishLocal(err: StatusError): void {
    if (this.done.tripped) return
    this.termErr = err
    this.done.trip()
    this.release()
  }

  // abandon releases a call whose caller has returned. If no terminal was
  // observed, the server must still be told to stop — otherwise the remote
  // handler would leak until its own timers fire.
  /** @internal */
  abandon(): void {
    if (this.done.tripped) return
    if (this.deadlineFired || (this.callerSignal !== undefined && this.callerSignal.aborted)) {
      this.abortFromCtx()
      return
    }
    this.sendAbort(Code.CANCELLED)
    this.finishLocal(statusError(Code.CANCELLED, 'call abandoned'))
  }

  // finishReset ends the call because the server declared it unknown, and
  // enters the abort path: if the RESET was stale or forged while a real
  // handler lives, the retransmitted abort reclaims it (PROTOCOL.md §9.3,
  // §10.3).
  /** @internal */
  finishReset(): void {
    if (this.done.tripped) return
    this.sendAbort(Code.CANCELLED)
    this.finishLocal(statusError(Code.UNAVAILABLE, 'call reset by peer'))
  }

  private release(): void {
    // Unpark any sender waiting on credit: the call is over, and nothing will
    // ever grant again (§4.2.1).
    this.flowTx.release()
    if (this.deadlineTimer !== undefined) {
      clearTimeout(this.deadlineTimer)
      this.deadlineTimer = undefined
    }
    this.signalDispose()
    this.conn.retire(this as ClientStream<unknown, unknown>)
    this.hdrLatch.trip()
  }

  // ------------------------------------------------------------------
  // unreliable-mode hooks driven by the Conn sweep
  // ------------------------------------------------------------------

  // transmit sends a non-probe frame, feeding the tx idle clocks.
  /** @internal */
  transmit(f: Frame): Promise<void> {
    this.lastTx = nowMs()
    return this.conn.transmit(f)
  }

  // scheduleRetx (re)arms the stream's retransmission timer. Each control
  // event starts a fresh RTI schedule (PROTOCOL.md §10.3) — a half-close
  // must not inherit the OPEN's backed-off cadence.
  private scheduleRetx(): void {
    if (this.conn.isReliable) return
    this.retxIval = this.conn.timing.retransmitMs
    this.retxAt = nowMs() + this.retxIval
    this.conn.kickSweep()
  }

  // dropRetx ends this stream's retransmission obligations: it is retired,
  // and any pending abort now lives on the tombstone (PROTOCOL.md §9.2).
  /** @internal */
  dropRetx(): void {
    this.retxOpen = undefined
    this.retxClose = undefined
    this.retxAt = 0
  }

  // sweepRetx returns the control frames due for retransmission and advances
  // the backoff (×2, capped at T_probe). PROTOCOL.md §10.3.
  /** @internal */
  sweepRetx(now: number, capMs: number): Frame[] {
    if (this.retxAt === 0 || now < this.retxAt) return []
    const out: Frame[] = []
    if (this.retxOpen !== undefined) out.push(this.retxOpen)
    if (this.retxClose !== undefined) out.push(this.retxClose)
    if (out.length === 0) {
      this.retxAt = 0
      return []
    }
    this.retxIval = Math.min(this.retxIval * 2, capMs)
    this.retxAt = now + this.retxIval
    return out
  }

  // probeDue emits a stream probe when both idle clocks passed T_probe
  // (PROTOCOL.md §10.5). Probes reset neither idle clock.
  /** @internal */
  probeDue(now: number, probeMs: number): Frame | undefined {
    if (now - this.lastRx < probeMs || now - this.lastTx < probeMs || now - this.lastProbe < probeMs) {
      return undefined
    }
    this.lastProbe = now
    return frame({ epoch: this.conn.epoch, sid: this.sid, flags: FlagPing })
  }
}
