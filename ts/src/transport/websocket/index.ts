// drpc over WebSocket: one binary message carries one marshaled Envelop. The
// channel is reliable and ordered, so the core runs in reliable mode with
// every protocol timer and retransmission off (PROTOCOL.md §10.6) — leaving
// this adapter the two duties the protocol no longer covers:
//
//   - Teardown (§4.5): when the socket dies, conn.close() /
//     server.disconnectPeer() is the only mechanism that unblocks live calls.
//     The attached client pump and servePeer call it on every exit path —
//     this is the point of the adapter, not a nicety.
//   - Liveness (§10.6): death is detected out of band — the socket's own
//     close/error events, a send stalled at the buffered-amount mark, and,
//     where the runtime exposes ping/pong (node `ws`, not the browser), a
//     keepalive that declares a peer with no read progress dead.
//
// This is the TS twin of the Go `transport/gorilla` adapter and interoperates
// with it on the wire: a browser client here talks to a Go `drpc.Server`
// behind a gorilla `Gateway`, and vice versa.
//
// The socket is the WhatWG `WebSocket` (browser, Deno, Node ≥22, or the `ws`
// package) — `WebSocketLike` is structural, so all of them and test mocks
// fit, and nothing is imported: no npm dependency, no node builtin, safe to
// bundle for the browser. The adapter sets `binaryType = 'arraybuffer'`
// itself; text messages and undecodable payloads are ignored, never a
// teardown (§4.2).
//
// WebSocket fragments and reassembles internally, so no size logic is needed:
// the default send limit is unlimited (§4.4). `maxMessageSize` bounds sends
// for deployments whose path (a proxy, a browser) caps message size.
//
// Receive-side note: the WhatWG `WebSocket` gives no way to pause delivery,
// so inbound messages queue in the adapter without bound while a reliable-
// mode consumer is slow (Go's blocking read loop, which turns a full stream
// buffer into TCP backpressure, has no browser equivalent). Frames are still
// delivered strictly in order, one at a time, awaiting the core — the §4.2
// no-silent-drop contract holds; memory is the cost.
//
// A ws:// wire is plaintext. Deploy wss:// (TLS) or stay on a trusted
// network — see PROTOCOL.md §15.

import { Conn, type ConnOptions } from '../../conn'
import type { Server } from '../../server'
import type { ConnAttacher, FrameContext, FrameHandler, TransportInfo } from '../../seam'
import { unpack } from '../../seam'
import { Code, MessageTooLargeError, StatusError } from '../../status'
import { abortListener, Latch, noop, unrefTimer } from '../../util'
import { decodeEnvelop, encodeEnvelop, type Frame } from '../../wire'

// WhatWG WebSocket.readyState values, spelled out so no DOM lib is required.
const CONNECTING = 0
const OPEN = 1
const CLOSING = 2
const CLOSED = 3

// DefaultMaxMessageSize is 0 — unlimited. A reliable transport carries any
// size and WebSocket fragments internally (PROTOCOL.md §4.4).
export const DefaultMaxMessageSize = 0

// DefaultMaxBufferedAmount bounds ws.bufferedAmount: send() never blocks, so
// a peer that stops draining would otherwise grow the outbound buffer without
// limit. Sends park once this much is unflushed.
export const DefaultMaxBufferedAmount = 1 << 20

// DefaultKeepaliveIntervalMs is the ping cadence; DefaultKeepaliveTimeoutMs is
// how long the peer may go without read progress (data or pong) before the
// connection is declared dead. The timeout leaves room for one lost ping
// round on a congested path. Both apply only where the runtime exposes
// ping/pong (see WebSocketLike.ping).
export const DefaultKeepaliveIntervalMs = 20_000
export const DefaultKeepaliveTimeoutMs = 30_000

// bufferedPollMs is how often a parked send re-reads bufferedAmount: the
// WhatWG WebSocket has no 'bufferedamountlow' event to wait on.
const bufferedPollMs = 25

// closeCodesClean are the close codes that mean "the peer went away in an
// orderly fashion": no error cause, the same set gorilla's IsCloseError
// treats as a clean exit. 1005 is "no status received", what a browser
// reports for a close frame with no code.
const closeCodesClean = new Set([1000, 1001, 1005])

// WebSocketLike is the structural subset of the WhatWG WebSocket the adapter
// needs. Event wiring prefers addEventListener and falls back to the on*
// properties — the adapter owns the socket either way.
export interface WebSocketLike {
  readonly readyState: number // 0 CONNECTING | 1 OPEN | 2 CLOSING | 3 CLOSED
  readonly bufferedAmount: number
  binaryType: string
  send(data: Uint8Array): void
  close(code?: number, reason?: string): void
  addEventListener?(type: string, listener: (ev: never) => void): void
  onopen?: ((ev: unknown) => void) | null
  onclose?: ((ev: unknown) => void) | null
  onerror?: ((ev: unknown) => void) | null
  onmessage?: ((ev: unknown) => void) | null
  // Optional keepalive seam. The browser WebSocket API exposes neither, so
  // the keepalive is simply off there (the stack answers pings itself and
  // death arrives as close/error); node's `ws` exposes both, and then the
  // adapter runs gorilla's liveness rule — ping every interval, dead without
  // read progress within the timeout.
  ping?(data?: unknown): void
  on?(type: string, listener: (...args: never[]) => void): void
}

export interface WebSocketOptions {
  // Largest marshaled Envelop this endpoint will send, in bytes; 0 (the
  // default) is unlimited — a reliable transport carries any size
  // (PROTOCOL.md §4.4). Bounds sends only; receives accept any message.
  maxMessageSize?: number
  // Outbound high-water mark, in bytes: sends park while bufferedAmount is at
  // or above it; 0 never parks. Default DefaultMaxBufferedAmount.
  maxBufferedAmount?: number
  // Bounds how long one send may wait in total — for the socket to open and
  // at the buffered-amount mark — before the socket is declared dead
  // (PROTOCOL.md §4.2); 0 waits on the frame's own signal alone. Defaults to
  // the keepalive timeout, the way gorilla's write deadline does.
  sendStallTimeoutMs?: number
  // Ping cadence, and how long the peer may go without read progress (data or
  // pong) before the connection is declared dead. Ignored where the runtime
  // exposes no ping/pong (the browser). A non-positive interval disables the
  // keepalive.
  keepaliveIntervalMs?: number
  keepaliveTimeoutMs?: number
}

function wire(ws: WebSocketLike, type: 'open' | 'close' | 'error' | 'message', fn: (ev: unknown) => void): void {
  if (typeof ws.addEventListener === 'function') {
    ws.addEventListener(type, fn as (ev: never) => void)
  } else {
    ws[`on${type}`] = fn
  }
}

// errorOf extracts a cause from an error event: node's `ws` carries the real
// Error, a browser ErrorEvent at most a message, the DOM Event nothing at all.
function errorOf(ev: unknown): unknown {
  const e = ev as { error?: unknown; message?: unknown } | undefined
  if (e?.error !== undefined && e.error !== null) return e.error
  if (typeof e?.message === 'string') return new Error(`websocket: ${e.message}`)
  return new Error('websocket: transport error')
}

// closeCauseOf maps a close event to a death cause: undefined for an orderly
// close (the peer said goodbye — live calls still fail UNAVAILABLE, they just
// carry no error detail), an Error for an abnormal one (1006 no close frame,
// 1011 internal error, a policy close, ...).
function closeCauseOf(ev: unknown): unknown {
  const e = ev as { code?: unknown; reason?: unknown } | undefined
  const code = typeof e?.code === 'number' ? e.code : 1005
  if (closeCodesClean.has(code)) return undefined
  const reason = typeof e?.reason === 'string' && e.reason !== '' ? `: ${e.reason}` : ''
  return new Error(`websocket: closed with code ${code}${reason}`)
}

function wakeAll(waiters: (() => void)[]): void {
  if (waiters.length === 0) return
  const ws = waiters.splice(0)
  for (const w of ws) w()
}

// Socket owns one WebSocket: it buffers inbound messages from the moment it
// is constructed (messages that arrive with no handler registered are lost by
// the stack), gates outbound messages on open and on the buffered-amount
// mark, and runs the keepalive where the runtime allows one.
class Socket {
  private readonly max: number // send limit in bytes; <= 0 is unlimited
  private readonly high: number // bufferedAmount high-water mark; 0 disables parking
  private readonly stallMs: number // max wait for open / at the mark; <= 0 waits on the signal alone
  private readonly kaIntervalMs: number
  private readonly kaTimeoutMs: number

  private readonly opened = new Latch()
  private readonly dead = new Latch()
  private err: unknown // first death cause observed; undefined for a clean close
  private readonly rx: Uint8Array[] = []
  private rxWaiters: (() => void)[] = []

  private kaTimer: ReturnType<typeof setInterval> | undefined
  private kaDeadline: ReturnType<typeof setTimeout> | undefined

  constructor(
    readonly ws: WebSocketLike,
    o: WebSocketOptions,
  ) {
    this.max = o.maxMessageSize ?? DefaultMaxMessageSize
    this.high = o.maxBufferedAmount ?? DefaultMaxBufferedAmount
    this.kaIntervalMs = o.keepaliveIntervalMs ?? DefaultKeepaliveIntervalMs
    this.kaTimeoutMs = o.keepaliveTimeoutMs ?? DefaultKeepaliveTimeoutMs
    this.stallMs = o.sendStallTimeoutMs ?? this.kaTimeoutMs

    // Binary framing is the wire contract (§4.1): one marshaled Envelop per
    // message. Without this a browser hands back Blobs, which cannot be read
    // synchronously.
    try {
      ws.binaryType = 'arraybuffer'
    } catch {
      // a socket that refuses the assignment still delivers Uint8Array on
      // node; onMessage accepts both shapes
    }
    wire(ws, 'open', () => this.onOpen())
    wire(ws, 'error', (ev) => this.fail(errorOf(ev)))
    wire(ws, 'close', (ev) => this.fail(closeCauseOf(ev)))
    wire(ws, 'message', (ev) => this.onMessage(ev))
    // Events already past do not refire on late registration: read the
    // current state once.
    if (ws.readyState === OPEN) this.onOpen()
    else if (ws.readyState === CLOSING || ws.readyState === CLOSED) this.fail(undefined)
  }

  // fail records the first death cause and trips dead, stopping the keepalive
  // and waking every parked waiter. A clean close may be observed before a
  // racing error is recorded — the teardown is the same either way.
  fail(err: unknown): void {
    if (!this.dead.tripped && this.err === undefined) this.err = err
    this.dead.trip()
    this.stopKeepalive()
    wakeAll(this.rxWaiters)
  }

  deathErr(): unknown {
    return this.err
  }

  // closedErr is what a send racing the teardown fails with. It is a
  // StatusError so that race is invisible: the core passes a StatusError
  // through unchanged (toStatusError), so the send fails with the very code
  // the §4.5 teardown would have given the call a moment later.
  private closedErr(): StatusError {
    const detail = this.err instanceof Error ? `: ${this.err.message}` : ''
    const e = new StatusError(Code.UNAVAILABLE, `websocket: socket closed${detail}`)
    if (this.err !== undefined) e.cause = this.err
    return e
  }

  private onOpen(): void {
    if (this.dead.tripped) return
    this.opened.trip()
    this.startKeepalive()
  }

  // startKeepalive arms gorilla's liveness rule where the runtime exposes
  // ping/pong: a ping every interval, death when the peer makes no read
  // progress (data or pong) within the timeout. A ping the socket cannot even
  // carry is transport death seen from this side — the one death signal that
  // fires while the pump is blocked in reliable-mode backpressure (§4.5).
  //
  // The browser WebSocket exposes no ping API, and its stack answers pings
  // without telling the page, so there the keepalive is simply off: death
  // arrives as a close/error event (which, unlike Go's read loop, is
  // delivered even while delivery is blocked) or as a stalled send.
  private startKeepalive(): void {
    if (this.kaTimer !== undefined || this.dead.tripped) return
    if (this.kaIntervalMs <= 0 || this.kaTimeoutMs <= 0) return
    const ws = this.ws
    if (typeof ws.ping !== 'function' || typeof ws.on !== 'function') return

    ws.on('pong', () => this.progress())
    const t = setInterval(() => {
      try {
        ws.ping?.()
      } catch (e) {
        this.fail(new Error(`websocket: keepalive ping: ${e instanceof Error ? e.message : String(e)}`, { cause: e }))
      }
    }, this.kaIntervalMs)
    unrefTimer(t)
    this.kaTimer = t
    this.progress() // arm the read-progress deadline
  }

  // progress grants another keepalive window; any read progress — a data
  // message or a pong — calls it. A no-op when no keepalive is armed, so an
  // idle browser socket is never declared dead for lack of pings it cannot
  // send.
  private progress(): void {
    if (this.kaDeadline !== undefined) clearTimeout(this.kaDeadline)
    this.kaDeadline = undefined
    if (this.kaTimer === undefined || this.dead.tripped) return
    const t = setTimeout(() => this.fail(new Error(`websocket: no read progress within ${this.kaTimeoutMs}ms`)), this.kaTimeoutMs)
    unrefTimer(t)
    this.kaDeadline = t
  }

  private stopKeepalive(): void {
    if (this.kaTimer !== undefined) clearInterval(this.kaTimer)
    if (this.kaDeadline !== undefined) clearTimeout(this.kaDeadline)
    this.kaTimer = undefined
    this.kaDeadline = undefined
  }

  private onMessage(ev: unknown): void {
    // Nothing is delivered after death: past the §4.5 teardown the calls this
    // frame could belong to are already failed, and the pump is gone.
    if (this.dead.tripped) return
    this.progress()
    const data = (ev as { data?: unknown }).data
    if (data instanceof ArrayBuffer) this.rx.push(new Uint8Array(data))
    else if (ArrayBuffer.isView(data)) this.rx.push(new Uint8Array(data.buffer, data.byteOffset, data.byteLength))
    else return // string / Blob: not a drpc envelop; dropped, never a teardown
    wakeAll(this.rxWaiters)
  }

  // send transmits one envelop as one binary message. It refuses an envelop
  // over the size limit synchronously (PROTOCOL.md §4.4), waits for the
  // socket to open, and parks while bufferedAmount is at the high-water mark
  // — each wait bounded by socket death and by one stall budget. The budget
  // is what gorilla's write deadline is: with protocol timers off, a peer
  // that stops draining would otherwise block a send forever and wedge every
  // call on this socket, so a send that cannot progress is transport death
  // (§4.2). The core's abort path sends with no signal at all, so a signal
  // alone cannot be the bound.
  async send(frames: readonly Frame[], signal?: AbortSignal): Promise<void> {
    const data = encodeEnvelop(frames)
    if (this.max > 0 && data.length > this.max) {
      throw new MessageTooLargeError(`websocket: ${data.length}-byte envelop over the ${this.max}-byte limit`)
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
          const err = new Error(`websocket: send stalled: socket not open within ${this.stallMs}ms`)
          this.fail(err)
          throw err
        }
        if (signalAborted.tripped) throw new Error('websocket: send aborted')
      }
      while (this.high > 0 && this.ws.bufferedAmount >= this.high) {
        if (this.dead.tripped) throw this.closedErr()
        await Promise.race([poll(), stalled.wait(), this.dead.wait(), signalAborted.wait()])
        if (this.dead.tripped) throw this.closedErr()
        if (stalled.tripped) {
          // The peer stopped draining: transport death (PROTOCOL.md §4.2),
          // tripping the same teardown as a socket error.
          const err = new Error(`websocket: send stalled at the buffered-amount mark for ${this.stallMs}ms`)
          this.fail(err)
          throw err
        }
        if (signalAborted.tripped) throw new Error('websocket: send aborted')
      }
      if (this.dead.tripped) throw this.closedErr()
      try {
        this.ws.send(data)
      } catch (e) {
        // The stack refused the message (InvalidStateError on a socket that
        // died between the check and here): transport death, tripping the
        // same teardown as a close event.
        this.fail(e)
        throw this.closedErr()
      }
    } finally {
      if (stallTimer !== undefined) clearTimeout(stallTimer)
      disposeAbort()
    }
  }

  // pump delivers buffered messages to h in order until the socket dies; on
  // death it flushes what was received first, then resolves with the death
  // cause (undefined for a clean close). Frames are delivered under a signal
  // that aborts on death: in reliable mode the core may block in backpressure
  // (PROTOCOL.md §4.2), and death detected out of band must unblock it or the
  // §4.5 teardown never runs. The death flush still delivers every frame that
  // fits a stream buffer (the core prefers delivery over a dead signal); only
  // deliveries that would have to block fail their call instead.
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

  // close trips the death latch and closes the socket. The latch is tripped
  // directly rather than waited for through the stack: a socket still in
  // CONNECTING may never fire close, and the teardown must not depend on it.
  close(): void {
    this.fail(undefined)
    try {
      if (this.ws.readyState === CONNECTING || this.ws.readyState === OPEN) this.ws.close(1000, '')
    } catch {
      // an already-closed socket may throw; the latch is what matters
    }
  }
}

function poll(): Promise<void> {
  return new Promise((res) => {
    const t = setTimeout(res, bufferedPollMs)
    unrefTimer(t)
  })
}

// ---------------------------------------------------------------------------
// client transport
// ---------------------------------------------------------------------------

// WebSocketTransport is the client-side endpoint: one WebSocket talking to one
// server, so no peer key is needed (PROTOCOL.md §6.4). It is the tx handler
// for the Conn constructor — implementing the TransportInfo and ConnAttacher
// discovery interfaces directly, so neither is masked by a wrapper. The Conn
// attaches it and the receive pump and keepalive start by themselves: no user
// plumbing, and conn.close() (or close() here) tears everything down, socket
// included.
//
// Construct it promptly after the socket exists — ideally on the same tick as
// `new WebSocket(url)` — so no early message is lost: the stack drops
// messages that arrive before a listener is registered.
export class WebSocketTransport implements FrameHandler, TransportInfo, ConnAttacher {
  private readonly sock: Socket
  private attached = false
  private closed = false

  constructor(ws: WebSocketLike, opts: WebSocketOptions = {}) {
    this.sock = new Socket(ws, opts)
  }

  // reliable reports true: WebSocket neither loses, duplicates, nor reorders.
  // The Conn discovers this at construction and disables every protocol timer
  // (PROTOCOL.md §10.6), which is what makes the pump's teardown duty
  // mandatory.
  reliable(): boolean {
    return true
  }

  // attachConn is called by the Conn constructor: it starts the receive pump,
  // which runs until the socket dies or close() is called, and on every exit
  // path performs the §4.5 teardown — conn.close(cause) — the only mechanism
  // that unblocks live calls in reliable mode.
  attachConn(conn: Conn): void {
    if (this.attached) throw new Error('websocket: transport already attached to a Conn')
    this.attached = true
    void (async () => {
      const err = await this.sock.pump(conn, {})
      conn.close(err)
      this.close()
    })()
  }

  // handle sends one frame as a single-frame envelop, gated on open and on
  // the buffered-amount mark; an envelop over the size limit is refused
  // synchronously with MessageTooLargeError (PROTOCOL.md §4.4).
  handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    return this.sock.send([f], ctx.signal)
  }

  // close stops the pump and closes the WebSocket; the pump's exit fails any
  // live calls. Idempotent — Conn.close calls it, and it is safe to call
  // directly.
  close(): void {
    if (this.closed) return
    this.closed = true
    this.sock.close()
  }
}

// ---------------------------------------------------------------------------
// server gateway
// ---------------------------------------------------------------------------

interface GwSocket {
  sock: Socket
  key: number
  served: boolean
}

// WebSocketGateway is the server-side endpoint: one Server serving many peers,
// one WebSocket each. It is the tx handler for the Server constructor,
// implementing TransportInfo directly so mode discovery is not masked by a
// wrapper — every WebSocket peer is reliable, so unlike the WebRTC gateway
// there is a single answer to advertise.
//
// The peer key is a fresh opaque counter per socket, deliberately not the
// remote address: addresses collide behind proxies, and one connection is one
// peer (PROTOCOL.md §6.4).
export class WebSocketGateway implements FrameHandler, TransportInfo {
  private readonly o: WebSocketOptions
  private next = 0
  private readonly socks = new Map<WebSocketLike, GwSocket>()
  private readonly peers = new Map<number, Socket>()

  constructor(opts: WebSocketOptions = {}) {
    this.o = opts
  }

  // reliable reports true: the Server discovers it once at construction and
  // runs every peer with protocol timers off (PROTOCOL.md §10.6), which is
  // what makes servePeer's teardown duty mandatory.
  reliable(): boolean {
    return true
  }

  // bind registers the gateway's handlers on ws and starts buffering its
  // inbound messages; it is idempotent per socket. Call it synchronously in
  // the upgrade/connection handler so no early message is lost; servePeer
  // binds implicitly:
  //
  //   wss.on('connection', (ws) => {
  //     gw.bind(ws)
  //     void gw.servePeer(server, ws)
  //   })
  bind(ws: WebSocketLike): void {
    this.bindSocket(ws)
  }

  private bindSocket(ws: WebSocketLike): GwSocket {
    let b = this.socks.get(ws)
    if (b === undefined) {
      b = { sock: new Socket(ws, this.o), key: ++this.next, served: false }
      this.socks.set(ws, b)
      this.peers.set(b.key, b.sock)
    }
    return b
  }

  private drop(ws: WebSocketLike, b: GwSocket): void {
    // Trip the death latch ourselves: parked sends must unblock even when the
    // stack never fires a close event (a socket that never finished
    // connecting).
    b.sock.fail(undefined)
    this.socks.delete(ws)
    this.peers.delete(b.key)
  }

  // servePeer delivers ws's frames to server under a fresh peer key —
  // annotated reliable, so the peer runs with every timer off (PROTOCOL.md
  // §4.3, §10.6) — until the socket dies or opts.signal aborts. On EVERY exit
  // it performs the §4.5 teardown duty — server.disconnectPeer with the cause
  // — and deregisters the peer: exiting abandons the socket (the key is never
  // reused), so the peer's live calls and state must die with it. Resolves
  // with the death cause (undefined for a clean close or signal abort). Each
  // socket is served at most once.
  async servePeer(server: Server, ws: WebSocketLike, opts: { signal?: AbortSignal } = {}): Promise<unknown> {
    const b = this.bindSocket(ws)
    if (b.served) throw new Error('websocket: socket already served')
    b.served = true

    let disposeAbort = noop
    if (opts.signal !== undefined) {
      if (opts.signal.aborted) b.sock.close()
      else disposeAbort = abortListener(opts.signal, () => b.sock.close())
    }
    try {
      const err = await b.sock.pump(server, { peer: b.key, reliable: true })
      server.disconnectPeer(b.key, err)
      return err
    } finally {
      disposeAbort()
      this.drop(ws, b)
    }
  }

  // handle sends one frame as a single-frame envelop to the peer named in
  // ctx, with the same gating as the client transport.
  handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    const key = ctx.peer
    if (typeof key !== 'number') {
      return Promise.reject(new Error(`websocket: no gateway peer in context (got ${String(key)})`))
    }
    const sock = this.peers.get(key)
    if (sock === undefined) {
      return Promise.reject(new Error(`websocket: peer ${key} is disconnected`))
    }
    return sock.send([f], ctx.signal)
  }

  // close tears every served socket down; each servePeer then exits through
  // its own §4.5 teardown.
  close(): void {
    for (const b of [...this.socks.values()]) b.sock.close()
  }
}

// ---------------------------------------------------------------------------
// convenience
// ---------------------------------------------------------------------------

interface WebSocketCtor {
  new (url: string, protocols?: string | string[]): WebSocketLike
}

// dialWebSocket opens a client socket with the runtime's global WebSocket
// (browser, Deno, Node ≥22) and hands back a Conn over it, ready to call:
//
//   const conn = dialWebSocket('wss://host/rpc') // reliable mode, no timers
//
// dial is the verb for reaching a peer that already exists, and what it hands
// back is the endpoint you make calls on — the same bargain as Go's net.Dial,
// which returns a net.Conn. It is synchronous and returns before the
// handshake: sends are gated on open, so a call made on this very tick queues
// rather than fails.
//
// The options are one bag, and no two of its readers share a key: the socket
// takes `protocols`, the adapter reads WebSocketOptions (the size ceiling, the
// buffered-amount mark, the stall budget, the keepalive), and the Conn reads
// everything ConnOptions declares.
//
// Build the pair yourself — `new Conn(new WebSocketTransport(ws), opts)` —
// when you brought the socket (node's `ws` package, a runtime with no global
// WebSocket, a socket needing constructor options this does not expose) or
// when you need the transport object itself. Do it on the same tick as `new
// WebSocket(url)`: the stack drops messages that arrive before a listener is
// registered.
export function dialWebSocket(url: string, opts: ConnOptions & WebSocketOptions & { protocols?: string | string[] } = {}): Conn {
  const ctor = (globalThis as { WebSocket?: WebSocketCtor }).WebSocket
  if (ctor === undefined) {
    throw new Error("websocket: this runtime has no global WebSocket; construct one (e.g. from the 'ws' package) and pass it to new WebSocketTransport()")
  }
  return new Conn(new WebSocketTransport(new ctor(url, opts.protocols), opts), opts)
}
