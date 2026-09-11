// drpc over WebTransport datagrams: one datagram carries one marshaled
// Envelop, the channel is unreliable (drpc's default mode), and nothing is
// ever fragmented — an envelop over the size limit is refused at send with
// MessageTooLargeError, which the core surfaces as RESOURCE_EXHAUSTED on the
// owning call (PROTOCOL.md §4.4). This is the TS twin of the Go
// `transport/webtransport` adapter and interoperates with it on the wire: a
// browser page here talks to a Go `drpc.Server` behind that Gateway.
//
// The session is the platform `WebTransport` — `WebTransportLike` is
// structural, naming only the members the adapter touches, so test mocks and
// non-browser implementations fit, and nothing is imported: no npm
// dependency, no node builtin, safe to bundle for the browser. Only the
// datagram side is used; the session's streams are left alone (a reliable
// per-stream channel is a possible second step, on the pion precedent).
//
// Unlike UDP the session is connection-oriented, so the §4.5 teardown duty
// applies — and it is met from the session, not the read loop: `closed`
// settling (a graceful close resolves it, an abrupt one rejects it) and
// `ready` rejecting (the handshake failed) each call conn.close(cause), and
// conn.close() closes the session. The datagram reader is only a pump; the
// platform closes it in the same cleanup that settles `closed`. Loss is not
// death: a vanished peer is the core's liveness machinery to bound, as on
// UDP, and the adapter runs no keepalive of its own.
//
// The wire is QUIC, so it is always TLS: there is no plaintext path the way
// ws:// is one, and §15's "deploy encrypted" is met by the platform. What
// the platform does not do is authenticate the page — the protocol has no
// authentication either; that stays with the application.

import { Conn, type ConnOptions } from '../../conn'
import type { ConnAttacher, FrameHandler, TransportInfo } from '../../seam'
import { unpack } from '../../seam'
import { Code, MessageTooLargeError, StatusError } from '../../status'
import { Latch, noop } from '../../util'
import { decodeEnvelop, encodeEnvelop, type Frame } from '../../wire'

// DefaultMaxMessageSize is the send ceiling when the session reports no
// maxDatagramSize of its own: one QUIC packet on the typical 1500-byte path
// MTU, with room for the IP/UDP/QUIC headers and the HTTP/3 datagram prefix.
// A session that does report one wins — it knows its path (PROTOCOL.md §4.4).
export const DefaultMaxMessageSize = 1200

// WebTransportLike is the structural subset of the platform WebTransport the
// adapter needs: the two lifecycle promises, the datagram duplex stream, and
// close. The DOM declaration fits it as is; so does a mock built from a
// ReadableStream/WritableStream pair. The adapter owns the session either
// way: it takes the datagram writer and reader for itself.
export interface WebTransportLike {
  // Resolves once the session is established; rejects when the handshake
  // fails (then `closed` rejects with the same error).
  readonly ready: Promise<void>
  // Settles when the session ends: resolves with the close info
  // ({ closeCode, reason }) on a graceful close by either side, rejects on an
  // abrupt one or a failed handshake.
  readonly closed: Promise<unknown>
  readonly datagrams: {
    readonly readable: ReadableStream<Uint8Array>
    // The datagram sink comes in two shapes, and engines ship either: the
    // spec's createWritable() (WebKit ships only this; Gecko both) or the
    // older `writable` attribute (Chromium ships only this, and it is what
    // the DOM lib declares). The adapter takes createWritable() where it
    // exists, else `writable`; a session with neither is refused at
    // construction.
    readonly writable?: WritableStream<Uint8Array>
    createWritable?(): WritableStream<Uint8Array>
    // The platform's datagram ceiling in bytes, as of now: a browser reports
    // a placeholder until `ready` (Chromium: 1024) and the QUIC path's real
    // value after, and may raise it as path MTU discovery proceeds.
    readonly maxDatagramSize?: number
  }
  // The adapter closes with no close info: code 0, no reason.
  close(info?: unknown): void
}

export interface WebTransportDatagramOptions {
  // Largest marshaled Envelop this endpoint will send, in bytes; 0 removes
  // the adapter's check (the platform still drops what its own ceiling
  // refuses — silently). Unset, it follows the session once established —
  // its datagrams.maxDatagramSize at the time of each write, or
  // DefaultMaxMessageSize when the session reports none — and is
  // DefaultMaxMessageSize while the session is still connecting: what a
  // platform reports before `ready` is a placeholder, not the ceiling the
  // datagram will meet. Bounds sends only; receives accept any datagram
  // (PROTOCOL.md §4.4).
  maxMessageSize?: number
}

// WebTransportDialOptions are the platform constructor's options, spelled
// out so no DOM lib is required of the consumer; dialWebTransport passes
// them straight to `new WebTransport(url, options)`.
export interface WebTransportDialOptions {
  // Share a connection with other WebTransport sessions to the same origin.
  // Off by default, and the platform refuses it alongside
  // serverCertificateHashes.
  allowPooling?: boolean
  congestionControl?: 'default' | 'throughput' | 'low-latency'
  // Application protocols to offer (WT-Available-Protocols).
  protocols?: string[]
  // Fail the handshake unless the server supports unreliable delivery — the
  // datagrams this adapter is made of, so it defaults to true here where the
  // platform defaults to false. A session that could only fall back to
  // reliable emulation is not the channel this adapter was chosen for.
  requireUnreliable?: boolean
  // Pins the server certificate by the SHA-256 of its DER encoding, the door
  // for a self-signed development certificate no CA vouches for. The
  // platform only opens it for an X.509v3 certificate valid at most two
  // weeks with an ECDSA P-256 key (never RSA), on a dedicated connection.
  // `value` is a BufferSource in the DOM's sense — a view over a plain
  // ArrayBuffer, never a shared one — so the bag stays assignable to the
  // DOM's WebTransportOptions and back.
  serverCertificateHashes?: { algorithm: string; value: ArrayBuffer | ArrayBufferView<ArrayBuffer> }[]
}

// closeCauseOf maps a graceful close to a death cause: undefined for the
// default close info (code 0, no reason — the peer said goodbye; live calls
// still fail UNAVAILABLE, they just carry no detail), an Error carrying the
// application's code and reason otherwise.
function closeCauseOf(info: unknown): unknown {
  const i = info as { closeCode?: unknown; reason?: unknown } | undefined | null
  const code = typeof i?.closeCode === 'number' ? i.closeCode : 0
  const reason = typeof i?.reason === 'string' && i.reason !== '' ? `: ${i.reason}` : ''
  if (code === 0 && reason === '') return undefined
  return new Error(`webtransport: session closed with code ${code}${reason}`)
}

// bytesOf accepts the chunk shapes a datagram readable may hand out: the
// spec says Uint8Array; a lenient implementation may give another view.
function bytesOf(v: unknown): Uint8Array | undefined {
  if (v instanceof Uint8Array) return v
  if (ArrayBuffer.isView(v)) return new Uint8Array(v.buffer, v.byteOffset, v.byteLength)
  if (v instanceof ArrayBuffer) return new Uint8Array(v)
  return undefined
}

// WebTransportDatagramTransport is the client-side endpoint: one session
// talking to one server, so no peer key is needed (PROTOCOL.md §6.4). It is
// the tx handler for the Conn constructor — implementing the TransportInfo
// and ConnAttacher discovery interfaces directly, so neither is masked by a
// wrapper. The Conn attaches it and the receive pump starts by itself: no
// user plumbing, and conn.close() (or close() here) tears everything down,
// session included.
//
// Nothing is lost by wrapping late: the platform queues inbound datagrams in
// the readable until someone reads (bounded by incomingHighWaterMark, then
// dropped as any datagram may be).
export class WebTransportDatagramTransport implements FrameHandler, TransportInfo, ConnAttacher {
  private readonly wt: WebTransportLike
  private readonly max: number | undefined // explicit ceiling; undefined follows the session
  private readonly writer: WritableStreamDefaultWriter<Uint8Array> | undefined // undefined: the sink refused, the session already dead
  private reader: ReadableStreamDefaultReader<Uint8Array> | undefined

  private readonly opened = new Latch()
  private readonly dead = new Latch()
  private err: unknown // first death cause observed; undefined for a clean close
  private attached = false
  private closed = false

  constructor(wt: WebTransportLike, opts: WebTransportDatagramOptions = {}) {
    this.wt = wt
    this.max = opts.maxMessageSize
    // The datagram sink, in whichever shape the engine ships (see
    // WebTransportLike). createWritable() throws InvalidStateError on a
    // session already closed or failed: a death, reported through the latch
    // like any other so the Conn tears down with the cause, not a
    // construction failure.
    const ds = wt.datagrams
    let sink: WritableStream<Uint8Array> | undefined
    if (typeof ds.createWritable === 'function') {
      try {
        sink = ds.createWritable()
      } catch (e) {
        this.fail(e)
      }
    } else {
      sink = ds.writable
      if (sink === undefined) {
        throw new Error('webtransport: session exposes neither datagrams.createWritable() nor datagrams.writable')
      }
    }
    this.writer = sink?.getWriter()
    // The writer's own closed promise rejects when the session dies; it is
    // never awaited here, and must not surface as an unhandled rejection.
    this.writer?.closed.catch(noop)
    // The session's lifecycle is the death signal (§4.5): it fires whether
    // or not the datagram reader makes progress. On a failed handshake the
    // platform rejects `ready` first and then `closed` with the same error —
    // only the first cause is kept.
    wt.ready.then(
      () => this.opened.trip(),
      (e: unknown) => this.fail(e),
    )
    wt.closed.then(
      (info) => this.fail(closeCauseOf(info)),
      (e: unknown) => this.fail(e),
    )
  }

  // reliable reports false: datagrams lose, duplicate, and reorder (§4.3).
  // The Conn discovers this at construction and runs the full timer
  // machinery, which is what bounds a peer that vanishes without closing.
  reliable(): boolean {
    return false
  }

  // attachConn is called by the Conn constructor: it starts the receive
  // pump, and arms the §4.5 teardown on the session's death — conn.close
  // with the cause — independent of the pump, which is only a reader.
  attachConn(conn: Conn): void {
    if (this.attached) throw new Error('webtransport: transport already attached to a Conn')
    this.attached = true
    // Taken here, synchronously, so a readable someone else already locked
    // fails the Conn constructor rather than a detached promise.
    const reader = this.wt.datagrams.readable.getReader()
    this.reader = reader
    void this.pump(conn, reader)
    void this.dead.wait().then(() => {
      conn.close(this.err)
      this.close()
    })
  }

  // pump delivers each datagram's frames to conn, in order, until the
  // readable ends — which the platform does only in the session cleanup that
  // also settles `closed`, so the loop carries no teardown duty of its own.
  private async pump(conn: Conn, reader: ReadableStreamDefaultReader<Uint8Array>): Promise<void> {
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) return
        // Nothing is delivered after death: past the §4.5 teardown the calls
        // this datagram could belong to are already failed.
        if (this.dead.tripped) return
        const data = bytesOf(value)
        if (data === undefined) continue
        let frames: Frame[]
        try {
          frames = decodeEnvelop(data)
        } catch {
          continue // malformed datagram: dropped, never a teardown (§4.2)
        }
        // Unreliable mode: conn.handle never blocks, so ordered awaited
        // delivery just drains the datagram's frames in order.
        void unpack(frames, conn, {})
      }
    } catch {
      // The readable errored: the session's abrupt cleanup, whose `closed`
      // rejection carries the cause.
    }
  }

  // limit is the send ceiling as of now: the explicit option; else, once
  // the session is established, its word — read per send, because the
  // platform may raise its ceiling over the life of the session — else the
  // default. Before `ready` the session's word is a placeholder (Chromium:
  // 1024), not the ceiling the datagram will actually meet, so the default
  // — the Go twin's constant — stands in until the handshake is done.
  private limit(): number {
    if (this.max !== undefined) return this.max
    if (!this.opened.tripped) return DefaultMaxMessageSize
    const m = this.wt.datagrams.maxDatagramSize
    return typeof m === 'number' && m > 0 ? m : DefaultMaxMessageSize
  }

  // check refuses an envelop over the ceiling as of now (PROTOCOL.md §4.4).
  private check(data: Uint8Array): void {
    const max = this.limit()
    if (max > 0 && data.length > max) {
      throw new MessageTooLargeError(`webtransport: ${data.length}-byte envelop over the ${max}-byte limit`)
    }
  }

  // handle sends one frame as a single-frame envelop, gated on `ready`; an
  // envelop over the ceiling is refused with MessageTooLargeError before the
  // platform, which would drop an oversize datagram silently, ever sees it
  // (PROTOCOL.md §4.4): synchronously here, against the ceiling known now,
  // and once more at the write when the session was still connecting (see
  // send) — a call made on the dial tick queues against the default rather
  // than failing against a placeholder, and is judged against the real
  // ceiling when it goes out. The core treats the rejection as it does the
  // throw: it walks the cause chain for the refusal and reclaims the seq.
  handle(f: Frame): Promise<void> {
    const data = encodeEnvelop([f])
    this.check(data)
    return this.send(data)
  }

  // send writes one datagram once the session is established. There is no
  // stall budget of the adapter's own: in unreliable mode the core's timers
  // bound a call whose datagrams go nowhere (§10), the platform ages queued
  // datagrams out rather than blocking, and a handshake that never completes
  // ends in `ready` rejecting. A write the platform refuses is the session
  // dying; the cause is what `closed` reports.
  private async send(data: Uint8Array): Promise<void> {
    if (!this.opened.tripped) {
      await Promise.race([this.opened.wait(), this.dead.wait()])
      if (this.dead.tripped) throw this.closedErr()
      // Judged against the default while connecting; the ceiling in force
      // now is the one this datagram meets, and still nothing has reached
      // the platform.
      this.check(data)
    }
    const writer = this.writer
    if (writer === undefined || this.dead.tripped) throw this.closedErr()
    try {
      await writer.write(data)
    } catch (e) {
      this.fail(e)
      throw this.closedErr()
    }
  }

  // fail records the first death cause and trips the latch: the teardown
  // armed in attachConn and every parked send wake from it.
  private fail(err: unknown): void {
    if (!this.dead.tripped && this.err === undefined) this.err = err
    this.dead.trip()
  }

  // closedErr is what a send racing the teardown fails with. It is a
  // StatusError so that race is invisible: the core passes a StatusError
  // through unchanged (toStatusError), so the send fails with the very code
  // the §4.5 teardown would have given the call a moment later.
  private closedErr(): StatusError {
    const detail = this.err instanceof Error ? `: ${this.err.message}` : ''
    const e = new StatusError(Code.UNAVAILABLE, `webtransport: session closed${detail}`)
    if (this.err !== undefined) e.cause = this.err
    return e
  }

  // close ends the session and the pump; the attached Conn's live calls fail
  // through the death latch, tripped here directly rather than waited for
  // through the platform — a session still connecting settles `closed` only
  // by rejecting, and the teardown must not depend on when. Idempotent:
  // Conn.close calls it, and it is safe to call directly.
  close(): void {
    if (this.closed) return
    this.closed = true
    this.fail(undefined)
    try {
      this.wt.close()
    } catch {
      // an already-closed session may throw; the latch is what matters
    }
    this.reader?.cancel().catch(noop)
  }
}

// ---------------------------------------------------------------------------
// convenience
// ---------------------------------------------------------------------------

interface WebTransportCtor {
  new (url: string, options?: WebTransportDialOptions): WebTransportLike
}

// dialWebTransport opens a session with the runtime's global WebTransport
// (the browser — Node has none) and hands back a Conn over its datagrams,
// ready to call:
//
//   const conn = dialWebTransport('https://host:4433/rpc') // unreliable mode, full timer machinery
//
// dial is the verb for reaching a peer that already exists, and what it hands
// back is the endpoint you make calls on — the same bargain as Go's net.Dial,
// which returns a net.Conn. It is synchronous and returns before the
// handshake: sends are gated on `ready`, so a call made on this very tick
// queues rather than fails. The URL must be https: — WebTransport has no
// plaintext form.
//
// The options are one bag, and no two of its readers share a key: the
// session takes WebTransportDialOptions (the certificate pins, pooling,
// congestion control, protocols, requireUnreliable), the adapter reads
// maxMessageSize, and the Conn reads everything ConnOptions declares.
//
// Build the pair yourself — `new Conn(new WebTransportDatagramTransport(wt), opts)`
// — when you brought the session (a runtime with no global WebTransport, a
// session needing constructor options this does not expose) or when you need
// the transport object itself.
export function dialWebTransport(url: string, opts: ConnOptions & WebTransportDatagramOptions & WebTransportDialOptions = {}): Conn {
  const ctor = (globalThis as { WebTransport?: WebTransportCtor }).WebTransport
  if (ctor === undefined) {
    throw new Error('webtransport: this runtime has no global WebTransport; construct one and pass it to new WebTransportDatagramTransport()')
  }
  const { allowPooling, congestionControl, protocols, requireUnreliable = true, serverCertificateHashes } = opts
  const wt = new ctor(url, { allowPooling, congestionControl, protocols, requireUnreliable, serverCertificateHashes })
  return new Conn(new WebTransportDatagramTransport(wt, opts), opts)
}
