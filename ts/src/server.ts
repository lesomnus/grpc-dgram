// The server endpoint: Server, its per-call streams, and the server half of
// the unreliable-mode machinery — per-incarnation peer containers
// (tombstones, aged watermark, liveness), delayed RESETs, stream probes, and
// the sweep loop (PROTOCOL.md §9–§10, Appendix C).
//
// State layout note: the Go original keeps flat maps keyed by
// (peer, epoch, sid) tuples; JS Maps have no composite keys, so state nests
// instead — transport peer → PeerSlot → client incarnation → PeerState →
// calls. The §15 caps that Go checks as flat map sizes are preserved as
// server-wide counters. Synchronous sections need no locks; the demux path
// from handle() through open() deliberately contains no await, which is what
// makes Go's under-lock re-checks unnecessary here.

import type { CallOptions, MethodDesc, NamedCodec, PayloadCodec } from './desc'
import { isUnary } from './desc'
import { chain2, type StreamServerHandler, type StreamServerInterceptor, type UnaryServerHandler, type UnaryServerInterceptor } from './interceptor'
import { resolveLimits, resolveRxConfig, type Limits, type ResolvedLimits, type ResolvedRxConfig, type RxBufferConfig } from './limits'
import { metadataJoin, validateMetadata, type Metadata } from './metadata'
import { RxVerdict, RxWindow, TxSeq } from './seq'
import { emit, statsSink, type ProtocolEventKind, type ProtocolStats } from './stats'
import { abortCause, Code, isMessageTooLarge, StatusError, statusError, toStatusError } from './status'
import { resolveTiming, type Mode } from './timing'
import { hasTransportInfo, type FrameContext, type FrameHandler } from './seam'
// FlowTiming / DetailedStatusError are declared in conn.ts (Timing and
// status.ts are shared and were left untouched); both imports are type-only,
// so the server pulls in no client code.
import type { DetailedStatusError, FlowTiming } from './conn'
import {
  checkRecvSize,
  checkSendSize,
  compressPayload,
  DEFAULT_MAX_RECV_MSG_SIZE,
  DEFAULT_MAX_SEND_MSG_SIZE,
  DEFAULT_STALL_MS,
  decompressPayload,
  FlowReceiver,
  FlowSender,
  FrameQueue,
  Latch,
  nonzeroEpoch,
  noop,
  nowMs,
  rawPayload,
  reliableRxSize,
  sizeOr,
  Sweeper,
  unrefTimer,
  type Compressor,
  type WirePayload,
} from './util'
import {
  FlagClose,
  FlagCompressed,
  FlagPing,
  FlagReset,
  FlagWindow,
  frame,
  frameStatus,
  hasUnknownFlags,
  isClose,
  isCompressed,
  isData,
  isHalfClose,
  isOpen,
  isPing,
  isReset,
  isTerminal,
  legalShape,
  resetFor,
  setFrameError,
  shapeOf,
  type Any,
  type Frame,
} from './wire'

export interface ServerOptions {
  // Overrides transport discovery (PROTOCOL.md §4.3); a mixed-mode gateway
  // instead annotates each frame's context (FrameContext.reliable).
  reliable?: boolean
  // Protocol timers (unreliable mode only, §10.1 — except stallMs, §4.2.1).
  timing?: FlowTiming
  // Clamps client-asserted timeouts (§10.2). Off unless set.
  maxHandlerTimeoutMs?: number
  // Endpoint-wide per-stream rx buffer (§4.2).
  rxBuffer?: RxBufferConfig
  // Per-method rx buffer overrides, by full method name (§4.2).
  methodRxBuffer?: Record<string, RxBufferConfig>
  // Resource caps (§15).
  limits?: Limits
  // Named wire codecs this server accepts beyond '' = proto (§12).
  codecs?: Record<string, NamedCodec>
  // Message compressors this server accepts, by name (§12.1) — the twin of
  // ConnOptions.compressors. An OPEN naming anything absent here draws
  // T{UNIMPLEMENTED}, exactly like an unknown codec: a server serves what it
  // registered, never what a peer asks for, the way a grpc-go server serves
  // exactly the compressors its binary imported. To speak the interop
  // baseline, hand it the platform's:
  //
  //   new Server(tx, { compressors: { gzip: getCompressor('gzip')! } })
  compressors?: Record<string, Compressor>
  // Caps one received message AFTER decompression (default 4 MiB) and one
  // sent message AFTER compression (default effectively unlimited), failing
  // with RESOURCE_EXHAUSTED exactly as gRPC does (grpc.MaxRecvMsgSize /
  // MaxSendMsgSize parity). 0 rejects everything — reading it as "unlimited"
  // would turn a deliberate lockdown into an open door.
  maxRecvMsgSize?: number
  maxSendMsgSize?: number
  // Interceptor chains (interceptor.ts). Element 0 runs outermost; the last
  // element is handed the registered handler — grpc-go's order
  // (ChainUnaryInterceptors / ChainStreamInterceptors). One stream chain
  // serves all three streaming shapes; ctx.desc tells them apart.
  unaryInterceptors?: UnaryServerInterceptor[]
  streamInterceptors?: StreamServerInterceptor[]
  // Observers of the protocol events gRPC has no concept of (PROTOCOL.md
  // §14; stats.ts) — the twin of ConnOptions.protocolStats. Every event a
  // server emits names the transport peer it concerns, which is what makes a
  // RESET storm attributable (§15). One or several; called synchronously on
  // the receive path and the sweep, and must not block. A throw is contained
  // and costs that observer the event, never the endpoint the step.
  protocolStats?: ProtocolStats | ProtocolStats[]
}

// EMPTY stands in for an absent payload: a frame without one decodes as the
// zero-length message, never as garbage.
const EMPTY = new Uint8Array(0)

// detailsOf reads the rich google.rpc.Status details a handler attached to
// its error (§5): a StatusError carrying `details` — the DetailedStatusError
// shape the client reads back with statusDetails() — rides the terminal
// frame. Anything else is ignored, and each entry is shape-checked, so a
// stray property can never crash the frame encoder.
function detailsOf(err: unknown): Any[] | undefined {
  if (!(err instanceof StatusError)) return undefined
  const d = (err as DetailedStatusError).details
  if (!Array.isArray(d) || d.length === 0) return undefined
  const out: Any[] = []
  for (const e of d as unknown[]) {
    if (e === null || typeof e !== 'object') continue
    const a = e as { typeUrl?: unknown; value?: unknown }
    if (typeof a.typeUrl === 'string' && a.value instanceof Uint8Array) out.push({ typeUrl: a.typeUrl, value: a.value })
  }
  return out.length > 0 ? out : undefined
}

const errText = (e: unknown): string => (e instanceof Error ? e.message : String(e))

// windowOf reads a frame's flow-control field defensively: a hand-built
// partial frame (or one from a JS caller) may carry no `window` at all, and a
// NaN credit would park a sender forever.
function windowOf(f: Frame): number {
  const w = f.window
  return typeof w === 'number' && Number.isFinite(w) && w > 0 ? Math.floor(w) : 0
}

// ---------------------------------------------------------------------------
// handler surface
// ---------------------------------------------------------------------------

export interface ServerContext {
  // Aborted when the call ends for any reason: terminal frames, RESET,
  // liveness expiry, deadline, window overrun, stop, disconnectPeer
  // (PROTOCOL.md §9.4). The reason is the StatusError cause.
  readonly signal: AbortSignal
  // Incoming request metadata from the OPEN frame (§11).
  readonly metadata: Metadata | undefined
  readonly peer: unknown
  // Full method name (§13) — desc.path — and the descriptor it was
  // registered under, which is how a stream interceptor tells the three
  // streaming shapes apart (clientStreams / serverStreams).
  readonly method: string
  readonly desc: MethodDesc
  // Call deadline (epoch ms) when the client propagated a budget (§10.2).
  readonly deadline: number | undefined
  // Joins metadata into the pending header (§11). It rejects — StatusError
  // {INTERNAL} — once the header is already on the wire, and for metadata
  // that is not legal on the wire (grpc-go's Validate rules).
  setHeader(md: Metadata): void
  // Flushes the header immediately as an H frame — on unary calls too, so a
  // client's header() returns before the response exists, as it does on gRPC
  // (§8, §11). Flushing twice rejects with INTERNAL; the core's own creation
  // ack is not a flush.
  sendHeader(md?: Metadata): Promise<void>
  // Records trailer metadata, which rides the terminal frame (§11). Invalid
  // metadata is dropped, as grpc-go does.
  setTrailer(md: Metadata): void
}

export interface ServerReader<Req> {
  // Returns the next request message, or undefined once the client
  // half-closed. A cancelled call throws its status.
  recv(): Promise<Req | undefined>
  [Symbol.asyncIterator](): AsyncIterator<Req>
}

export interface ServerWriter<Res> {
  send(msg: Res): Promise<void>
}

export type UnaryHandler<Req, Res> = (req: Req, ctx: ServerContext) => Res | Promise<Res>
export type ServerStreamingHandler<Req, Res> = (req: Req, stream: ServerWriter<Res>, ctx: ServerContext) => void | Promise<void>
export type ClientStreamingHandler<Req, Res> = (stream: ServerReader<Req>, ctx: ServerContext) => Res | Promise<Res>
export type BidiHandler<Req, Res> = (stream: ServerReader<Req> & ServerWriter<Res>, ctx: ServerContext) => void | Promise<void>

type AnyHandler = UnaryHandler<unknown, unknown> | ServerStreamingHandler<unknown, unknown> | ClientStreamingHandler<unknown, unknown> | BidiHandler<unknown, unknown>

interface Registration {
  desc: MethodDesc<unknown, unknown>
  handler: AnyHandler
}

// ---------------------------------------------------------------------------
// per-peer state
// ---------------------------------------------------------------------------

// srvTomb remembers a finished call: its stored terminal frame is replayed
// (rate-limited) when stragglers or probes hit it (PROTOCOL.md §9.2).
interface SrvTomb {
  sid: number
  term: Frame | undefined // undefined = key-only (dedup preserved, replay lost)
  size: number
  expireAt: number
  lastReplay: number
}

interface HwmCheckpoint {
  at: number
  hwm: number
}

// pendingReset is a scheduled delayed RESET for an unknown-sid frame whose
// OPEN may merely be late (PROTOCOL.md §9.3).
interface PendingReset {
  due: number
  echo: number // epoch of the offending frame
  peerEcho: number // peer_epoch of the offending frame (§9.3)
  epoch: number
  sid: number
}

// PeerState is the container for one client incarnation seen from one
// transport peer: (peer, client-epoch) (PROTOCOL.md §9.4).
class PeerState {
  hwm = 0
  cps: HwmCheckpoint[] = [] // watermark checkpoints, appended by the sweep (§9.4)
  dead = false // liveness expired; cleared state

  readonly tombs = new Map<number, SrvTomb>()
  tombOrder: number[] = [] // insertion order for byte-cap degradation
  tombBytes = 0
  // tombFloor covers entry-cap evictions (§9.2, §15): sids at or below it
  // keep key-only tombstone semantics — deduped, replay lost — at zero
  // memory. sids are monotonic per incarnation (§6.2), so evicting the
  // lowest sid and raising the floor loses nothing the entry could dedup.
  tombFloor = 0

  readonly calls = new Map<number, ServerStream<unknown, unknown>>()
  liveCalls = 0

  lastRx: number
  lastTx: number
  lastPing = 0

  constructor(
    readonly peer: unknown,
    readonly epoch: number,
    // The mode of this peer's channel, captured at container creation from
    // the frame annotation (PROTOCOL.md §4.3). A reliable container runs no
    // timers: the sweep skips it entirely — no liveness, no PING, no
    // checkpoints, no GC (it lives until teardown, §10.6).
    readonly reliable: boolean,
    readonly createdAt: number,
    readonly maxTombs: number,
    readonly maxTombBytes: number,
  ) {
    this.lastRx = createdAt
    this.lastTx = createdAt
  }

  // hwmAged is the high-water mark as of TTL_tomb ago; sids at or below it
  // are necessarily stale (PROTOCOL.md §9.4). Reliable mode degenerates to
  // the plain current hwm (no aging: nothing is ever late).
  hwmAged(now: number, ttlMs: number): number {
    if (this.reliable) return this.hwm
    let aged = 0
    for (const cp of this.cps) {
      if (now - cp.at >= ttlMs) aged = cp.hwm
    }
    return aged
  }

  addTomb(sid: number, term: Frame | undefined, expireAt: number): void {
    if (sid <= this.tombFloor) {
      // Already covered key-only by the floor: an entry would add nothing
      // but the (lost) replay.
      return
    }
    const size = term?.payload?.length ?? 0
    const old = this.tombs.get(sid)
    if (old !== undefined) {
      // Replace in place: keep the order entry, fix the byte accounting.
      this.tombBytes += size - old.size
      old.term = term
      old.size = size
      if (expireAt > old.expireAt) old.expireAt = expireAt
      return
    }
    this.tombs.set(sid, { sid, term, size, expireAt, lastReplay: 0 })
    this.tombOrder.push(sid)
    this.tombBytes += size

    // Byte cap: degrade oldest stored terminals to key-only (§9.2, §15).
    for (let i = 0; this.tombBytes > this.maxTombBytes && i < this.tombOrder.length; i++) {
      const tb = this.tombs.get(this.tombOrder[i]!)
      if (tb !== undefined && tb.term !== undefined) {
        this.tombBytes -= tb.size
        tb.term = undefined
        tb.size = 0
      }
    }
    // Entry cap: evict the lowest sid and raise the floor — dedup for the
    // evicted sid survives at zero memory, so no re-execution window opens
    // (§9.2, §14, §15). Only the stored replay is lost.
    while (this.tombs.size > this.maxTombs) {
      let lowest = 0
      for (const tsid of this.tombs.keys()) {
        if (lowest === 0 || tsid < lowest) lowest = tsid
      }
      const tb = this.tombs.get(lowest)
      if (tb !== undefined) this.tombBytes -= tb.size
      this.tombs.delete(lowest)
      if (this.tombFloor < lowest) this.tombFloor = lowest
    }
  }

  removeTomb(sid: number): void {
    const tb = this.tombs.get(sid)
    if (tb !== undefined) {
      this.tombBytes -= tb.size
      this.tombs.delete(sid)
    }
  }

  // replayDue reports whether the per-tombstone rate limit would allow a
  // replay now, without spending anything — callers check the aggregate
  // reply budget (§15) between this and replayTomb, so a budget-denied reply
  // burns neither the 1/RTI slot nor the keepalive clock.
  replayDue(tb: SrvTomb, now: number, rtiMs: number): boolean {
    return tb.term !== undefined && now - tb.lastReplay >= rtiMs
  }

  // replayTomb returns the stored terminal if the per-tombstone rate limit
  // allows another replay (≤ 1 per RTI, PROTOCOL.md §9.2).
  replayTomb(tb: SrvTomb, now: number, rtiMs: number): Frame | undefined {
    if (tb.term === undefined || now - tb.lastReplay < rtiMs) return undefined
    tb.lastReplay = now
    this.lastTx = now
    return tb.term
  }
}

// PeerSlot aggregates everything the server keeps for one transport peer;
// the Go flat maps keyed by peer / callKey live here per slot, with the §15
// caps enforced through server-wide counters.
class PeerSlot {
  readonly epochs = new Map<number, PeerState>() // client incarnations (§6.1)
  liveCalls = 0 // across client epochs (§15 MaxLiveCalls)
  replyBudget: { windowStart: number; n: number } | undefined
  readonly resetAt = new Map<string, number>() // `${epoch}:${sid}` → last immediate RESET
  readonly pendingResets = new Map<string, PendingReset>()

  constructor(readonly peer: unknown) {}
}

const eksid = (epoch: number, sid: number): string => `${epoch}:${sid}`

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

export class Server {
  // This Server incarnation's nonce (PROTOCOL.md §6.1).
  readonly epoch: number

  private readonly tx: FrameHandler
  private readonly mode: Mode
  private readonly maxHandlerTimeoutMs: number
  private readonly rxCfg: ResolvedRxConfig
  private readonly methodRx: Map<string, ResolvedRxConfig>
  private readonly limits: ResolvedLimits
  private readonly codecs: Map<string, NamedCodec>
  private readonly compressors: Map<string, Compressor>
  // Endpoint-wide per-call size caps (§16, gRPC parity) and the flow-control
  // park bound (§4.2.1).
  private readonly maxRecv: number
  private readonly maxSend: number
  private readonly stallMs: number
  /** @internal */ readonly pstats: readonly ProtocolStats[]

  private readonly services = new Map<string, Registration>()
  // serving flips on the first handle; the registry is immutable after that
  // (PROTOCOL.md §13).
  private serving = false

  private readonly slots = new Map<unknown, PeerSlot>()
  // Server-wide sizes of the per-slot maps, standing in for Go's flat-map
  // len() cap checks (§15).
  private pendingResetTotal = 0
  private resetAtTotal = 0
  private replyBudgetTotal = 0

  private drain = false
  private closed = false
  private liveTasks = 0
  private idleWaiters: (() => void)[] = []

  // Latches once any unreliable-mode state exists; until then the sweeper
  // has nothing it could ever do (reliable peers run no timers) and is never
  // started.
  private sawUnreliable = false
  private readonly sw = new Sweeper()

  // The folded interceptor chains; undefined = none, and the call goes
  // straight to the registered handler (server.go NewServer).
  private readonly unaryInt: UnaryServerInterceptor | undefined
  private readonly streamInt: StreamServerInterceptor | undefined

  constructor(tx: FrameHandler, opts: ServerOptions = {}) {
    this.epoch = nonzeroEpoch()
    this.tx = tx
    this.mode = {
      reliable: opts.reliable ?? (hasTransportInfo(tx) ? tx.reliable() : false),
      timing: resolveTiming(opts.timing),
    }
    this.maxHandlerTimeoutMs = opts.maxHandlerTimeoutMs ?? 0
    this.rxCfg = resolveRxConfig(opts.rxBuffer)
    this.methodRx = new Map(Object.entries(opts.methodRxBuffer ?? {}).map(([k, v]) => [k, resolveRxConfig(v)]))
    this.limits = resolveLimits(opts.limits)
    this.codecs = new Map(Object.entries(opts.codecs ?? {}))
    this.compressors = new Map(Object.entries(opts.compressors ?? {}))
    this.maxRecv = sizeOr(opts.maxRecvMsgSize, DEFAULT_MAX_RECV_MSG_SIZE)
    this.maxSend = sizeOr(opts.maxSendMsgSize, DEFAULT_MAX_SEND_MSG_SIZE)
    this.stallMs = opts.timing?.stallMs ?? DEFAULT_STALL_MS
    this.pstats = statsSink(opts.protocolStats)
    this.unaryInt = chain2(opts.unaryInterceptors)
    this.streamInt = chain2(opts.streamInterceptors)
  }

  // protoEvent reports one peer-scope protocol event (stats.ts): the transport
  // peer it concerns, and the sid where the event is about a call this server
  // does not hold (a RESET, a tombstone replay).
  private protoEvent(kind: ProtocolEventKind, peer: unknown, sid = 0): void {
    if (this.pstats.length === 0) return
    emit(this.pstats, { kind, peer, sid, method: '', count: 0 })
  }

  register<Req, Res>(desc: MethodDesc<Req, Res> & { clientStreams: false; serverStreams: false }, handler: UnaryHandler<Req, Res>): void
  register<Req, Res>(desc: MethodDesc<Req, Res> & { clientStreams: false; serverStreams: true }, handler: ServerStreamingHandler<Req, Res>): void
  register<Req, Res>(desc: MethodDesc<Req, Res> & { clientStreams: true; serverStreams: false }, handler: ClientStreamingHandler<Req, Res>): void
  register<Req, Res>(desc: MethodDesc<Req, Res> & { clientStreams: true; serverStreams: true }, handler: BidiHandler<Req, Res>): void
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  register(desc: MethodDesc<any, any>, handler: AnyHandler): void {
    if (this.serving) {
      throw new Error('drpc: register called after the server started serving')
    }
    this.services.set(desc.path, { desc: desc as MethodDesc<unknown, unknown>, handler })
  }

  // rxReliable resolves the mode governing a received frame: the adapter's
  // per-channel annotation when present (PROTOCOL.md §4.3), else the
  // server's own mode.
  private rxReliable(ctx: FrameContext): boolean {
    return ctx.reliable ?? this.mode.reliable
  }

  // handle delivers one client frame to this Server. Adapters call it for
  // each frame of a received envelop, in order, with the peer in ctx,
  // awaiting each (PROTOCOL.md §9.1).
  async handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    this.serving = true

    if (isReset(f)) {
      // Act only if the echoed epoch is ours; RESET never refreshes
      // liveness (PROTOCOL.md §9.1, §9.3).
      if (f.epoch !== this.epoch) return
      this.protoEvent('reset-received', ctx.peer, f.sid)
      this.resetByPeerSid(ctx.peer, f.sid, f.peerEpoch)
      return
    }

    const peer = ctx.peer
    const sid = f.sid
    const slot = this.slots.get(peer)
    const ps = slot?.epochs.get(f.epoch)
    const now = nowMs()

    if (isPing(f)) {
      // Well-formed PINGs are validated (PROTOCOL.md §9.1).
      if (ps !== undefined) ps.lastRx = now
      if (sid === 0) return // peer keepalive (§10.4)
      // Stream probe (§10.5): live → no-op; tombstone with a stored T →
      // replay; key-only or unknown → immediate RESET (§9.3).
      if (ps?.calls.has(sid)) return
      const tb = ps?.tombs.get(sid)
      if (ps !== undefined && tb !== undefined) {
        const rti = this.mode.timing.retransmitMs
        if (ps.replayDue(tb, now, rti) && this.allowReply(slot!, now)) {
          const replay = ps.replayTomb(tb, now, rti)
          if (replay !== undefined) {
            this.protoEvent('tombstone-replay', peer, sid)
            await this.send(replay, ctx)
            return
          }
        }
        if (tb.term !== undefined) return // replay rate-limited; next probe retries
        return this.sendReset(slot, f, ctx)
      }
      return this.sendReset(slot, f, ctx)
    }

    const st = ps?.calls.get(sid)
    if (st !== undefined) {
      await st.handleRx(f, ctx)
      return
    }

    if (shapeOf(f) === FlagWindow) {
      // See Conn.handle: a grant for a call this side has already finished is
      // dropped in silence, never answered with a RESET (§4.2.1, §9.3) — a
      // grant legitimately races the call's end, and answering it would turn
      // every well-behaved stream into a RESET exchange. It is also not a
      // tombstone hit: a WINDOW carries no seq and validates nothing.
      return
    }

    if (ps !== undefined) {
      // Tombstoned call: validated; replay the stored terminal, rate-limited
      // (PROTOCOL.md §9.2).
      const tb = ps.tombs.get(sid)
      if (tb !== undefined) {
        ps.lastRx = now
        const rti = this.mode.timing.retransmitMs
        if (ps.replayDue(tb, now, rti) && this.allowReply(slot!, now)) {
          const replay = ps.replayTomb(tb, now, rti)
          if (replay !== undefined) {
            this.protoEvent('tombstone-replay', peer, sid)
            await this.send(replay, ctx)
          }
        }
        return
      }
      if (sid <= ps.tombFloor) {
        // Evicted under the entry cap: the floor keeps key-only semantics
        // (validated, deduped, replay lost) at zero memory (PROTOCOL.md
        // §9.2, §14) — this also swallows duplicate OPENs, so eviction opens
        // no re-execution window.
        ps.lastRx = now
        return
      }
    }

    if (isOpen(f) && f.seq === 1) {
      // Aged-watermark admission (PROTOCOL.md §9.4): an unknown sid at or
      // below hwm_aged is necessarily a stale straggler.
      if (ps !== undefined) {
        // sids never wrap (§6.2): plain comparison.
        if (sid <= ps.hwmAged(now, this.mode.timing.tombstoneMs)) {
          return this.sendReset(slot, f, ctx)
        }
      }
      return this.open(ctx, f)
    }

    // Unknown, non-OPEN, non-PING: delayed RESET — the OPEN may merely be
    // late (PROTOCOL.md §9.3). A reliable channel has no reordering:
    // immediate.
    if (this.rxReliable(ctx)) {
      return this.sendReset(slot, f, ctx)
    }
    const s = this.ensureSlot(peer)
    const k = eksid(f.epoch, sid)
    if (!s.pendingResets.has(k) && this.pendingResetTotal < this.limits.maxPendingResets) {
      s.pendingResets.set(k, { due: now + this.mode.timing.holdMs, echo: f.epoch, peerEcho: f.peerEpoch, epoch: f.epoch, sid })
      this.pendingResetTotal++
      this.sawUnreliable = true
    }
    this.kickSweep()
  }

  // ------------------------------------------------------------------
  // call creation (PROTOCOL.md §9.4)
  // ------------------------------------------------------------------

  private async open(ctx: FrameContext, f: Frame): Promise<void> {
    if (this.drain || this.closed) {
      // Draining/stopped servers refuse new calls with RESET (§9.4).
      return this.sendReset(this.slots.get(ctx.peer), f, ctx)
    }

    // The frame's channel mode governs the whole call (PROTOCOL.md §4.3):
    // captured here, inherited by the stream and the peer container.
    const rel = this.rxReliable(ctx)

    // Methods are addressed by full name, always (PROTOCOL.md §13).
    const reg = this.services.get(f.method)
    if (reg === undefined) {
      return this.rejectOpen(ctx, f, Code.UNIMPLEMENTED, 'method not found')
    }
    let codec: { request: PayloadCodec<unknown>; response: PayloadCodec<unknown> }
    if (f.codec === '') {
      codec = { request: reg.desc.request, response: reg.desc.response }
    } else {
      const named = this.codecs.get(f.codec)
      if (named === undefined) {
        return this.rejectOpen(ctx, f, Code.UNIMPLEMENTED, `unsupported codec: ${f.codec}`)
      }
      codec = named.resolve(reg.desc)
    }
    // The compressor, like the codec, is named on the OPEN and governs the
    // whole call in both directions (PROTOCOL.md §12.1). Truthiness, not
    // `!== ''`: a frame handed in by JS (or by a hand-built partial literal)
    // must read as "no compressor", never as one named "undefined" — the same
    // discipline encodeFrame applies (wire.ts).
    let comp: Compressor | undefined
    if (f.compressor) {
      // Registered compressors only — the server never enables one implicitly
      // for a peer that asks, the way grpc-go's server serves exactly what the
      // binary registered (§12.1).
      comp = this.compressors.get(f.compressor)
      if (comp === undefined) {
        return this.rejectOpen(ctx, f, Code.UNIMPLEMENTED, `unsupported compressor: ${f.compressor}`)
      }
    }

    const now = nowMs()
    const slot = this.ensureSlot(ctx.peer)
    const ps = this.ensurePeer(slot, f.epoch, now, rel)
    // No re-check of tombs/watermark here: unlike the Go original, nothing
    // can interleave between handle()'s checks and this point (no await).
    if (slot.liveCalls >= this.limits.maxLiveCalls) {
      // Live-call cap per transport peer (PROTOCOL.md §15): refuse rather
      // than let one peer's OPEN flood spawn unbounded handlers. Counted
      // across client epochs — an epoch-spoofing peer gets no more.
      return this.rejectOpen(ctx, f, Code.RESOURCE_EXHAUSTED, 'too many concurrent calls')
    }

    // The reliable-mode floor (§4.2.1) is applied here, not in resolveRxConfig:
    // it depends on the mode of the channel THIS call arrived on, which a
    // per-peer mixed-mode server resolves per frame (§4.3).
    const cfg = this.methodRx.get(reg.desc.path) ?? this.rxCfg
    const rxCfg: ResolvedRxConfig = { size: reliableRxSize(cfg.size, rel), policy: cfg.policy }
    const st = new ServerStream(this, ctx.peer, f.epoch, f.sid, reg, codec, rxCfg, rel, comp, this.maxRecv, this.maxSend, this.stallMs)
    st.ps = ps
    st.slot = slot
    if (rel) {
      // The OPEN advertises the client's receive window (§4.2.1); it precedes
      // every server frame, so the server never needs the assumed window.
      st.flowTx.observe(windowOf(f))
    }

    // The client-asserted budget bounds the handler ctx, clamped by the
    // server cap when configured (PROTOCOL.md §10.2). A non-positive budget
    // (expired before the OPEN escaped) yields an already-expired ctx, not
    // an unbounded one: the handler unwinds into T{DEADLINE_EXCEEDED} at
    // once.
    if (f.timeoutMs !== undefined) {
      let d = f.timeoutMs
      if (this.maxHandlerTimeoutMs > 0 && d > this.maxHandlerTimeoutMs) d = this.maxHandlerTimeoutMs
      st.deadlineAt = now + d
      if (d <= 0) {
        st.cancel(statusError(Code.DEADLINE_EXCEEDED, 'call timeout'))
      } else {
        const t = setTimeout(() => st.cancel(statusError(Code.DEADLINE_EXCEEDED, 'call timeout')), d)
        unrefTimer(t)
        st.deadlineTimer = t
      }
    }
    st.metadata = f.header

    ps.calls.set(f.sid, st as ServerStream<unknown, unknown>)
    slot.liveCalls++
    ps.liveCalls++
    if (ps.hwm < f.sid) ps.hwm = f.sid
    ps.dead = false // the peer is evidently back
    ps.lastRx = now
    // The OPEN arrived after all: cancel any RESET scheduled for its sid.
    if (slot.pendingResets.delete(eksid(f.epoch, f.sid))) this.pendingResetTotal--
    this.liveTasks++
    this.kickSweep()

    const desc = reg.desc
    if (isUnary(desc)) {
      void this.runUnary(st, f)
    } else if (!desc.clientStreams) {
      // A server-streaming OPEN piggybacks the request message and the
      // half-close (PROTOCOL.md §8); the handler reads the request from the
      // stream buffer.
      if (f.payload !== undefined) st.rxq.tryPut(f)
      if (isClose(f)) st.rxEOF.trip()
      // Creation ack (§8): without it, a slow producer would leave the
      // client's OPEN|CLOSE — full request payload — retransmitting.
      st.sendH()
      void this.runStream(st)
    } else {
      // CS/bidi OPENs are eager and bare: payload or CLOSE here is
      // off-shape and dropped (PROTOCOL.md §8).
      if (f.payload !== undefined || isClose(f)) st.rxDropped++
      // Creation ack (PROTOCOL.md §8).
      st.sendH()
      void this.runStream(st)
    }
  }

  // rejectOpen answers an OPEN that cannot start a call with a terminal
  // frame, tombstone-stored so duplicates elicit a rate-limited replay
  // instead of a fresh answer each (PROTOCOL.md §9.4).
  private async rejectOpen(ctx: FrameContext, f: Frame, code: Code, msg: string): Promise<void> {
    const t = frame({
      epoch: this.epoch,
      sid: f.sid,
      seq: 1,
      flags: FlagClose,
      desc: msg,
      peerEpoch: f.epoch, // name the client incarnation (§6.1)
    })
    t.code = code

    if (!this.rxReliable(ctx)) {
      const now = nowMs()
      const slot = this.ensureSlot(ctx.peer)
      const ps = this.ensurePeer(slot, f.epoch, now, false)
      ps.lastRx = now
      if (ps.hwm < f.sid) ps.hwm = f.sid
      ps.addTomb(f.sid, t, now + this.mode.timing.tombstoneMs)
      this.kickSweep()
    }
    await this.send(t, ctx)
  }

  private async runUnary(st: ServerStream<unknown, unknown>, open: Frame): Promise<void> {
    let term: Frame | undefined
    try {
      let err: StatusError | undefined
      let resp: unknown
      try {
        // The request rides the OPEN; it is decompressed and size-capped like
        // any received message (§12.1).
        const req = await st.decodeMessage(open)
        const h: UnaryServerHandler = async (r, c) => (await (st.reg.handler as UnaryHandler<unknown, unknown>)(r, c)) as NonNullable<unknown>
        resp = this.unaryInt === undefined ? await h(req, st.context) : await this.unaryInt(req, st.context, h)
        // A response is required; nothing is a bug upstream (a handler or an
        // interceptor that forgot to return), not an empty message.
        if (resp === undefined) throw statusError(Code.INTERNAL, 'unary handler produced no response')
      } catch (e) {
        err = toStatusError(e)
      }
      if (err === undefined && st.signal.aborted) err = abortCause(st.signal)
      if (err === undefined) {
        try {
          st.setResponse(resp)
        } catch (e) {
          err = statusError(Code.INTERNAL, `marshal response: ${errText(e)}`)
        }
      }
      const t = await st.terminalFrame(err)

      // The terminal is sent even when the handler ctx ended: the client (or
      // its tombstone) decides what to do with it — unless the peer disowned
      // the call (RESET) or vanished (liveness), where nothing listens (§9.3).
      if (st.suppressTerm) return
      term = t
      void st.transmitTerminal(t).catch(noop)
    } finally {
      this.finish(st, term)
      this.taskDone()
    }
  }

  private async runStream(st: ServerStream<unknown, unknown>): Promise<void> {
    let term: Frame | undefined
    try {
      const desc = st.reg.desc
      let err: StatusError | undefined
      try {
        // The innermost handler runs the registered one in its shape. It
        // uses the stream and ctx it is handed, not st: an interceptor may
        // wrap either (grpc-go's WrapServerStream idiom).
        const last: StreamServerHandler = async (stream, ctx) => {
          if (!desc.clientStreams) {
            // Server-streaming: the request rode the OPEN (§8).
            const req = await stream.recv()
            if (req === undefined) throw statusError(Code.UNKNOWN, 'missing request message')
            return (st.reg.handler as ServerStreamingHandler<unknown, unknown>)(req, stream, ctx)
          }
          if (!desc.serverStreams) return (st.reg.handler as ClientStreamingHandler<unknown, unknown>)(stream, ctx)
          return (st.reg.handler as BidiHandler<unknown, unknown>)(stream, ctx)
        }
        const out = this.streamInt === undefined ? await last(st, st.context) : await this.streamInt(st, st.context, last)
        if (desc.clientStreams && !desc.serverStreams) {
          // Client-streaming: what the chain resolves to is the response,
          // riding the terminal frame (§8 SendAndClose). Nothing is a bug
          // upstream (a handler or an interceptor that forgot to return),
          // not an empty message.
          if (out === undefined) throw statusError(Code.INTERNAL, 'client-streaming handler produced no response')
          st.setResponse(out)
        }
      } catch (e) {
        err = toStatusError(e)
      }
      if (err === undefined && st.signal.aborted) err = abortCause(st.signal)

      const t = await st.terminalFrame(err)
      if (st.suppressTerm) return
      term = t
      void st.transmitTerminal(t).catch(noop)
    } finally {
      this.finish(st, term)
      this.taskDone()
    }
  }

  private finish(st: ServerStream<unknown, unknown>, term: Frame | undefined): void {
    const now = nowMs()
    // Decrement the slot this call was created on (st.slot), not the one the
    // peer key currently maps to — see ServerStream.slot.
    const slot = st.slot
    const ps = st.ps
    if (ps !== undefined) {
      ps.calls.delete(st.sid)
      ps.liveCalls--
      if (!st.reliable) {
        let ttl = this.mode.timing.tombstoneMs
        if (st.deadlineAt !== undefined) {
          // TTL floor: the propagated timeout remainder (§9.2).
          ttl = Math.max(ttl, st.deadlineAt - now)
        }
        if (ps.dead) term = undefined // peer lost: key-only (§10.4)
        ps.addTomb(st.sid, term, now + ttl)
      }
    }
    if (slot !== undefined && slot.liveCalls > 0) slot.liveCalls--
    // Unpark anything still waiting for flow-control credit before the cancel
    // it would otherwise only observe through the abort (§4.2.1).
    st.flowTx.release()
    st.cancel(statusError(Code.CANCELLED, 'call finished'))
    if (st.deadlineTimer !== undefined) {
      clearTimeout(st.deadlineTimer)
      st.deadlineTimer = undefined
    }
    this.kickSweep()
  }

  // ------------------------------------------------------------------
  // RESET paths (PROTOCOL.md §9.3)
  // ------------------------------------------------------------------

  // resetByPeerSid cancels live calls from peer with the given sid. A
  // RESET's peer_epoch names the client incarnation of the offending call
  // (§9.3), so only that incarnation's call dies; 0 (a crafted or foreign
  // RESET) falls back to every epoch with the sid.
  private resetByPeerSid(peer: unknown, sid: number, peerEpoch: number): void {
    const slot = this.slots.get(peer)
    if (slot === undefined) return
    const targets: ServerStream<unknown, unknown>[] = []
    for (const [epoch, ps] of slot.epochs) {
      if (peerEpoch !== 0 && epoch !== peerEpoch) continue
      const st = ps.calls.get(sid)
      if (st !== undefined) targets.push(st)
    }
    const cause = statusError(Code.UNAVAILABLE, 'call reset by peer')
    for (const st of targets) {
      // The peer disowned the call: no terminal is sent and the tombstone is
      // key-only (PROTOCOL.md §9.3).
      st.suppressTerm = true
      st.cancel(cause)
    }
  }

  // sendReset answers a frame with an immediate RESET, rate-limited per call
  // key and per peer on unreliable channels (PROTOCOL.md §9.3, §15).
  private async sendReset(slot: PeerSlot | undefined, f: Frame, ctx: FrameContext): Promise<void> {
    if (!this.rxReliable(ctx)) {
      const n = nowMs()
      const s = slot ?? this.ensureSlot(ctx.peer)
      const k = eksid(f.epoch, f.sid)
      const last = s.resetAt.get(k)
      if (last !== undefined) {
        if (n - last < this.mode.timing.retransmitMs) return
      } else if (this.resetAtTotal >= this.limits.maxPendingResets) {
        // Bounded: drop rather than grow (anti-amplification, §15).
        return
      }
      if (!this.allowReply(s, n)) return
      if (!s.resetAt.has(k)) this.resetAtTotal++
      s.resetAt.set(k, n)
      this.sawUnreliable = true
      this.kickSweep()
    }
    this.protoEvent('reset-sent', ctx.peer, f.sid)
    await this.send(resetFor(f), ctx)
  }

  // allowReply spends one unit of the peer's aggregate reply budget. Denial
  // means silence — anti-amplification prefers dropping a reply over
  // answering a flood (PROTOCOL.md §15).
  private allowReply(slot: PeerSlot, now: number): boolean {
    let b = slot.replyBudget
    if (b === undefined) {
      if (this.replyBudgetTotal >= this.limits.maxPendingResets) {
        // Bounded: deny rather than grow (§15).
        return false
      }
      b = { windowStart: now, n: 0 }
      slot.replyBudget = b
      this.replyBudgetTotal++
    }
    if (now - b.windowStart >= this.mode.timing.retransmitMs) {
      b.windowStart = now
      b.n = 0
    }
    if (b.n >= this.limits.maxRepliesPerRTI) return false
    b.n++
    return true
  }

  // ------------------------------------------------------------------
  // lifecycle
  // ------------------------------------------------------------------

  // gracefulStop refuses new calls and waits for in-flight handlers.
  async gracefulStop(): Promise<void> {
    this.drain = true
    await this.waitIdle()
    this.closed = true
    this.sw.stop()
  }

  // stop cancels every in-flight handler and refuses new calls
  // (PROTOCOL.md §9.4). Idempotent. It resolves when handlers unwound.
  async stop(): Promise<void> {
    this.drain = true
    this.closed = true
    const targets: ServerStream<unknown, unknown>[] = []
    for (const slot of this.slots.values()) {
      for (const ps of slot.epochs.values()) {
        for (const st of ps.calls.values()) targets.push(st)
      }
    }
    const cause = statusError(Code.UNAVAILABLE, 'server stopped')
    for (const st of targets) st.cancel(cause)
    await this.waitIdle()
    this.sw.stop()
  }

  // disconnectPeer fails every live call from peer and releases the peer's
  // containers. Adapters call it when a peer's transport dies (PROTOCOL.md
  // §4.5); for a connection-oriented gateway this IS the teardown that ends
  // the "state until teardown" retention of reliable containers (§9.4,
  // §10.6). Idempotent.
  disconnectPeer(peer: unknown, err?: unknown): void {
    const slot = this.slots.get(peer)
    if (slot === undefined) return
    const targets: ServerStream<unknown, unknown>[] = []
    for (const ps of slot.epochs.values()) {
      for (const st of ps.calls.values()) targets.push(st)
    }
    this.pendingResetTotal -= slot.pendingResets.size
    this.resetAtTotal -= slot.resetAt.size
    if (slot.replyBudget !== undefined) this.replyBudgetTotal--
    this.slots.delete(peer)

    const cause =
      err === undefined || err === null
        ? statusError(Code.UNAVAILABLE, 'transport closed')
        : statusError(Code.UNAVAILABLE, `transport closed: ${err instanceof Error ? err.message : String(err)}`)
    for (const st of targets) st.cancel(cause)
  }

  private waitIdle(): Promise<void> {
    if (this.liveTasks === 0) return Promise.resolve()
    return new Promise((res) => this.idleWaiters.push(res))
  }

  private taskDone(): void {
    this.liveTasks--
    if (this.liveTasks === 0) {
      const ws = this.idleWaiters.splice(0)
      for (const w of ws) w()
    }
  }

  // ------------------------------------------------------------------
  // shared internals
  // ------------------------------------------------------------------

  /** @internal */
  get timing() {
    return this.mode.timing
  }

  /** @internal */
  send(f: Frame, ctx: FrameContext): Promise<void> {
    return Promise.resolve(this.tx.handle(f, ctx)).catch(noop)
  }

  // sendOrThrow propagates adapter refusals (backpressure bounds, size
  // limits) to the sending stream, which maps them onto the owning call
  // (PROTOCOL.md §4.4).
  /** @internal */
  sendOrThrow(f: Frame, ctx: FrameContext): Promise<void> {
    return Promise.resolve(this.tx.handle(f, ctx))
  }

  /** @internal */
  txFor(peer: unknown): FrameContext {
    return peer === undefined ? {} : { peer }
  }

  /** @internal */
  allowReplyFor(peer: unknown, now: number): boolean {
    const slot = this.slots.get(peer)
    if (slot === undefined) return false
    return this.allowReply(slot, now)
  }

  private ensureSlot(peer: unknown): PeerSlot {
    let slot = this.slots.get(peer)
    if (slot === undefined) {
      slot = new PeerSlot(peer)
      this.slots.set(peer, slot)
    }
    return slot
  }

  // ensurePeer returns the container for (peer, epoch), creating it and
  // enforcing the per-peer container cap (never evicting containers with
  // live calls, PROTOCOL.md §15). reliable applies on creation only: the
  // mode is a property of the peer's channel and cannot change (§4.3) — an
  // existing container keeps its first-captured value.
  private ensurePeer(slot: PeerSlot, epoch: number, now: number, reliable: boolean): PeerState {
    let ps = slot.epochs.get(epoch)
    if (ps !== undefined) return ps

    // Cap dead containers of this transport peer.
    const dead: PeerState[] = []
    for (const p of slot.epochs.values()) {
      if (p.liveCalls === 0) dead.push(p)
    }
    if (dead.length >= this.limits.maxDeadPeers) {
      let oldest = dead[0]!
      for (const p of dead) {
        if (p.createdAt < oldest.createdAt) oldest = p
      }
      slot.epochs.delete(oldest.epoch)
    }

    ps = new PeerState(slot.peer, epoch, reliable, now, this.limits.maxTombstones, this.limits.maxTombstoneBytes)
    slot.epochs.set(epoch, ps)
    if (!reliable) this.sawUnreliable = true
    return ps
  }

  /** @internal */
  kickSweep(): void {
    if (!this.sawUnreliable) {
      // Only unreliable-mode state needs timers; a server that has seen none
      // runs no sweeper at all.
      return
    }
    this.sw.kick(
      this.mode.timing.tickMs,
      () => this.sweep(nowMs()),
      () => this.hasWork(),
    )
  }

  private hasWork(): boolean {
    if (this.pendingResetTotal > 0 || this.resetAtTotal > 0 || this.replyBudgetTotal > 0) return true
    for (const slot of this.slots.values()) {
      for (const ps of slot.epochs.values()) {
        // Reliable containers are not swept (no timers, no GC): only an
        // unreliable one keeps the sweeper alive.
        if (!ps.reliable) return true
      }
    }
    return false
  }

  private sweep(now: number): void {
    const t = this.mode.timing
    const jobs: { f: Frame; ctx: FrameContext }[] = []
    const lost: ServerStream<unknown, unknown>[] = []

    for (const [peerKey, slot] of this.slots) {
      // Delayed RESETs: fire if the call is still unknown (§9.3).
      for (const [k, pr] of slot.pendingResets) {
        if (now < pr.due) continue
        const ps = slot.epochs.get(pr.epoch)
        if (ps !== undefined && (ps.calls.has(pr.sid) || ps.tombs.has(pr.sid))) {
          slot.pendingResets.delete(k)
          this.pendingResetTotal--
          continue
        }
        if (!this.allowReply(slot, now)) {
          // Aggregate reply budget spent (§15): keep the entry — the next
          // sweep retries once the budget window turns over, so the RESET is
          // deferred, not lost.
          continue
        }
        slot.pendingResets.delete(k)
        this.pendingResetTotal--
        this.protoEvent('reset-sent', slot.peer, pr.sid)
        jobs.push({
          f: frame({ flags: FlagReset, epoch: pr.echo, peerEpoch: pr.peerEcho, sid: pr.sid }),
          ctx: this.txFor(slot.peer),
        })
      }

      // Prune the immediate-RESET rate-limit history and reply budgets.
      for (const [k, at] of slot.resetAt) {
        if (now - at > t.tombstoneMs) {
          slot.resetAt.delete(k)
          this.resetAtTotal--
        }
      }
      if (slot.replyBudget !== undefined && now - slot.replyBudget.windowStart > t.tombstoneMs) {
        slot.replyBudget = undefined
        this.replyBudgetTotal--
      }

      // Containers: checkpoints, tombstone expiry, liveness, keepalive, GC.
      for (const [epoch, ps] of slot.epochs) {
        if (ps.reliable) {
          // A reliable peer runs no timers (PROTOCOL.md §10.6): no liveness,
          // no PING, no tombstones to expire, no aging (its watermark is
          // plain hwm), and no GC — state lives until teardown
          // (disconnectPeer/stop).
          continue
        }
        ps.cps.push({ at: now, hwm: ps.hwm })
        while (ps.cps.length > 1 && now - ps.cps[1]!.at >= t.tombstoneMs) {
          // Keep exactly one checkpoint older than TTL: it defines hwm_aged.
          ps.cps.shift()
        }

        const aged = ps.hwmAged(now, t.tombstoneMs)
        for (const [sid, tb] of ps.tombs) {
          // Expiry is coupled to the aged watermark (§9.2): a tombstone dies
          // only once hwm_aged covers its sid. Plain compare (§6.2).
          if (now > tb.expireAt && sid <= aged) ps.removeTomb(sid)
        }
        if (ps.tombOrder.length > 2 * ps.tombs.size + 16) {
          // Compact the eviction order of expired entries.
          ps.tombOrder = ps.tombOrder.filter((sid) => ps.tombs.has(sid))
        }

        if (ps.liveCalls > 0 && !ps.dead) {
          if (now - ps.lastRx >= t.livenessMs) {
            // Peer lost (§10.4): cancel its calls, degrade tombstones.
            ps.dead = true
            this.protoEvent('liveness-expired', slot.peer)
            for (const st of ps.calls.values()) {
              st.suppressTerm = true
              lost.push(st)
            }
            for (const tb of ps.tombs.values()) {
              ps.tombBytes -= tb.size
              tb.term = undefined
              tb.size = 0
            }
          } else if (now - ps.lastTx >= t.probeMs && now - ps.lastPing >= t.probeMs) {
            ps.lastPing = now
            ps.lastTx = now
            this.protoEvent('keepalive-sent', slot.peer)
            jobs.push({
              f: frame({ epoch: this.epoch, flags: FlagPing, peerEpoch: epoch }), // name the incarnation (§6.1)
              ctx: this.txFor(slot.peer),
            })
          }
        }

        // Containers outlive their tombstones (retention ≥ TTL_tomb after
        // the last activity, §9.4): the aged watermark must still be there
        // to reject stale OPENs once the tombstones are gone.
        if (ps.liveCalls === 0 && ps.tombs.size === 0 && now - ps.lastRx > 2 * t.tombstoneMs) {
          slot.epochs.delete(epoch)
        }
      }

      // Stream probes (§10.5). Calls on reliable channels are not probed —
      // gated on the call's own mode, as Go does, not the container's.
      for (const ps of slot.epochs.values()) {
        for (const st of ps.calls.values()) {
          if (st.reliable) continue
          const f = st.probeDue(now, t.probeMs, this.epoch)
          if (f !== undefined) {
            st.protoEvent('probe-sent')
            ps.lastTx = now
            jobs.push({ f, ctx: this.txFor(slot.peer) })
          }
        }
      }

      if (
        slot.epochs.size === 0 &&
        slot.liveCalls === 0 &&
        slot.pendingResets.size === 0 &&
        slot.resetAt.size === 0 &&
        slot.replyBudget === undefined
      ) {
        this.slots.delete(peerKey)
      }
    }

    const cause = statusError(Code.UNAVAILABLE, 'peer lost')
    for (const st of lost) st.cancel(cause)
    for (const j of jobs) void this.send(j.f, j.ctx)
  }
}

// ---------------------------------------------------------------------------
// server stream
// ---------------------------------------------------------------------------

class ServerStream<Req, Res> implements ServerReader<Req>, ServerWriter<Res> {
  readonly context: ServerContext

  /** @internal */ ps: PeerState | undefined
  // The PeerSlot this call was created on. finish() decrements ITS liveCalls,
  // not whatever slot the peer key currently maps to: a disconnectPeer can
  // delete the slot and a reused key can install a fresh one before this call
  // unwinds, and decrementing the new slot (never incremented for this call)
  // would under-count and under-enforce the §15 per-peer cap. Go is immune
  // because its flat livePeer map survives DisconnectPeer (server.go).
  /** @internal */ slot: PeerSlot | undefined
  /** @internal */ deadlineAt: number | undefined
  /** @internal */ deadlineTimer: ReturnType<typeof setTimeout> | undefined
  /** @internal */ metadata: Metadata | undefined

  // suppressTerm: the peer disowned the call (RESET) or vanished (liveness
  // expiry) — no terminal is sent, the tombstone is key-only.
  /** @internal */ suppressTerm = false

  // Per-stream flow control (reliable mode, §4.2.1): flowTx is credit for
  // what this side sends, flowRx accounts what the handler consumed.
  /** @internal */ readonly flowTx = new FlowSender()
  private readonly flowRx = new FlowReceiver()

  // tx state.
  private readonly txSeq = new TxSeq()
  /** @internal */ txHeader: Metadata | undefined // set via setHeader/sendHeader
  private hdrSent = false // header MD already rode some frame
  private hdrFlushed = false // the HANDLER flushed a header (sendHeader), §11
  private hdrFrame: Frame | undefined // stored creation ack for byte-identical replay (§8)
  /** @internal */ trailerMd: Metadata | undefined
  private resp: Uint8Array | undefined // captured client-streaming response payload
  private respSet = false

  // Idle clocks and ack-replay limiter (unreliable, PROTOCOL.md §10.5, §8).
  private lastRx: number
  private lastTx: number
  private lastProbe = 0
  private hReplayAt = 0

  // rx sequencing. The server enforces incarnation isolation structurally —
  // calls are keyed by (peer, epoch, sid) in the demux — so no per-stream
  // epoch gate here.
  private readonly rxWin = new RxWindow()
  /** @internal */ readonly rxq: FrameQueue
  private readonly rxCfg: ResolvedRxConfig
  /** @internal */ rxDropped = 0
  /** @internal */ readonly rxEOF = new Latch()

  private readonly ctrl = new AbortController()
  private readonly endLatch = new Latch()

  /** @internal */
  constructor(
    private readonly server: Server,
    readonly peer: unknown,
    readonly clientEpoch: number,
    readonly sid: number,
    /** @internal */ readonly reg: Registration,
    codec: { request: PayloadCodec<unknown>; response: PayloadCodec<unknown> },
    rxCfg: ResolvedRxConfig,
    // The mode of the channel this call arrived on (PROTOCOL.md §4.3): it
    // selects strict sequencing and gates the probe/tombstone machinery of
    // the peer-mixed server.
    /** @internal */ readonly reliable: boolean,
    // The call's message compressor, named on the OPEN; undefined = none
    // (§12.1). Like the codec it governs both directions.
    private readonly comp: Compressor | undefined,
    private readonly maxRecv: number,
    private readonly maxSend: number,
    private readonly stallMs: number,
  ) {
    this.reqCodec = codec.request
    this.resCodec = codec.response
    this.rxCfg = rxCfg
    this.rxq = new FrameQueue(rxCfg.size)
    this.rxWin.l = 1 // the accepted OPEN
    this.rxWin.strict = reliable
    if (reliable) {
      // Advertise this side's buffer as the client's initial send window; it
      // rides the creation ack H (§4.2.1).
      this.flowRx.enable(rxCfg.size)
    }
    const n = nowMs()
    this.lastRx = n
    this.lastTx = n

    const self = this
    this.context = {
      signal: this.ctrl.signal,
      get metadata() {
        return self.metadata
      },
      peer,
      method: reg.desc.path,
      desc: reg.desc,
      get deadline() {
        return self.deadlineAt
      },
      setHeader: (md) => this.setHeader(md),
      sendHeader: (md) => this.sendHeader(md),
      setTrailer: (md) => {
        if (Object.keys(md).length === 0) return
        try {
          validateMetadata(md)
        } catch {
          // Invalid metadata is dropped, as grpc-go does (the signature has
          // no error to return) — validating here is what keeps it from
          // failing the terminal frame's encode (§11). Reported as off-shape,
          // the one drop of this kind Go reports too.
          this.protoEvent('off-shape', 1)
          return
        }
        this.trailerMd = metadataJoin(this.trailerMd, md)
      },
    }
  }

  /** @internal */ readonly reqCodec: PayloadCodec<unknown>
  /** @internal */ readonly resCodec: PayloadCodec<unknown>

  get signal(): AbortSignal {
    return this.ctrl.signal
  }

  /** @internal */
  cancel(cause: StatusError): void {
    this.ctrl.abort(cause) // first abort wins, like context.CancelCause
    this.endLatch.trip()
  }

  // ------------------------------------------------------------------
  // receive path
  // ------------------------------------------------------------------

  // handleRx processes one client frame for this live call. Called by
  // Server.handle. In reliable mode it may block on a full buffer, bounded
  // by the rx signal (PROTOCOL.md §4.2).

  // protoEvent reports one protocol event for this call (stats.ts): the
  // transport peer it arrived from, its sid, and its method. /** @internal */:
  // the Server's sweep reports the probes it sends on the stream's behalf.
  /** @internal */
  protoEvent(kind: ProtocolEventKind, count = 0): void {
    const sink = this.server.pstats
    if (sink.length === 0) return
    emit(sink, { kind, peer: this.peer, sid: this.sid, method: this.reg.desc.path, count })
  }

  /** @internal */
  async handleRx(f: Frame, ctx: FrameContext): Promise<void> {
    if (hasUnknownFlags(f) || !legalShape(shapeOf(f))) {
      // See the client twin: a modifier bit from a newer peer changes
      // something about this frame that we cannot honor, and an illegal shape
      // combination is not a frame we can route — delivering either would be
      // a silent corruption, dropping it a silent gap (§7.1, §8).
      this.cancel(statusError(Code.INTERNAL, `drpc: frame carries unsupported flags 0x${f.flags.toString(16)}`))
      return
    }
    if (shapeOf(f) === FlagWindow) {
      // Stateless flow-control grant: no seq, no delivery, and only where
      // flow control exists at all — a stray or injected WINDOW must never
      // park an unreliable-mode sender (§4.2.1, §7, §15).
      if (this.reliable) this.flowTx.grant(windowOf(f))
      return
    }
    if (isOpen(f)) {
      if (f.seq !== 1) {
        // Off-shape: an OPEN's seq MUST be 1 (PROTOCOL.md §8).
        this.rxDropped++
        return
      }
      if (this.reliable) {
        // No retransmission exists in reliable mode, so a duplicate OPEN
        // means the transport duplicated a frame: fail loud (§10.6).
        this.cancel(statusError(Code.INTERNAL, 'reliable transport lost or reordered a frame'))
        return
      }
      // Duplicate OPEN (its seq 1 is always a dedup). For streaming calls it
      // re-elicits the creation ack (PROTOCOL.md §8 ack recovery); unary owes
      // one only if its handler flushed a header, which the client's Header()
      // is now blocked on (§8, §11).
      this.noteValidatedRx()
      if (!isUnary(this.reg.desc) || this.hdrFrame !== undefined) this.replayH()
      return
    }

    const v = this.rxWin.check(f.seq)
    switch (v) {
      case RxVerdict.Dup:
        this.noteValidatedRx()
        return
      case RxVerdict.Beyond:
        return
      case RxVerdict.DataLoss:
        this.protoEvent('data-loss')
        this.cancel(statusError(Code.DATA_LOSS, 'seq window overrun: >W_fwd consecutive frames lost'))
        return
      case RxVerdict.ProtocolError:
        this.cancel(statusError(Code.INTERNAL, 'reliable transport lost or reordered a frame'))
        return
      case RxVerdict.Accept:
        break
    }
    const gap = this.rxWin.takeGap()
    if (gap > 0) this.protoEvent('skipped', gap) // §14
    this.noteValidatedRx()

    if (isTerminal(f)) {
      // Client abort: cancel the handler; the terminal T is produced as it
      // unwinds (PROTOCOL.md §10.3).
      this.cancel(frameStatus(f))
    } else if (isHalfClose(f)) {
      this.rxEOF.trip()
    } else if (isData(f)) {
      if (isUnary(this.reg.desc) || !this.reg.desc.clientStreams) {
        this.rxDropped++
        return
      }
      if (this.reliable) {
        if (this.flowRx.active) {
          // A conforming peer never exceeds the window it was granted, so a
          // full buffer here is a contract violation — and blocking on it
          // would be the deadlock flow control exists to remove: the grant
          // that would unpark the peer travels the very read loop this would
          // stall (§4.2, §4.2.1).
          if (!this.rxq.tryPut(f)) {
            this.cancel(statusError(Code.INTERNAL, 'drpc: peer exceeded the advertised flow-control window'))
          }
        } else if (!(await this.rxq.putBlocking(f, this.endLatch, ctx.signal))) {
          // See the client twin: teardown ate the frame — fail loud rather
          // than leave a silent gap on a reliable channel (§14).
          this.rxDropped++
          this.cancel(statusError(Code.UNAVAILABLE, 'transport closed during delivery'))
        }
      } else if (this.rxq.putDrop(f, this.rxCfg.policy) > 0) {
        this.protoEvent('dropped', 1)
      }
    } else {
      this.rxDropped++
    }
  }

  // noteValidatedRx runs for every validated client frame of this stream
  // (accepted or dedup-dropped, PROTOCOL.md §9.1): refresh the idle clocks.
  private noteValidatedRx(): void {
    const n = nowMs()
    this.lastRx = n
    if (this.ps !== undefined) this.ps.lastRx = n
  }

  async recv(): Promise<Req | undefined> {
    for (;;) {
      const f = this.rxq.tryTake()
      if (f !== undefined) {
        // A failed delivery ends the call; granting then would only draw a
        // RESET for a sid the peer has already forgotten (§4.2.1).
        const msg = (await this.decodeMessage(f)) as Req
        this.grantWindow(1)
        return msg
      }
      if (this.rxEOF.tripped) return undefined
      if (this.ctrl.signal.aborted) throw abortCause(this.ctrl.signal)
      await Promise.race([this.rxq.readable(), this.rxEOF.wait(), this.endLatch.wait()])
    }
  }

  async *[Symbol.asyncIterator](): AsyncIterator<Req> {
    for (;;) {
      const m = await this.recv()
      if (m === undefined) return
      yield m
    }
  }

  // ------------------------------------------------------------------
  // send path
  // ------------------------------------------------------------------

  // transmit sends a non-probe frame, feeding the tx idle clocks.
  /** @internal */
  transmit(f: Frame): Promise<void> {
    const n = nowMs()
    this.lastTx = n
    if (this.ps !== undefined) this.ps.lastTx = n
    return this.server.send(f, this.server.txFor(this.peer))
  }

  private nextFrame(): Frame {
    const f = frame({ epoch: this.server.epoch, sid: this.sid, seq: this.txSeq.next() })
    // Name the client incarnation (PROTOCOL.md §6.1): a restarted client
    // re-allocates sids, so the sid alone must never route this frame there.
    f.peerEpoch = this.clientEpoch
    return f
  }

  // attachHeader piggybacks the pending header MD once (PROTOCOL.md §11).
  // force makes the field present even when the handler set no metadata: an
  // explicit sendHeader must unblock the client's header(), while a creation
  // ack must not pin the header to empty (§8).
  private attachHeader(f: Frame, force: boolean): void {
    if (this.hdrSent) return
    if (this.txHeader !== undefined) {
      f.header = this.txHeader
      this.hdrSent = true
      return
    }
    if (force) {
      f.header = {}
      this.hdrSent = true
    }
  }

  // sendH emits the creation-ack header frame (PROTOCOL.md §8). The header
  // field is present only if the handler already set one — the core's own ack
  // is not a flush (§11). The first H is stored for byte-identical replay,
  // and in reliable mode it advertises this side's receive window (§4.2.1).
  /** @internal */
  sendH(): void {
    const f = this.nextFrame()
    if (this.reliable) f.window = this.rxCfg.size
    this.attachHeader(f, false)
    if (this.hdrFrame === undefined) this.hdrFrame = f
    void this.transmit(f).catch(noop)
  }

  // grantWindow reports messages the handler consumed and sends the resulting
  // credit (§4.2.1). Called only for buffered data frames — a terminal
  // payload never occupied a buffer slot. A WINDOW frame is stateless: it
  // carries no seq and burns none.
  private grantWindow(n: number): void {
    if (this.ctrl.signal.aborted) return // the call is over; a grant would draw a RESET
    const g = this.flowRx.consumed(n)
    if (g === 0) return
    const f = frame({ epoch: this.server.epoch, sid: this.sid, peerEpoch: this.clientEpoch, flags: FlagWindow, window: g })
    void this.transmit(f).catch(noop)
  }

  // replayH answers a duplicate OPEN with the creation ack, rate-limited to
  // one per RTI per call plus the peer's aggregate reply budget (PROTOCOL.md
  // §8 ack recovery, §15): the stored H replayed byte-identically, else a
  // freshly-seq'd H with the current header state.
  private replayH(): void {
    const n = nowMs()
    if (n - this.hReplayAt < this.server.timing.retransmitMs) return
    this.hReplayAt = n
    if (!this.server.allowReplyFor(this.peer, n)) return
    let f = this.hdrFrame
    if (f === undefined) {
      f = this.nextFrame()
      if (this.reliable) f.window = this.rxCfg.size
      this.attachHeader(f, false)
    }
    void this.transmit(f).catch(noop)
  }

  // probeDue emits a stream probe when both idle clocks passed T_probe
  // (PROTOCOL.md §10.5). Probes reset neither idle clock.
  /** @internal */
  probeDue(now: number, probeMs: number, epoch: number): Frame | undefined {
    if (now - this.lastRx < probeMs || now - this.lastTx < probeMs || now - this.lastProbe < probeMs) {
      return undefined
    }
    this.lastProbe = now
    return frame({ epoch, sid: this.sid, flags: FlagPing, peerEpoch: this.clientEpoch })
  }

  // setHeader joins metadata into the pending header. It throws once the
  // header is already on the wire — joining more would silently lose it
  // (§11 first-wins), which is grpc-go's ErrIllegalHeaderWrite.
  /** @internal */
  setHeader(md: Metadata): void {
    if (Object.keys(md).length === 0) return
    validateMetadata(md) // StatusError{INTERNAL}: the call fails locally (§11)
    if (this.hdrFlushed || this.hdrSent) throw illegalHeaderWrite()
    this.txHeader = metadataJoin(this.txHeader, md)
  }

  // sendHeader flushes the header metadata as an H frame at once — including
  // on unary calls, so a client's header() returns before the response
  // exists, as it does on gRPC (PROTOCOL.md §8, §11). Calling it twice is an
  // error, as in grpc-go; the core's own creation ack is not a flush.
  /** @internal */
  async sendHeader(md?: Metadata): Promise<void> {
    if (md !== undefined) validateMetadata(md)
    if (this.hdrFlushed || this.hdrSent) {
      // Flushed twice, or after the header already rode a data frame:
      // refusing is what keeps the metadata from being silently dropped.
      throw illegalHeaderWrite()
    }
    this.hdrFlushed = true
    if (md !== undefined) this.txHeader = metadataJoin(this.txHeader, md)
    const f = this.nextFrame()
    this.attachHeader(f, true)
    if (this.hdrFrame === undefined) {
      // Keep it for byte-identical replay: on a unary call this H is the only
      // thing a client's header() is waiting for (§8 ack recovery).
      this.hdrFrame = f
    }
    try {
      await this.transmitOrThrow(f)
    } catch (e) {
      this.undoRefused(f, e)
      throw toStatusError(e)
    }
  }

  async send(msg: Res): Promise<void> {
    const raw = this.resCodec.marshal(msg)
    if (!this.reg.desc.serverStreams) {
      throw statusError(Code.INTERNAL, 'send on a non-server-streaming call')
    }

    // Flow control (§4.2.1): park until the client has room, instead of
    // letting its full buffer stall every call on the channel. The park is
    // bounded by the call ending and by T_stall — nothing else could break it
    // in reliable mode.
    if (!this.flowTx.tryAcquire()) {
      this.protoEvent('flow-stall')
      switch (await this.flowTx.acquire(this.endLatch, this.stallMs)) {
        case 'ok':
          this.protoEvent('flow-resume')
          break
        case 'stalled':
          throw statusError(Code.UNAVAILABLE, `drpc: flow-control stall: the peer granted no credit for ${this.stallMs}ms`)
        default:
          // The call ended underneath the sender: report the status that
          // ended it, as grpc-go does.
          throw abortCause(this.ctrl.signal)
      }
    }

    // Compress BEFORE the frame exists. Compression is asynchronous here (the
    // platform's CompressionStream is a stream), and an await between a seq
    // allocation and the transmit would let a racing send put ITS frame on
    // the wire first — a gap the client's strict window would fail the call
    // on. Everything from nextFrame() to transmitOrThrow() is synchronous.
    let enc: WirePayload
    try {
      enc = await this.encodePayload(raw)
    } catch (e) {
      this.flowTx.undo() // the message never reached the wire (§4.2.1)
      throw e
    }

    if (this.ctrl.signal.aborted) {
      // grpc-go parity: report the status describing why the stream ended.
      throw abortCause(this.ctrl.signal)
    }
    const f = this.nextFrame()
    f.payload = enc.bytes
    if (enc.compressed) f.flags |= FlagCompressed
    this.attachHeader(f, false)
    try {
      await this.transmitOrThrow(f)
    } catch (e) {
      // A synchronous adapter refusal reclaims the seq so the terminal
      // carrying the handler's real status stays gap-free (see TxSeq.undo).
      this.undoRefused(f, e)
      throw e
    }
  }

  private transmitOrThrow(f: Frame): Promise<void> {
    const n = nowMs()
    this.lastTx = n
    if (this.ps !== undefined) this.ps.lastTx = n
    return this.server.sendOrThrow(f, this.server.txFor(this.peer))
  }

  private undoRefused(f: Frame, err: unknown): void {
    if (!isMessageTooLarge(err)) return
    this.txSeq.undo(f.seq)
    // The message never reached the wire, so its credit was never spent:
    // without the refund a handler that ignores the error leaks its whole
    // window and parks forever (§4.2.1, §4.4).
    if (isData(f)) this.flowTx.undo()
  }

  // ------------------------------------------------------------------
  // payload codec: compression + size caps (PROTOCOL.md §12.1)
  // ------------------------------------------------------------------

  // encodePayload prepares one outgoing message: compressed when the call has
  // a compressor and it actually shrinks the bytes (never on an empty
  // payload, §5, §7), then capped on the bytes that reach the wire — i.e.
  // after compression, as grpc-go caps them (§12.1, §16).
  private async encodePayload(payload: Uint8Array): Promise<WirePayload> {
    const enc = this.comp === undefined ? rawPayload(payload) : await compressPayload(this.comp, payload)
    checkSendSize(enc.bytes.length, this.maxSend)
    return enc
  }

  // decodeMessage returns the message a received frame carries: decompressed
  // if marked — bounded by the receive cap, so a decompression bomb cannot
  // allocate without limit — then capped on the DECOMPRESSED message and
  // unmarshaled with the call's codec (§12.1, §16).
  /** @internal */
  async decodeMessage(f: Frame): Promise<unknown> {
    const raw = f.payload ?? EMPTY
    const payload = isCompressed(f) ? await decompressPayload(this.comp, raw, this.maxRecv) : raw
    checkRecvSize(payload.length, this.maxRecv)
    return this.reqCodec.unmarshal(payload)
  }

  // setResponse captures the unary / client-streaming response; it rides the
  // terminal frame (PROTOCOL.md §8). Compression and the send cap are applied
  // when that frame is built.
  /** @internal */
  setResponse(resp: unknown): void {
    if (this.respSet) throw statusError(Code.INTERNAL, 'response already set')
    this.resp = this.resCodec.marshal(resp)
    this.respSet = true
  }

  // terminalFrame builds T after the handler returned (PROTOCOL.md §8).
  // T re-carries the header MD once set so it survives first-frame loss.
  /** @internal */
  async terminalFrame(err: StatusError | undefined): Promise<Frame> {
    // The response payload is encoded BEFORE the frame exists, for the reason
    // send() spells out: compression is asynchronous, and a seq allocated
    // across that await could be overtaken by a still-running send.
    let enc: WirePayload | undefined
    let st = err
    if (st === undefined && this.respSet) {
      try {
        enc = await this.encodePayload(this.resp ?? EMPTY)
      } catch (e) {
        // Oversize or uncompressible: the response cannot be delivered, so
        // the call ends with that status instead of a truncated message.
        st = toStatusError(e)
      }
    }

    const f = this.nextFrame()
    f.flags = FlagClose
    if (this.txHeader !== undefined) {
      f.header = this.txHeader
      this.hdrSent = true
    }
    if (this.trailerMd !== undefined) f.trailer = this.trailerMd
    if (st !== undefined) {
      setFrameError(f, st)
      // Rich google.rpc.Status details travel too (§5). They are a passenger:
      // a terminal the channel cannot carry is re-sent without them
      // (transmitTerminal).
      f.details = detailsOf(st)
      return f
    }
    if (enc !== undefined) {
      f.payload = enc.bytes
      if (enc.compressed) f.flags |= FlagCompressed
    }
    f.code = Code.OK
    return f
  }

  // transmitTerminal sends the call's terminal frame, shrinking it until the
  // channel accepts it. Every termination path depends on this frame arriving
  // (§10.7), so its passengers are shed in order of expendability: first the
  // status details (§5), then the response payload — which cannot be
  // delivered anyway, so the call ends with RESOURCE_EXHAUSTED rather than
  // with silence. The frame is mutated in place and keeps its seq: a refused
  // frame never reached the wire, so no sequence number is burned (§4.4) and
  // what the tombstone stores for replay is exactly what was sent (§9.2).
  /** @internal */
  async transmitTerminal(f: Frame): Promise<void> {
    let err = await this.tryTransmit(f)
    if (err === undefined) return
    if (f.details !== undefined && f.details.length > 0) {
      f.details = undefined
      const e2 = await this.tryTransmit(f)
      if (e2 === undefined) return
      err = e2
    }
    if (f.payload === undefined && f.code === Code.RESOURCE_EXHAUSTED) {
      return // already minimal; the channel simply cannot carry it
    }
    f.payload = undefined
    f.flags &= ~FlagCompressed
    f.details = undefined
    setFrameError(f, statusError(Code.RESOURCE_EXHAUSTED, `drpc: the terminal frame does not fit the transport: ${errText(err)}`))
    await this.tryTransmit(f)
  }

  // tryTransmit sends f and returns the adapter's size refusal, if that is
  // what it was. Any other failure is the adapter's own business (§4.2) and
  // is swallowed: retrying a smaller frame would not help.
  private async tryTransmit(f: Frame): Promise<unknown> {
    try {
      await this.transmitOrThrow(f)
      return undefined
    } catch (e) {
      return isMessageTooLarge(e) ? e : undefined
    }
  }
}

// illegalHeaderWrite mirrors grpc-go's transport.ErrIllegalHeaderWrite: the
// header is already on the wire, so anything more would be silently lost.
function illegalHeaderWrite(): StatusError {
  return statusError(Code.INTERNAL, 'drpc: sendHeader called multiple times')
}
