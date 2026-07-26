// drpc over a JS message port: one posted message carries one marshaled
// Envelop (PROTOCOL.md §4.1) — byte for byte the wire the WebSocket adapter
// speaks. A "port" here is anything with `postMessage(data)` and a `message`
// event: both ends of a MessageChannel, a Worker seen from the main thread,
// and `self` inside a dedicated worker. It deliberately does NOT cover
// `window.postMessage`, whose second argument is a targetOrigin rather than a
// transfer list — for an iframe, transfer a MessagePort through the window
// and hand that port here.
//
// The motivating deployment is a Go `drpc.Server` compiled to GOOS=js
// GOARCH=wasm running inside the page, with the browser UI as its client, so
// a page reload restarts the whole server. Nothing here knows about wasm
// though: it is a port, and the peer could equally be another TS endpoint
// across a Worker boundary. This is the TS twin of the Go transport/jsport
// adapter and interoperates with it on the wire.
//
// A port neither loses, duplicates nor reorders, so reliable() is true
// unconditionally: the core runs with every protocol timer off (§10.6) and
// per-stream flow control on (§4.2.1). That leaves the adapter the one duty
// the protocol no longer covers — teardown (§4.5) — and here it has to be
// invented, because there is no socket to die:
//
//   - **The goodbye is an empty message.** A 0-byte message decodes to an
//     Envelop with zero frames, which the wire never otherwise carries (§4.1
//     says 1..n), so it is free to mean "this endpoint is going away".
//     close() posts one, best effort, before closing the port; a pump that
//     reads a 0-byte message treats it as EOF, exits, and runs the §4.5
//     teardown — conn.close() / server.disconnectPeer(). This is the
//     equivalent of the WebSocket close handshake and the only reason a peer
//     that goes away does not leave live calls hanging forever.
//   - **An explicit close(cause) from the host.** The host knows what the
//     port cannot report: a wasm instance that exited or panicked (go.run()'s
//     promise resolving), a terminated Worker, a page teardown. The cause
//     travels into the teardown, so live calls fail UNAVAILABLE saying why.
//
// A `close` event is wired where the runtime fires one (newer MessagePort,
// node's), and `messageerror` — a message the runtime could not deserialize —
// is dropped like any other malformed input (§4.2), never a teardown.
//
// There is deliberately no keepalive: two endpoints in one process cannot be
// partitioned, so an unanswered ping would only measure how busy the peer is.
// Death is said out loud instead.
//
// Receive-side note: postMessage has no backpressure and inbound messages
// queue in the adapter without bound. That is safe for the same reason the
// WebSocket adapter's unbounded rx queue is: in reliable mode a conforming
// peer cannot put more in flight than the per-stream windows it was granted
// (§4.2.1). A received frame is never dropped — in reliable mode a gap is a
// protocol error, not a lost datagram (§4.2).
//
// No npm dependency, no node builtin, browser-safe: PortLike is structural,
// so a browser MessagePort, a node one, a Worker and a test mock all fit.

import type { Conn } from '../../conn'
import type { Server } from '../../server'
import type { ConnAttacher, FrameContext, FrameHandler, TransportInfo } from '../../seam'
import { unpack } from '../../seam'
import { Code, MessageTooLargeError, StatusError } from '../../status'
import { abortListener, Latch, noop } from '../../util'
import { decodeEnvelop, encodeEnvelop, type Frame } from '../../wire'

// DefaultMaxMessageSize is 0 — unlimited. Structured clone has no protocol
// ceiling, so like WebSocket this endpoint refuses nothing by default
// (PROTOCOL.md §4.4).
export const DefaultMaxMessageSize = 0

// The goodbye: a zero-frame envelop, which is exactly 0 bytes (see §4.1 and
// encodeEnvelop). One instance, since it is immutable and never transferred.
const GOODBYE = new Uint8Array(0)

// PortLike is the structural subset of a JS message port the adapter needs.
// Event wiring prefers addEventListener and falls back to the on* properties;
// `start` and `close` exist on a MessagePort but not on a Worker or on the
// worker global scope, so both are optional and called only where present.
//
// The listener parameter types are chosen so a real MessagePort and a real
// Worker are assignable: `unknown` in the (bivariant) method position of
// addEventListener, `never` in the contravariant property position of the on*
// handlers. Both accept any listener this file registers.
export interface PortLike {
  postMessage(data: Uint8Array, transfer?: unknown[]): void
  addEventListener?(type: string, fn: (ev: unknown) => void): void
  removeEventListener?(type: string, fn: (ev: unknown) => void): void
  onmessage?: ((ev: never) => void) | null
  onmessageerror?: ((ev: never) => void) | null
  // A MessagePort wired through addEventListener stays paused until start();
  // getting this wrong means silence, not an error.
  start?(): void
  close?(): void
  // Declared so a Worker fits the type — never called: terminate() aborts the
  // worker at once and would discard the goodbye still queued for it. Killing
  // a worker is the host's decision, taken after its endpoint has torn down.
  terminate?(): void
}

export interface PortOptions {
  // Largest marshaled Envelop this endpoint will send, in bytes; 0 (the
  // default) is unlimited (PROTOCOL.md §4.4). Bounds sends only; receives
  // accept any message.
  maxMessageSize?: number
  // Hand the message buffer to the peer instead of copying it (default true).
  // The adapter allocated the buffer and never looks at it again, so the
  // transfer is safe and saves the structured-clone copy — which is the whole
  // point of crossing a wasm boundary as marshaled bytes. A port that refuses
  // transfer lists still works: the send retries as a plain copy.
  transfer?: boolean
}

// wire registers fn for one event type and returns the disposer that undoes
// it. Only `message` and `messageerror` have on* fallbacks — no runtime
// exposes an `onclose` on a port — so a port without addEventListener simply
// has no close event, and the goodbye (or the host's close()) is what reports
// death there.
//
// Detaching matters because the adapter does not own every kind of port: a
// Worker and a worker's `self` outlive the endpoint (neither has a close()
// that would release them), and a listener left on one keeps this Port, its
// rx buffer and everything they close over alive for as long as the port is —
// and would hand a second endpoint bound to the same port a duplicate of
// every message.
function wire(port: PortLike, type: 'message' | 'messageerror' | 'close', fn: (ev: unknown) => void): () => void {
  if (typeof port.addEventListener === 'function') {
    port.addEventListener(type, fn)
    return () => port.removeEventListener?.(type, fn)
  }
  if (type === 'message') {
    port.onmessage = fn
    return () => {
      port.onmessage = null
    }
  }
  if (type === 'messageerror') {
    port.onmessageerror = fn
    return () => {
      port.onmessageerror = null
    }
  }
  return noop
}

function wakeAll(waiters: (() => void)[]): void {
  if (waiters.length === 0) return
  const ws = waiters.splice(0)
  for (const w of ws) w()
}

// Port owns one message port: it buffers inbound messages from the moment it
// is constructed (a MessagePort queues them itself until start(), but a
// Worker does not), posts one message per envelop, and turns both flavours of
// goodbye — the peer's empty message and the host's close() — into the single
// death signal the §4.5 teardown hangs off.
class Port {
  private readonly max: number // send limit in bytes; <= 0 is unlimited
  private readonly transferable: boolean

  private readonly dead = new Latch()
  private err: unknown // first death cause observed; undefined for a clean goodbye
  private closed = false // close() ran: the port is gone, no second goodbye
  private readonly rx: Uint8Array[] = []
  private rxWaiters: (() => void)[] = []
  private readonly detach: (() => void)[] = []

  constructor(
    readonly port: PortLike,
    o: PortOptions,
  ) {
    this.max = o.maxMessageSize ?? DefaultMaxMessageSize
    this.transferable = o.transfer ?? true

    this.detach.push(wire(port, 'message', (ev) => this.onMessage(ev)))
    // A messageerror is a message the runtime could not deserialize: garbage
    // from something else sharing the port, or a value this realm cannot
    // reconstruct. Malformed input is dropped (§4.2) — a peer must not be
    // able to tear the channel down by posting nonsense.
    this.detach.push(wire(port, 'messageerror', noop))
    // Where the runtime fires it (newer MessagePort, node's), a close event
    // is real death: the peer's port went away without a goodbye.
    this.detach.push(wire(port, 'close', () => this.fail(undefined)))
    // Listeners registered through addEventListener leave a MessagePort
    // paused; without this nothing is ever delivered and nothing reports why.
    port.start?.()
  }

  // fail records the first death cause and trips dead, waking every waiter.
  // The peer's goodbye and a clean close() both pass undefined: the teardown
  // is the same, only the detail on the failed calls differs.
  fail(err: unknown): void {
    if (!this.dead.tripped && this.err === undefined) this.err = err
    this.dead.trip()
    wakeAll(this.rxWaiters)
  }

  // closedErr is what a send racing the teardown fails with. It is a
  // StatusError so that race is invisible: the core passes a StatusError
  // through unchanged (toStatusError), so the send fails with the very code
  // the §4.5 teardown would have given the call a moment later. A host cause
  // ("the wasm instance exited") is spelled into the message — it is the only
  // explanation of the death anyone will get.
  private closedErr(): StatusError {
    const e = new StatusError(Code.UNAVAILABLE, `port: endpoint closed${causeDetail(this.err)}`)
    if (this.err !== undefined) e.cause = this.err
    return e
  }

  private onMessage(ev: unknown): void {
    // Nothing is delivered after death: past the §4.5 teardown the calls this
    // frame could belong to are already failed, and the pump is gone.
    if (this.dead.tripped) return
    const data = (ev as { data?: unknown }).data
    if (data instanceof ArrayBuffer) this.rx.push(new Uint8Array(data))
    else if (ArrayBuffer.isView(data)) this.rx.push(new Uint8Array(data.buffer, data.byteOffset, data.byteLength))
    else return // a string or some other library's object sharing the port (§4.2)
    wakeAll(this.rxWaiters)
  }

  // send posts one envelop as one message. An envelop over the size limit is
  // refused before anything reaches the port (PROTOCOL.md §4.4) — the core
  // then fails the owning call with RESOURCE_EXHAUSTED and reclaims its seq.
  // Nothing else here can park: postMessage has no backpressure, so a send
  // either happens now or the endpoint is dead. The frame's own signal
  // therefore has nothing to bound.
  async send(frames: readonly Frame[]): Promise<void> {
    const data = encodeEnvelop(frames)
    if (this.max > 0 && data.length > this.max) {
      throw new MessageTooLargeError(`port: ${data.length}-byte envelop over the ${this.max}-byte limit`)
    }
    if (this.dead.tripped) throw this.closedErr()
    try {
      this.post(data)
    } catch (e) {
      // A closed or neutered port refuses the message: death seen from this
      // side, tripping the same teardown as the peer's goodbye.
      this.fail(e)
      throw this.closedErr()
    }
  }

  // post hands the bytes over rather than copying them when the array owns
  // its whole buffer — which encodeEnvelop's output always does — and falls
  // back to a plain post if this port refuses transfer lists.
  private post(data: Uint8Array): void {
    if (this.transferable && data.byteOffset === 0 && data.byteLength === data.buffer.byteLength) {
      try {
        this.port.postMessage(data, [data.buffer])
        return
      } catch (e) {
        // Retry as a copy — but only if the buffer survived. A throw AFTER
        // the transfer detached it would leave a 0-byte view, and re-posting
        // that means posting the goodbye: the peer would tear down a healthy
        // channel because one send failed.
        if (data.byteLength === 0) throw e
      }
    }
    this.port.postMessage(data)
  }

  // pump delivers buffered messages to h in order until the endpoint dies; on
  // death it flushes what was already received first, then resolves with the
  // death cause (undefined for a clean goodbye). Frames are delivered under a
  // signal that aborts on death: in reliable mode the core may block in
  // backpressure (PROTOCOL.md §4.2), and death detected out of band must
  // unblock it or the §4.5 teardown never runs. The death flush still
  // delivers every frame that fits a stream buffer (the core prefers delivery
  // over a dead signal); only deliveries that would have to block fail their
  // call instead.
  async pump(h: FrameHandler, ctx: FrameContext): Promise<unknown> {
    const dctl = new AbortController()
    void this.dead.wait().then(() => dctl.abort(this.closedErr()))
    const dctx: FrameContext = { ...ctx, signal: dctl.signal }
    for (;;) {
      const data = this.rx.shift()
      if (data !== undefined) {
        // The goodbye is 0 BYTES, not merely an envelop that decoded to no
        // frames: decodeEnvelop skips envelop fields it does not know — a
        // v1.2 extension, another library's protobuf sharing the port — so
        // plenty of messages decode to zero frames, and reading any of them
        // as EOF would tear a healthy channel down over input §4.2 says to
        // drop. Only the empty message can be the close frame: §4.1 carries
        // 1..n frames and encodeEnvelop([]) is exactly 0 bytes, in Go too.
        // Anything else that decodes to no frames is delivered as no frames,
        // i.e. dropped like the malformed message it is.
        if (data.length === 0) {
          // There is no socket whose death could report the peer leaving: the
          // message IS the death signal. Trip the latch so parked sends and
          // blocked deliveries unblock, and leave — the caller runs the §4.5
          // teardown, and no cause is attached because the peer left in an
          // orderly way.
          this.fail(undefined)
          return undefined
        }
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

  // close says goodbye, trips the death latch with the host's cause, and
  // releases the port. The goodbye goes out FIRST and only while this endpoint
  // is still alive: a peer that already left cannot hear it, and a port
  // closed a moment earlier would refuse it. The latch is tripped directly
  // rather than waited for through the stack — a port is not guaranteed to
  // report its own closure (the web platform fires `close` on the remote end)
  // and the teardown must not depend on one. Idempotent.
  //
  // The listeners come off last, after the port is closed: the rx buffer is
  // deliberately left intact for the pump's death flush (§4.2 — a received
  // frame is never dropped), and nothing new can arrive once both the port
  // and the listeners are gone.
  close(cause?: unknown): void {
    if (this.closed) return
    this.closed = true
    if (!this.dead.tripped) {
      try {
        this.port.postMessage(GOODBYE)
      } catch {
        // the peer is already gone; the latch below is what matters
      }
    }
    this.fail(cause)
    // A dedicated worker's `self` is a port whose close() TERMINATES the
    // worker: one client going away would kill the whole instance, discarding
    // every task still queued in it, which is the host's decision to take
    // after its endpoints have torn down — never a side effect of one
    // peer's §4.5 teardown. Detaching is all this endpoint owes such a port.
    if (this.port !== (globalThis as unknown as PortLike)) {
      try {
        this.port.close?.()
      } catch {
        // an already-closed port may throw
      }
    }
    for (const off of this.detach.splice(0)) off()
  }
}

// causeDetail spells a death cause into a message. Unlike the socket
// adapters, whose causes are always Errors from the stack, the cause here
// comes from the host — close('the wasm instance exited') is idiomatic — so
// non-Error values are carried too rather than dropped.
function causeDetail(err: unknown): string {
  if (err === undefined) return ''
  if (err instanceof Error) return `: ${err.message}`
  return `: ${String(err)}`
}

// ---------------------------------------------------------------------------
// client transport
// ---------------------------------------------------------------------------

// PortTransport is the client-side endpoint: one port talking to one server,
// so no peer key is needed (PROTOCOL.md §6.4). It is the tx handler for the
// Conn constructor — implementing the TransportInfo and ConnAttacher
// discovery interfaces directly, so neither is masked by a wrapper. The Conn
// attaches it and the receive pump starts by itself: no user plumbing, and
// conn.close() (or close() here) tears everything down, port included.
//
// Construct it promptly after the port exists — on the same tick as the
// MessageChannel or the Worker — so no early message is lost: a Worker drops
// messages that arrive before a listener is registered.
export class PortTransport implements FrameHandler, TransportInfo, ConnAttacher {
  private readonly pt: Port
  private attached = false

  constructor(port: PortLike, opts: PortOptions = {}) {
    this.pt = new Port(port, opts)
  }

  // reliable reports true: a port neither loses, duplicates nor reorders. The
  // Conn discovers this at construction and disables every protocol timer
  // (PROTOCOL.md §10.6), which is what makes the pump's teardown duty
  // mandatory — nothing else would ever unblock a live call.
  reliable(): boolean {
    return true
  }

  // attachConn is called by the Conn constructor: it starts the receive pump,
  // which runs until the peer says goodbye or close() is called, and on every
  // exit path performs the §4.5 teardown — conn.close(cause) — then releases
  // the port.
  attachConn(conn: Conn): void {
    if (this.attached) throw new Error('port: transport already attached to a Conn')
    this.attached = true
    void (async () => {
      const err = await this.pt.pump(conn, {})
      conn.close(err)
      this.close()
    })()
  }

  // handle posts one frame as a single-frame envelop; an envelop over the
  // size limit is refused before anything reaches the port (PROTOCOL.md
  // §4.4). The frame's signal is not used: a post never parks (see Port.send).
  handle(f: Frame): Promise<void> {
    return this.pt.send([f])
  }

  // close posts the goodbye that lets the peer run its own §4.5 teardown,
  // then closes the port and stops the pump, whose exit fails any live call
  // with `cause` as the detail — say why the endpoint died and every hanging
  // call reports it:
  //
  //   go.run(inst).finally(() => transport.close('the wasm instance exited'))
  //
  // Idempotent — Conn.close calls it, and it is safe to call directly.
  close(cause?: unknown): void {
    this.pt.close(cause)
  }
}

// ---------------------------------------------------------------------------
// server gateway
// ---------------------------------------------------------------------------

interface GwPort {
  pt: Port
  key: number
  served: boolean
}

// PortGateway is the server-side endpoint: one Server serving many peers, one
// port each — a worker pool, or one page hosting several client contexts. It
// is the tx handler for the Server constructor, implementing TransportInfo
// directly so mode discovery is not masked by a wrapper; every port is
// reliable, so unlike the WebRTC gateway there is a single answer to
// advertise.
//
// The peer key is a fresh opaque counter per port, never reused: one port is
// one peer (PROTOCOL.md §6.4).
export class PortGateway implements FrameHandler, TransportInfo {
  private readonly o: PortOptions
  private next = 0
  private readonly ports = new Map<PortLike, GwPort>()
  private readonly peers = new Map<number, Port>()

  constructor(opts: PortOptions = {}) {
    this.o = opts
  }

  // reliable reports true: the Server discovers it once at construction and
  // runs every peer with protocol timers off (PROTOCOL.md §10.6), which is
  // what makes servePeer's teardown duty mandatory.
  reliable(): boolean {
    return true
  }

  // bind registers the gateway's handlers on port and starts buffering its
  // inbound messages; it is idempotent per port. Call it synchronously where
  // the port arrives — inside the `connect` message handler, or right after
  // `new Worker(...)` — so no early message is lost; servePeer binds
  // implicitly:
  //
  //   self.addEventListener('message', (ev) => {
  //     const port = ev.data.port as MessagePort
  //     gw.bind(port)
  //     void gw.servePeer(server, port)
  //   })
  bind(port: PortLike): void {
    this.bindPort(port)
  }

  private bindPort(port: PortLike): GwPort {
    let b = this.ports.get(port)
    if (b === undefined) {
      b = { pt: new Port(port, this.o), key: ++this.next, served: false }
      this.ports.set(port, b)
      this.peers.set(b.key, b.pt)
    }
    return b
  }

  // drop deregisters a peer and releases its port. The close here never posts
  // a goodbye — the pump only exits once this Port is dead, so either close()
  // already said it or the peer is the one who left — but it is what hands
  // the port back to the runtime and takes the listeners off it: a live
  // MessagePort keeps its owner's event loop alive, and a listener left on a
  // Worker keeps this Port alive with it.
  private drop(port: PortLike, b: GwPort): void {
    b.pt.close()
    this.ports.delete(port)
    this.peers.delete(b.key)
  }

  // servePeer delivers port's frames to server under a fresh peer key —
  // annotated reliable, so the peer runs with every timer off (PROTOCOL.md
  // §4.3, §10.6) — until the peer says goodbye, close() runs, or opts.signal
  // aborts. On EVERY exit it performs the §4.5 teardown duty —
  // server.disconnectPeer with the cause — and deregisters the peer: exiting
  // abandons the port (the key is never reused), so the peer's live calls and
  // state must die with it. Resolves with the death cause (undefined for a
  // clean goodbye or a signal abort). Each port is served at most once.
  async servePeer(server: Server, port: PortLike, opts: { signal?: AbortSignal } = {}): Promise<unknown> {
    const b = this.bindPort(port)
    if (b.served) throw new Error('port: port already served')
    b.served = true

    let disposeAbort = noop
    if (opts.signal !== undefined) {
      if (opts.signal.aborted) b.pt.close()
      else disposeAbort = abortListener(opts.signal, () => b.pt.close())
    }
    try {
      const err = await b.pt.pump(server, { peer: b.key, reliable: true })
      server.disconnectPeer(b.key, err)
      return err
    } finally {
      disposeAbort()
      this.drop(port, b)
    }
  }

  // handle posts one frame as a single-frame envelop to the peer named in
  // ctx, with the same size ceiling as the client transport.
  handle(f: Frame, ctx: FrameContext = {}): Promise<void> {
    const key = ctx.peer
    if (typeof key !== 'number') {
      return Promise.reject(new Error(`port: no gateway peer in context (got ${String(key)})`))
    }
    const pt = this.peers.get(key)
    if (pt === undefined) {
      return Promise.reject(new Error(`port: peer ${key} is disconnected`))
    }
    return pt.send([f])
  }

  // close says goodbye on every served port and tears it down; each servePeer
  // then exits through its own §4.5 teardown. This is what a wasm server runs
  // before the instance exits, so the page's clients fail fast instead of
  // waiting on a peer that no longer exists.
  close(): void {
    for (const b of [...this.ports.values()]) b.pt.close()
  }
}
