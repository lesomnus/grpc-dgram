// A user-written Batcher, pinned — the TS half of transport/udp/batcher_test.go.
//
// Batching policy belongs to the workload and the library's job is to provide
// the SEAM (PROTOCOL.md §4.1): what may share a datagram couples the frames'
// fate under loss, and how long a frame may wait for company spends the
// latency budget of §10.7 — neither has an answer that is right for every
// application. So no batcher is added to the library, and nothing in the
// library changes for this file to compile: it is built out of what the
// adapters export today (handle in, sendFrames out).
//
// It lives here, beside the node-udp adapter, because that adapter is the TS
// twin of the Go `transport/udp` this file mirrors, and because a real socket
// lets the test read exactly what left — one datagram, k frames, in order —
// the way the Go test reads its peer socket. It is a file of its own for the
// same reason the Go one is: index.test.ts pins the adapter's own behaviour,
// this pins what a user may build on top of it.
//
// The four properties:
//
//  1. Subclassing keeps every capability the Conn discovers. TS discovers by
//     duck typing (seam.ts), and a subclass inherits attachConn/reliable/close
//     through the prototype; a field-wrapper — the tempting shape — satisfies
//     FrameHandler and loses all three, silently.
//  2. A k-frame envelop the Batcher builds leaves as ONE datagram and arrives
//     as k frames, in order (§4.1) — the receive side has taken 1..n frames
//     per datagram all along, so nothing there changes.
//  3. The §4.4 duty survives the Batcher: a frame that cannot fit the
//     transport's budget fails synchronously out of handle, while the call
//     that owns it is still on the stack.
//  4. Above a GATEWAY the datagram's destination is the ctx of the call that
//     flushes it (§4.1, §6.4), so a batcher there may pack only frames whose
//     contexts name the same peer. The Go file could not show this — its
//     Batcher rides a connected socket with one destination; here it is
//     pinned directly.
//
// Of the Go file's fifth duty JS needs only half. The buffer needs no lock:
// Go's core calls Handle from many goroutines at once, while here it is only
// ever touched inside one synchronous turn of the single-threaded loop, with
// no await splitting it — a batcher that did await mid-buffer would be
// reintroducing the problem by hand. The ordering half still binds, and this
// adapter is the wrong place to see it: a flush on a RELIABLE adapter can
// park (websocket's Socket.send and webrtc's Channel.send wait for the
// channel to open and while bufferedAmount is at the high-water mark), so a
// second flush issued meanwhile overtakes the first and reliable mode has no
// retransmission to repair the reorder (§4.1, §4.3). A batcher there keeps
// one flush in flight. UDP is unreliable and Port.send never awaits, so
// neither this file nor the port adapter can show it.
//
// The policy used below — flush every flushAt frames — is a stand-in, NOT a
// recommendation. It delays frames, which no general-purpose default should
// do; it is merely the cheapest policy that makes a multi-frame datagram
// deterministic in a test. What a real batcher does with fate-sharing, a
// latency budget and its own frame mix is the workload's answer, not this
// file's.

import { createSocket, type Socket } from 'node:dgram'
import { afterEach, describe, expect, it } from 'vitest'
import { Conn } from '../../conn'
import { hasConnAttacher, hasTransportInfo, type FrameHandler } from '../../seam'
import { Server } from '../../server'
import { MessageTooLargeError } from '../../status'
import { echo, registerEcho } from '../../testing'
import { decodeEnvelop, encodeEnvelop, frame, type Frame } from '../../wire'
import { DefaultMaxMessageSize, listenUdp, UdpGateway, UdpTransport } from './index'
import { PortGateway, PortTransport } from '../port/index'
import { DataChannelGateway, DataChannelTransport } from '../webrtc/index'
import { WebSocketGateway, WebSocketTransport } from '../websocket/index'
import { WebTransportDatagramTransport } from '../webtransport/index'

// Everything opened by a test, closed in afterEach: a live socket keeps node's
// event loop alive and the run would never exit.
const opened: { close(): void }[] = []

afterEach(() => {
  for (const c of opened.splice(0)) {
    try {
      c.close()
    } catch {
      // already closed
    }
  }
})

// envelopSize is the marshaled length of the datagram these frames would make.
function envelopSize(frames: readonly Frame[]): number {
  return encodeEnvelop(frames).length
}

// Batcher is what a user writes: a FrameHandler that collects frames and hands
// them to the adapter as one envelop.
//
// It EXTENDS the adapter rather than holding one in a field. The Conn
// discovers capabilities by duck typing — hasConnAttacher, hasTransportInfo,
// and a callable close (seam.ts, conn.ts) — and a subclass inherits all three
// through the prototype, so overriding handle costs nothing. A field-wrapper
// would satisfy FrameHandler and lose them all (see FieldBatcher).
class Batcher extends UdpTransport {
  private pending: Frame[] = []
  // Envelops actually handed to the adapter, i.e. datagrams written.
  datagrams = 0

  constructor(
    socket: Socket,
    private readonly flushAt: number,
    // budget is the adapter's own send budget, not a second one: the same
    // number goes to the adapter below. The Batcher sits under the core and
    // over the socket, so it is the last place that can weigh a frame against
    // that budget while the owning call is still on the stack. (It is not
    // named `max`: the adapter's own ceiling is a private field of that name,
    // and TypeScript will not let a subclass redeclare one.)
    private readonly budget: number = DefaultMaxMessageSize,
  ) {
    super(socket, { maxMessageSize: budget })
  }

  get pendingLen(): number {
    return this.pending.length
  }

  // handle is the only method the Batcher overrides.
  override handle(f: Frame): Promise<void> {
    // PROTOCOL.md §4.4: the core never fragments, and a message that cannot
    // fit the channel must fail the call that owns it. handle is the last
    // moment at which that call is reachable, and deferring the check to the
    // flush does not merely lose the error: the flush runs under whichever
    // OTHER call's handle happened to trigger it, so that call is told its
    // frame was too large — and the core believes it, reclaiming that call's
    // seq while the frame that really overran is dropped in silence.
    const n = envelopSize([f])
    if (this.budget > 0 && n > this.budget) {
      throw new MessageTooLargeError(`batcher: ${n}-byte frame over the ${this.budget}-byte budget`)
    }

    let batch: Frame[] | undefined
    if (this.pending.length > 0 && envelopSize([...this.pending, f]) > this.budget) {
      // f does not fit alongside what is already waiting: what is waiting goes
      // now, in order, and f opens the next envelop.
      batch = this.pending
      this.pending = [f]
    } else {
      this.pending.push(f)
      if (this.pending.length >= this.flushAt) {
        batch = this.pending
        this.pending = []
      }
    }
    if (batch === undefined) return Promise.resolve()

    this.datagrams++
    // sendFrames is the exported envelop-level seam: one envelop of 1..n
    // frames, one datagram (§4.1).
    return this.sendFrames(batch)
  }
}

// FieldBatcher is the same Batcher written the tempting way: the adapter in a
// field instead of a base class. It satisfies FrameHandler, so `new
// Conn(fieldBatcher)` compiles and is accepted — and it is missing everything
// the Conn discovers. It exists only to keep the contrast below honest.
class FieldBatcher implements FrameHandler {
  constructor(private readonly tx: UdpTransport) {}
  handle(f: Frame): Promise<void> {
    return this.tx.sendFrames([f])
  }
}

// dataFrame is a plausible client data frame: the shape of what the core hands
// a tx, which is all a batcher ever sees.
function dataFrame(sid: number, seq: number, payload: Uint8Array): Frame {
  return frame({ epoch: 0x0badcafe, sid, seq, payload })
}

function bind(sock: Socket, port = 0): Promise<Socket> {
  opened.push(sock)
  return new Promise((res, rej) => {
    sock.once('error', rej)
    sock.bind(port, '127.0.0.1', () => {
      sock.off('error', rej)
      res(sock)
    })
  })
}

function connect(sock: Socket, port: number): Promise<Socket> {
  opened.push(sock)
  return new Promise((res, rej) => {
    sock.once('error', rej)
    sock.connect(port, '127.0.0.1', () => {
      sock.off('error', rej)
      res(sock)
    })
  })
}

// recv resolves with the next datagram's bytes, or rejects on a timeout: every
// assertion about what left the wire is made on real bytes.
function recv(sock: Socket, ms = 2000): Promise<Uint8Array> {
  return new Promise((res, rej) => {
    const t = setTimeout(() => {
      sock.off('message', onMsg)
      rej(new Error(`no datagram within ${ms}ms`))
    }, ms)
    const onMsg = (data: Buffer): void => {
      clearTimeout(t)
      res(new Uint8Array(data.buffer, data.byteOffset, data.byteLength))
    }
    sock.once('message', onMsg)
  })
}

// silent asserts nothing else left: the k frames cost one write, not k.
async function silent(sock: Socket, ms = 150): Promise<void> {
  await expect(recv(sock, ms)).rejects.toThrow(/no datagram/)
}

// dialBatcher gives back a Batcher over a connected socket and the socket that
// plays the peer, so a test can read what actually left.
async function dialBatcher(flushAt: number, max = DefaultMaxMessageSize): Promise<{ b: Batcher; peer: Socket }> {
  const peer = await bind(createSocket('udp4'))
  const sock = await connect(createSocket('udp4'), peer.address().port)
  return { b: new Batcher(sock, flushAt, max), peer }
}

describe('a user-written Batcher over the node-udp adapter', () => {
  // Subclassing is what keeps the capabilities reachable. The Conn does not
  // ask for them; it duck-types the tx it is handed, so a wrapper that hides
  // one loses the behaviour with no error at all.
  it('keeps every capability the Conn discovers', async () => {
    const { b } = await dialBatcher(2)

    // The seam itself (conn.ts): without it the Batcher is not a transport.
    expect(typeof b.handle).toBe('function')
    // conn.ts, at the end of the constructor. attachConn is what starts the
    // adapter's receive pump. Hide it and the Conn simply never calls it: the
    // endpoint sends fine and receives NOTHING, forever, with no error
    // anywhere — every call dies of its deadline instead.
    expect(hasConnAttacher(b)).toBe(true)
    // timing discovery (conn.ts, `hasTransportInfo(tx) ? tx.reliable() : false`).
    // Hide it and this UDP endpoint is taken for the default — unreliable —
    // which happens to be right here but is wrong for any reliable adapter
    // wrapped the same way: no timers, over a channel that loses.
    expect(hasTransportInfo(b)).toBe(true)
    expect(b.reliable()).toBe(false)
    // Conn.close calls a close() the tx exposes. Hide it and conn.close()
    // leaves the socket open and the receive pump alive — a leak per endpoint.
    expect(typeof (b as { close?: unknown }).close).toBe('function')

    // The contrast, so none of the above is a claim about a hypothetical: the
    // same Batcher with the adapter in a FIELD keeps only the seam it declares
    // itself, and loses the rest without a word from the compiler.
    const field = new FieldBatcher(b)
    expect(typeof field.handle).toBe('function')
    expect(hasConnAttacher(field)).toBe(false)
    expect(hasTransportInfo(field)).toBe(false)
    expect(typeof (field as { close?: unknown }).close).toBe('undefined')
  })

  // The property the whole argument rests on: k frames the Batcher collects
  // reach the wire as one datagram, and the receive side — unchanged — takes
  // all k out of it in order. The saving is exactly the k−1 writes and the k−1
  // IP/UDP headers this avoids.
  it('sends one datagram per batch, and the receive side reads k frames in order', async () => {
    const k = 3
    const { b, peer } = await dialBatcher(k)

    for (let i = 1; i <= k; i++) {
      await b.handle(dataFrame(i, i * 10, new Uint8Array([i])))
    }
    expect(b.datagrams).toBe(1)

    const data = await recv(peer)
    // decodeEnvelop is what an adapter runs on the receive path; driving it
    // here is the proof that a batched datagram needs no receive-side change.
    const frames = decodeEnvelop(data)
    // Order is not decoration: within one envelop the frames of a stream are
    // delivered in the order they were packed (§4.1).
    expect(frames.map((f) => [f.sid, f.seq])).toEqual([
      [1, 10],
      [2, 20],
      [3, 30],
    ])
    await silent(peer)
  })

  // The §4.4 duty through the Batcher. The core maps MessageTooLargeError to
  // RESOURCE_EXHAUSTED on the call that produced the frame, which only works
  // while that call is still waiting on handle — so the check cannot be
  // deferred to the flush.
  it('fails an oversize frame synchronously, and sends nothing', async () => {
    const { b, peer } = await dialBatcher(2)

    const huge = dataFrame(1, 1, new Uint8Array(2 * DefaultMaxMessageSize))
    expect(() => b.handle(huge)).toThrow(MessageTooLargeError)
    // Failed, not buffered: a frame parked for a later flush would take the
    // error away from its owning call and turn a RESOURCE_EXHAUSTED into a
    // call that hangs until its deadline.
    expect(b.pendingLen).toBe(0)
    expect(b.datagrams).toBe(0)

    // The seam keeps the duty too, for a batcher that packs past the budget:
    // the refusal is synchronous there as well, before any datagram exists.
    expect(() => b.sendFrames([huge])).toThrow(MessageTooLargeError)

    // A frame that does fit still flows: the refusal is per message, and the
    // transport is not broken by it (§4.4).
    for (let i = 1; i <= 2; i++) {
      await b.handle(dataFrame(i, i, new TextEncoder().encode('ok')))
    }
    expect(decodeEnvelop(await recv(peer))).toHaveLength(2)
  })

  // Real RPCs over a Conn whose transport is the Batcher — the shape a user
  // actually gets. It is what proves the inherited attachConn is not merely
  // present but effective: were it hidden, no response would ever be read and
  // the call below would die of its deadline.
  //
  // flushAt is 1 here, so the Batcher adds no delay of its own: a batcher that
  // waits for a partner needs a second frame to exist, and inventing one is a
  // policy question the library refuses to answer. The multi-frame datagram
  // itself is pinned above, on the wire, where it is deterministic.
  it('carries real calls, through the inherited receive pump', async () => {
    const { gateway, port } = await listenUdp(0, '127.0.0.1')
    opened.push(gateway)
    const server = new Server(gateway)
    registerEcho(server)
    void gateway.serve(server)

    const sock = await connect(createSocket('udp4'), port)
    const b = new Batcher(sock, 1)
    const conn = new Conn(b) // attachConn starts the receive pump
    try {
      await expect(conn.invoke(echo.once, { text: 'abc' })).resolves.toEqual({ text: 'echo:abc' })
      // Every frame this endpoint sent went out through the Batcher.
      expect(b.datagrams).toBeGreaterThan(0)
    } finally {
      conn.close() // the transport and its socket go with it
    }
  })
})

// GatewayBatcher is the duty the Go file says it cannot show: above a gateway
// the datagram's destination is the ctx of the call that flushes it (§4.1,
// §6.4), and a server hands its tx a different per-peer ctx per peer. So the
// buffer is keyed by peer and a flush carries the ctx that named it.
class GatewayBatcher extends UdpGateway {
  private readonly pending = new Map<string, Frame[]>()
  datagrams = 0

  constructor(
    socket: Socket,
    private readonly flushAt: number,
  ) {
    super(socket)
  }

  override handle(f: Frame, ctx: { peer?: unknown } = {}): Promise<void> {
    const key = ctx.peer
    if (typeof key !== 'string') return Promise.reject(new Error('batcher: no peer in context'))
    const pending = this.pending.get(key) ?? []
    pending.push(f)
    if (pending.length < this.flushAt) {
      this.pending.set(key, pending)
      return Promise.resolve()
    }
    this.pending.delete(key)
    this.datagrams++
    // The ctx goes with the batch: it IS the address. Flushing peer A's frames
    // under peer B's ctx sends them to B and nothing to A, and nothing
    // anywhere reports it.
    return this.sendFrames(pending, ctx)
  }
}

describe('a user-written Batcher over the node-udp gateway', () => {
  it('packs per peer and flushes each batch to the peer its ctx names', async () => {
    const gwSock = await bind(createSocket('udp4'))
    const gw = new GatewayBatcher(gwSock, 2)
    opened.push(gw)
    // A Server is what learns the peers; nothing is registered on it, because
    // what is asserted here is addressing, not dispatch.
    void gw.serve(new Server(gw))

    // Two peers, each announced by a datagram of its own: the gateway keys a
    // peer by its source address:port the moment it hears from it.
    const a = await bind(createSocket('udp4'))
    const b = await bind(createSocket('udp4'))
    const keyA = `127.0.0.1:${a.address().port}`
    const keyB = `127.0.0.1:${b.address().port}`
    for (const s of [a, b]) s.send(encodeEnvelop([]), gwSock.address().port, '127.0.0.1')
    await new Promise((res) => setTimeout(res, 50))

    // Interleaved, as a server serving two peers at once would emit them.
    await gw.handle(dataFrame(1, 1, new Uint8Array([0xa1])), { peer: keyA })
    await gw.handle(dataFrame(2, 1, new Uint8Array([0xb1])), { peer: keyB })
    await gw.handle(dataFrame(1, 2, new Uint8Array([0xa2])), { peer: keyA })
    await gw.handle(dataFrame(2, 2, new Uint8Array([0xb2])), { peer: keyB })
    expect(gw.datagrams).toBe(2)

    // Each peer got ONE datagram, carrying its own two frames in order — and
    // nothing of the other's.
    expect(decodeEnvelop(await recv(a)).map((f) => [f.sid, f.seq])).toEqual([
      [1, 1],
      [1, 2],
    ])
    expect(decodeEnvelop(await recv(b)).map((f) => [f.sid, f.seq])).toEqual([
      [2, 1],
      [2, 2],
    ])
    await silent(a)
    await silent(b)
  })
})

// The seam is one name on every adapter, transport and gateway alike: a
// batching middleware written against one of them is written against all of
// them, and a renamed or missing entry here is what would make that false.
describe('the envelop-level seam', () => {
  it('is sendFrames on every exported adapter class', () => {
    const classes = [
      UdpTransport,
      UdpGateway,
      WebTransportDatagramTransport,
      WebSocketTransport,
      WebSocketGateway,
      DataChannelTransport,
      DataChannelGateway,
      PortTransport,
      PortGateway,
    ]
    for (const c of classes) {
      expect(typeof (c.prototype as { sendFrames?: unknown }).sendFrames, c.name).toBe('function')
      expect(typeof (c.prototype as { handle?: unknown }).handle, c.name).toBe('function')
    }
  })
})
