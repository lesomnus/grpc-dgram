// The client endpoint: Conn and its streams, with the client half of the
// unreliable-mode machinery — control-frame retransmission, peer liveness,
// stream probes, and client tombstones (PROTOCOL.md §8–§10).
//
// Concurrency note: the Go original guards state with mutexes; here every
// state transition runs synchronously between await points, which gives the
// same atomicity for free. The places Go re-checks under a lock after a
// blocking region are marked where the translation still needs them.

import type { CallOptions, ForcedCodec, MethodDesc, PayloadCodec } from './desc'
import { DropPolicy, resolveLimits, resolveRxConfig, type Limits, type ResolvedLimits, type ResolvedRxConfig, type RxBufferConfig } from './limits'
import type { Metadata } from './metadata'
import { RxVerdict, RxWindow, TxSeq } from './seq'
import { abortCause, Code, isMessageTooLarge, StatusError, statusError, toStatusError } from './status'
import { resolveTiming, type Mode, type Timing } from './timing'
import { hasConnAttacher, hasTransportInfo, type FrameContext, type FrameHandler } from './transport'
import { abortListener, FrameQueue, Latch, nonzeroEpoch, noop, nowMs, Sweeper, unrefTimer } from './util'
import { FlagClose, FlagOpen, FlagPing, frame, frameStatus, isData, isHeaderFrame, isPing, isReset, isTerminal, resetFor, type Frame } from './wire'

// EndOfStreamError is thrown by send when the call already ended (a racing
// abort, terminal, or teardown) — the io.EOF of the grpc-go contract. The
// call's actual status surfaces via recv.
export class EndOfStreamError extends Error {
  constructor() {
    super('stream ended')
    this.name = 'EndOfStreamError'
  }
}

export interface ConnOptions {
  // Overrides transport discovery (PROTOCOL.md §4.3).
  reliable?: boolean
  // Protocol timers (unreliable mode only, §10.1).
  timing?: Timing
  // Per-stream rx buffer size and drop policy (§4.2).
  rxBuffer?: RxBufferConfig
  // Resource caps (§15); only maxPendingResets applies to a Conn.
  limits?: Limits
  defaultCallOptions?: CallOptions
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
  private readonly defaults: CallOptions

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

  constructor(tx: FrameHandler, opts: ConnOptions = {}) {
    this.epoch = nonzeroEpoch()
    this.tx = tx
    this.mode = {
      reliable: opts.reliable ?? (hasTransportInfo(tx) ? tx.reliable() : false),
      timing: resolveTiming(opts.timing),
    }
    this.rxCfg = resolveRxConfig(opts.rxBuffer)
    this.limits = resolveLimits(opts.limits)
    this.defaults = opts.defaultCallOptions ?? {}

    // Last, with the Conn fully usable: the transport may start delivering
    // frames from inside attachConn.
    if (hasConnAttacher(tx)) tx.attachConn(this)
  }

  get reliable(): boolean {
    return this.mode.reliable
  }

  // handle delivers one server frame to this Conn. Adapters call it for each
  // frame of a received envelop, in order, awaiting each (PROTOCOL.md §9.1).
  async handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    const sid = f.sid

    if (isReset(f)) {
      // Act only if the echoed epoch is ours; RESET never refreshes
      // liveness (PROTOCOL.md §9.1, §9.3).
      if (f.epoch !== this.epoch) return
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

  // invoke performs a unary call (grpc-go Invoke parity).
  async invoke<Req, Res>(desc: MethodDesc<Req, Res>, req: Req, opts: CallOptions<Req, Res> = {}): Promise<Res> {
    const merged: CallOptions<Req, Res> = { ...(this.defaults as CallOptions<Req, Res>), ...opts }
    let timeoutCause: StatusError | undefined
    if (!this.mode.reliable && merged.timeoutMs === undefined) {
      // T_call: the default unary deadline (PROTOCOL.md §10.2).
      merged.timeoutMs = this.mode.timing.callMs
      timeoutCause = statusError(Code.DEADLINE_EXCEEDED, 'drpc: default call timeout')
    }

    const s = this.createStream(desc, merged, timeoutCause)
    let err: StatusError | undefined
    let out: Res | undefined
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
    return out as Res
  }

  // newStream starts a streaming call. For client-streaming and bidi the
  // eager OPEN is sent at stream creation, so the server can start the
  // handler and push even if the client never sends (PROTOCOL.md §8).
  newStream<Req, Res>(desc: MethodDesc<Req, Res>, opts: CallOptions<Req, Res> = {}): ClientStream<Req, Res> {
    const merged: CallOptions<Req, Res> = { ...(this.defaults as CallOptions<Req, Res>), ...opts }
    const s = this.createStream(desc, merged, undefined)
    if (desc.clientStreams) void s.sendOpen()
    return s
  }

  private createStream<Req, Res>(desc: MethodDesc<Req, Res>, opts: CallOptions<Req, Res>, timeoutCause: StatusError | undefined): ClientStream<Req, Res> {
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
    const s = new ClientStream<Req, Res>(this, desc, this.sidNext, opts, timeoutCause)
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
        this.failAll(statusError(Code.UNAVAILABLE, 'peer lost'))
        return
      }
      if (now - this.lastTx >= t.probeMs && now - this.lastPing >= t.probeMs) {
        this.lastPing = now
        this.lastTx = now
        void Promise.resolve(this.tx.handle(frame({ epoch: this.epoch, flags: FlagPing }))).catch(noop)
      }
    }

    // Per-stream retransmissions and probes.
    for (const s of streams) {
      for (const f of s.sweepRetx(now, t.probeMs)) {
        void s.transmit(f).catch(noop)
      }
      const p = s.probeDue(now, t.probeMs)
      if (p !== undefined) {
        // Probes feed the peer-keepalive cadence but not the stream's own
        // idle clocks (PROTOCOL.md §10.5).
        this.lastTx = now
        void Promise.resolve(this.tx.handle(p)).catch(noop)
      }
    }
    // Tombstoned aborts.
    for (const f of tombRetx) {
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
  constructor(conn: Conn, desc: MethodDesc<Req, Res>, sid: number, opts: CallOptions<Req, Res>, timeoutCause: StatusError | undefined) {
    this.conn = conn
    this.desc = desc
    this.sid = sid
    const forced: ForcedCodec<Req, Res> | undefined = opts.codec
    this.reqCodec = forced?.request ?? desc.request
    this.resCodec = forced?.response ?? desc.response
    this.codecName = forced?.name ?? ''
    this.openHdr = opts.metadata
    this.callerSignal = opts.signal
    this.timeoutCause = timeoutCause
    if (opts.timeoutMs !== undefined) this.deadlineAt = nowMs() + opts.timeoutMs

    const cfg = conn.streamRxCfg
    this.rxq = new FrameQueue(cfg.size)
    this.rxPolicy = cfg.policy
    this.rxWin.strict = conn.isReliable

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
  /** @internal */
  async handleRx(f: Frame, ctx: FrameContext): Promise<void> {
    if (this.done.tripped) return

    if (this.srvEpochSet && f.epoch !== this.srvEpoch) {
      // A frame from a different server incarnation (stale straggler after a
      // restart, or a raw-UDP injection) must not touch this live call.
      this.rxDropped++
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
    this.noteValidatedRx()

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
        return
      }
      this.latchHeader(f)
      if (this.conn.isReliable) {
        if (!(await this.rxq.putBlocking(f, this.done, ctx.signal))) {
          // The rx signal died mid-delivery: the transport is tearing down
          // (§4.5). The frame is gone and the window advanced — end the call
          // rather than leave a silent gap (§14).
          this.rxDropped++
          this.finishLocal(statusError(Code.UNAVAILABLE, 'transport closed during delivery'))
        }
      } else {
        this.rxq.putDrop(f, this.rxPolicy)
      }
    } else {
      this.rxDropped++
    }
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

    const payload = this.reqCodec.marshal(m)

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
    f.payload = payload

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
      if (f !== undefined) return this.resCodec.unmarshal(f.payload ?? new Uint8Array())
      if (this.done.tripped) return this.terminalRecv()
      await Promise.race([this.rxq.readable(), this.done.wait()])
    }
  }

  private terminalRecv(): Res | undefined {
    if (this.termErr !== undefined) throw this.termErr
    const f = this.term as Frame
    if ((f.code ?? 0) !== Code.OK) throw frameStatus(f)
    if (f.payload !== undefined && !this.termPayloadDelivered) {
      // Terminal payload (unary response, sendAndClose result) is delivered
      // once; the next recv reports end-of-stream.
      this.termPayloadDelivered = true
      return this.resCodec.unmarshal(f.payload)
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
