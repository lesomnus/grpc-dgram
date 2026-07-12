// drpc over WebRTC DataChannels: one channel message carries one marshaled
// Envelop, and the protocol mode is derived from the channel's own
// configuration — an ordered channel with no retransmit or lifetime cap is
// reliable, so the core runs with every timer off (PROTOCOL.md §10.6); any
// other configuration is unreliable and the full timer machinery is on. Same
// adapter, mode decided by the channel. This is the TS twin of the Go
// transport/pion adapter.
//
// The adapter takes an already-negotiated data channel; RTCPeerConnection
// setup and signaling stay with the application. DataChannelLike is
// structural, so the browser RTCDataChannel, node WebRTC implementations
// (werift, node-datachannel wrappers), and test mocks all fit.
//
// DataChannels are connection-oriented, so the §4.5 teardown duty applies:
// the attached client pump and servePeer watch channel death and call
// Conn.close / Server.disconnectPeer — in reliable mode the only unblocking
// mechanism. A peer connection can be severed without a close ever surfacing
// on the channel (the SCTP shutdown needs a live transport to travel over);
// applications should watch the RTCPeerConnection state and close the
// channel — or the Conn/Server — themselves when it fails.
//
// Receive-side note: a browser gives no way to pause RTCDataChannel delivery,
// so inbound messages queue in the adapter without bound while a reliable-
// mode consumer is slow (the browser buffers SCTP internally regardless).
// Frames are still delivered strictly in order, one at a time, awaiting the
// core — the §4.2 no-silent-drop contract holds; memory is the cost.

import type { Conn } from '../../conn'
import type { Server } from '../../server'
import { MessageTooLargeError } from '../../status'
import type { FrameContext, FrameHandler } from '../../seam'
import { unpack } from '../../seam'
import { abortListener, Latch, noop, unrefTimer } from '../../util'
import { decodeEnvelop, encodeEnvelop, type Frame } from '../../wire'

// DefaultMaxMessageSizeUnreliable keeps an envelop inside one SCTP packet on
// the typical 1500-byte path MTU: a partially-reliable message that SCTP
// fragments is lost whenever any one fragment is lost, multiplying the
// effective loss rate (PROTOCOL.md §4.4).
export const DefaultMaxMessageSizeUnreliable = 1200

// DefaultMaxMessageSizeReliable is the classic DataChannel interop ceiling:
// SCTP fragments and reassembles transparently, but 16 KiB is the largest
// message every stack — browsers included — accepts without end-of-record
// negotiation (PROTOCOL.md §4.4).
export const DefaultMaxMessageSizeReliable = 16 * 1024

// DefaultMaxBufferedAmount bounds dc.bufferedAmount: the stack queues
// outbound messages without a useful limit, so sends block once this much is
// unacknowledged and resume on bufferedamountlow (half the mark).
export const DefaultMaxBufferedAmount = 1 << 20

// DefaultSendStallTimeoutMs bounds how long one send may wait in total — for
// the channel to open and at the buffered-amount mark. In reliable mode the
// core runs no timers and does not bound its sends, so the adapter must
// bound a stalled write itself (PROTOCOL.md §4.2): a channel that will not
// open or a peer that stops draining for this long is transport death, and
// the stall trips the same teardown as a channel error.
export const DefaultSendStallTimeoutMs = 30_000

// DataChannelLike is the structural subset of RTCDataChannel the adapter
// needs. Event wiring prefers addEventListener and falls back to the on*
// properties — the adapter owns the channel either way.
export interface DataChannelLike {
  readonly readyState: string // 'connecting' | 'open' | 'closing' | 'closed'
  readonly ordered: boolean
  readonly maxRetransmits?: number | null
  readonly maxPacketLifeTime?: number | null
  readonly bufferedAmount: number
  bufferedAmountLowThreshold: number
  binaryType: string
  send(data: Uint8Array): void
  close(): void
  addEventListener?(type: string, listener: (ev: never) => void): void
  onopen?: ((ev: unknown) => void) | null
  onclose?: ((ev: unknown) => void) | null
  onerror?: ((ev: unknown) => void) | null
  onmessage?: ((ev: unknown) => void) | null
  onbufferedamountlow?: ((ev: unknown) => void) | null
}

export interface DataChannelOptions {
  // Largest marshaled Envelop this endpoint will send, in bytes; 0 removes
  // the limit. Bounds sends only. Unset, it follows the channel mode:
  // DefaultMaxMessageSizeUnreliable or DefaultMaxMessageSizeReliable.
  maxMessageSize?: number
  // Outbound high-water mark, in bytes: sends block while bufferedAmount is
  // at or above it; 0 never blocks. Default DefaultMaxBufferedAmount.
  maxBufferedAmount?: number
  // Bounds how long one send may wait in total — for the channel to open and
  // at the buffered-amount mark — before the channel is declared dead
  // (PROTOCOL.md §4.2); 0 waits on the frame's own signal alone. Default
  // DefaultSendStallTimeoutMs.
  sendStallTimeoutMs?: number
}

// channelReliable derives the protocol mode from the channel configuration —
// both ends observe the same parameters, negotiated or DCEP-announced.
// Ordered delivery with neither a retransmit cap nor a lifetime cap is full
// SCTP reliability: the core runs with every timer off (PROTOCOL.md §10.6).
// Any cap — even maxRetransmits: 0 — or unordered delivery lets envelops
// vanish or arrive out of order: the loss profile the core's timer machinery
// exists for.
export function channelReliable(dc: DataChannelLike): boolean {
  return dc.ordered && dc.maxRetransmits == null && dc.maxPacketLifeTime == null
}

function wire(dc: DataChannelLike, type: 'open' | 'close' | 'error' | 'message' | 'bufferedamountlow', fn: (ev: unknown) => void): void {
  if (typeof dc.addEventListener === 'function') {
    dc.addEventListener(type, fn as (ev: never) => void)
  } else {
    dc[`on${type}`] = fn
  }
}

// Channel owns one data channel: it buffers inbound messages from the moment
// it is constructed (messages that arrive with no handler registered are
// lost) and gates outbound messages on channel open and on the
// buffered-amount mark.
class Channel {
  readonly reliable: boolean
  private readonly max: number // send limit in bytes; <= 0 is unlimited
  private readonly high: number // bufferedAmount high-water mark; 0 disables blocking
  private readonly stallMs: number // max wait at the mark; <= 0 waits on the signal alone

  private readonly opened = new Latch()
  private readonly dead = new Latch()
  private err: unknown // first transport error observed; undefined for clean close
  private readonly rx: Uint8Array[] = []
  private rxWaiters: (() => void)[] = []
  private bufLowWaiters: (() => void)[] = []

  constructor(
    readonly dc: DataChannelLike,
    reliable: boolean,
    o: DataChannelOptions,
  ) {
    this.reliable = reliable
    this.max = o.maxMessageSize ?? (reliable ? DefaultMaxMessageSizeReliable : DefaultMaxMessageSizeUnreliable)
    this.high = o.maxBufferedAmount ?? DefaultMaxBufferedAmount
    this.stallMs = o.sendStallTimeoutMs ?? DefaultSendStallTimeoutMs

    dc.binaryType = 'arraybuffer'
    wire(dc, 'open', () => this.opened.trip())
    wire(dc, 'error', (ev) => this.fail((ev as { error?: unknown })?.error ?? ev))
    wire(dc, 'close', () => this.fail(undefined))
    if (this.high > 0) {
      dc.bufferedAmountLowThreshold = Math.floor(this.high / 2)
      wire(dc, 'bufferedamountlow', () => wakeAll(this.bufLowWaiters))
    }
    wire(dc, 'message', (ev) => this.onMessage(ev))
    // Events already past do not refire on late registration: read the
    // current state once.
    if (dc.readyState === 'open') this.opened.trip()
    else if (dc.readyState === 'closed' || dc.readyState === 'closing') this.fail(undefined)
  }

  // fail records the first death cause and trips dead. A clean close may be
  // observed before a racing error is recorded — the teardown is the same
  // either way.
  fail(err: unknown): void {
    if (!this.dead.tripped && this.err === undefined) this.err = err
    this.dead.trip()
    wakeAll(this.bufLowWaiters)
    wakeAll(this.rxWaiters)
  }

  deathErr(): unknown {
    return this.err
  }

  private closedErr(): Error {
    const e = new Error('webrtc: data channel closed')
    if (this.err !== undefined) e.cause = this.err
    return e
  }

  private onMessage(ev: unknown): void {
    const data = (ev as { data?: unknown }).data
    if (data instanceof ArrayBuffer) this.rx.push(new Uint8Array(data))
    else if (data instanceof Uint8Array) this.rx.push(data)
    else return // string / Blob: not a drpc envelop; dropped
    wakeAll(this.rxWaiters)
  }

  // send transmits one envelop as one channel message. It refuses an envelop
  // over the size limit synchronously (PROTOCOL.md §4.4), waits for the
  // channel to open, and blocks while bufferedAmount is at the high-water
  // mark — each wait bounded by the signal and by channel death, all under
  // one stall budget (the core's abort path sends with no signal at all, so
  // a signal alone cannot be the bound, §4.2).
  async send(frames: readonly Frame[], signal?: AbortSignal): Promise<void> {
    const data = encodeEnvelop(frames)
    if (this.max > 0 && data.length > this.max) {
      throw new MessageTooLargeError(`webrtc: ${data.length}-byte envelop over the ${this.max}-byte limit`)
    }

    const stalled = new Latch()
    let stallTimer: ReturnType<typeof setTimeout> | undefined
    if (this.stallMs > 0) {
      stallTimer = setTimeout(() => stalled.trip(), this.stallMs)
      unrefTimer(stallTimer)
    }
    let disposeAbort = noop
    const signalAborted = new Latch()
    if (signal !== undefined) {
      if (signal.aborted) signalAborted.trip()
      else disposeAbort = abortListener(signal, () => signalAborted.trip())
    }

    try {
      while (!this.opened.tripped) {
        await Promise.race([this.opened.wait(), stalled.wait(), this.dead.wait(), signalAborted.wait()])
        if (this.opened.tripped) break
        if (this.dead.tripped) throw this.closedErr()
        if (stalled.tripped) {
          const err = new Error(`webrtc: send stalled: channel not open within ${this.stallMs}ms`)
          this.fail(err)
          throw err
        }
        if (signalAborted.tripped) throw new Error('webrtc: send aborted')
      }
      while (this.high > 0 && this.dc.bufferedAmount >= this.high) {
        if (this.dead.tripped) throw this.closedErr()
        const low = new Promise<void>((res) => this.bufLowWaiters.push(res))
        await Promise.race([low, stalled.wait(), this.dead.wait(), signalAborted.wait()])
        if (this.dead.tripped) throw this.closedErr()
        if (stalled.tripped) {
          // The peer stopped draining: transport death (PROTOCOL.md §4.2),
          // tripping the same teardown as a channel error.
          const err = new Error(`webrtc: send stalled at the buffered-amount mark for ${this.stallMs}ms`)
          this.fail(err)
          throw err
        }
        if (signalAborted.tripped) throw new Error('webrtc: send aborted')
      }
      if (this.dead.tripped) throw this.closedErr()
      this.dc.send(data)
    } finally {
      if (stallTimer !== undefined) clearTimeout(stallTimer)
      disposeAbort()
    }
  }

  // pump delivers buffered messages to h in order until the channel dies; on
  // death it flushes what was received first, then resolves with the death
  // cause (undefined for a clean close). Frames are delivered under a signal
  // that aborts on channel death: in reliable mode the core may block in
  // backpressure (PROTOCOL.md §4.2), and death detected out-of-band must
  // unblock it or the §4.5 teardown never runs. The death flush still
  // delivers every frame that fits a stream buffer (the core prefers
  // delivery over a dead signal); only deliveries that would have to block
  // fail their call instead.
  async pump(h: FrameHandler, ctx: FrameContext): Promise<unknown> {
    const dctl = new AbortController()
    void this.dead.wait().then(() => dctl.abort(this.closedErr()))
    const dctx: FrameContext = { ...ctx, signal: dctl.signal }
    for (;;) {
      const data = this.rx.shift()
      if (data !== undefined) {
        let frames: Frame[]
        try {
          frames = decodeEnvelop(data)
        } catch {
          continue // malformed messages are dropped; never tear down (§4.2)
        }
        await unpack(frames, h, dctx)
        continue
      }
      if (this.dead.tripped) return this.err
      await Promise.race([this.rxReadable(), this.dead.wait()])
    }
  }

  private rxReadable(): Promise<void> {
    if (this.rx.length > 0) return Promise.resolve()
    return new Promise((res) => this.rxWaiters.push(res))
  }
}

function wakeAll(waiters: (() => void)[]): void {
  if (waiters.length === 0) return
  const ws = waiters.splice(0)
  for (const w of ws) w()
}

// ---------------------------------------------------------------------------
// client transport
// ---------------------------------------------------------------------------

// DataChannelTransport is the client-side endpoint: one data channel talking
// to one server, so no peer key is needed (PROTOCOL.md §6.4). It is the tx
// handler for the Conn constructor — implementing the TransportInfo and
// ConnAttacher discovery interfaces directly. The Conn attaches it and the
// drain pump starts by itself: no user plumbing, and conn.close() (or
// close() here) tears everything down, data channel included.
//
// Construct it promptly after the channel exists (for a remotely-announced
// channel, synchronously inside ondatachannel): messages that arrive before
// the handlers are registered are lost by the stack, not buffered.
export class DataChannelTransport {
  private readonly ch: Channel
  private attached = false
  private closed = false

  constructor(dc: DataChannelLike, opts: DataChannelOptions = {}) {
    this.ch = new Channel(dc, channelReliable(dc), opts)
  }

  // reliable reports the mode derived from the channel configuration; the
  // Conn reads it once at construction (PROTOCOL.md §4.3).
  reliable(): boolean {
    return this.ch.reliable
  }

  // attachConn is called by the Conn constructor: it starts the drain pump,
  // which runs until the channel dies or close() is called, then performs
  // the §4.5 teardown — conn.close(cause) — the only mechanism that unblocks
  // live calls in reliable mode.
  attachConn(conn: Conn): void {
    if (this.attached) throw new Error('webrtc: transport already attached to a Conn')
    this.attached = true
    void (async () => {
      const err = await this.ch.pump(conn, {})
      conn.close(err)
    })()
  }

  // handle sends one frame as a single-frame envelop, gated on channel open
  // and the buffered-amount mark; an envelop over the size limit is refused
  // synchronously with MessageTooLargeError (PROTOCOL.md §4.4).
  handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    return this.ch.send([f], ctx.signal)
  }

  // close closes the data channel; its death path flushes what was already
  // received, stops the pump, and fails any live calls. The death latch is
  // tripped directly rather than through the stack: a channel whose
  // association never established may never fire close/error, and the
  // teardown must not depend on it. Idempotent.
  close(): void {
    if (this.closed) return
    this.closed = true
    this.ch.fail(undefined)
    try {
      this.ch.dc.close()
    } catch {
      // an already-closed channel may throw; the latch is what matters
    }
  }
}

// ---------------------------------------------------------------------------
// server gateway
// ---------------------------------------------------------------------------

interface GwChannel {
  ch: Channel
  key: number
  served: boolean
}

// DataChannelGateway is the server-side endpoint: one Server serving many
// peers, one data channel each. It is the tx handler for the Server
// constructor.
//
// Channels of differing reliability mix freely — a reliable control channel
// and unreliable telemetry channels on one peer connection is the natural
// wiring. Each channel's mode is derived from its own configuration and
// annotated per peer (FrameContext.reliable), so the server runs every peer
// in its channel's mode; the Gateway itself deliberately does not implement
// the TransportInfo discovery — there is no single answer to advertise.
export class DataChannelGateway {
  private readonly o: DataChannelOptions
  private next = 0
  private readonly chans = new Map<DataChannelLike, GwChannel>()
  private readonly peers = new Map<number, Channel>()

  constructor(opts: DataChannelOptions = {}) {
    this.o = opts
  }

  // bind registers the gateway's handlers on dc and starts buffering its
  // inbound messages; it is idempotent per channel. For a remotely-announced
  // channel it should run synchronously inside ondatachannel, so no early
  // message is lost; servePeer binds implicitly for channels created
  // locally:
  //
  //   pc.ondatachannel = ({ channel }) => {
  //     gw.bind(channel)
  //     void gw.servePeer(server, channel)
  //   }
  bind(dc: DataChannelLike): void {
    this.bindChannel(dc)
  }

  private bindChannel(dc: DataChannelLike): GwChannel {
    let b = this.chans.get(dc)
    if (b === undefined) {
      b = { ch: new Channel(dc, channelReliable(dc), this.o), key: ++this.next, served: false }
      this.chans.set(dc, b)
      this.peers.set(b.key, b.ch)
    }
    return b
  }

  private drop(dc: DataChannelLike, b: GwChannel): void {
    // Trip the death latch ourselves: gated sends must unblock even when the
    // stack never fires a close event (never-established association).
    b.ch.fail(undefined)
    this.chans.delete(dc)
    this.peers.delete(b.key)
  }

  // servePeer delivers dc's frames to server under a fresh peer key —
  // annotated with the channel's own reliability, so the server runs this
  // peer in the channel's mode (PROTOCOL.md §4.3) — until the channel dies
  // or opts.signal aborts. On EVERY exit it performs the §4.5 teardown duty —
  // server.disconnectPeer with the cause — and deregisters the peer: exiting
  // abandons the channel (the key is never reused), so the peer's live calls
  // and state must die with it. Resolves with the death cause (undefined for
  // a clean close or signal abort). Each channel is served at most once.
  async servePeer(server: Server, dc: DataChannelLike, opts: { signal?: AbortSignal } = {}): Promise<unknown> {
    const b = this.bindChannel(dc)
    if (b.served) throw new Error('webrtc: channel already served')
    b.served = true

    let disposeAbort = noop
    if (opts.signal !== undefined) {
      if (opts.signal.aborted) b.ch.fail(undefined)
      else disposeAbort = abortListener(opts.signal, () => b.ch.fail(undefined))
    }
    try {
      const err = await b.ch.pump(server, { peer: b.key, reliable: b.ch.reliable })
      server.disconnectPeer(b.key, err)
      return err
    } finally {
      disposeAbort()
      this.drop(dc, b)
    }
  }

  // handle sends one frame as a single-frame envelop to the peer named in
  // ctx, with the same gating as the client transport.
  handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    const key = ctx.peer
    if (typeof key !== 'number') {
      return Promise.reject(new Error(`webrtc: no gateway peer in context (got ${String(key)})`))
    }
    const ch = this.peers.get(key)
    if (ch === undefined) {
      return Promise.reject(new Error(`webrtc: peer ${key} is gone`))
    }
    return ch.send([f], ctx.signal)
  }
}
