// The message-port adapter (src/transport/port/index.ts) against two kinds of
// channel: a real MessageChannel (node's is the platform one) for the shapes
// that must work end to end, and a mock port pair for the paths a real port
// cannot produce — a postMessage that throws, a port with only `onmessage`, a
// port that refuses transfer lists, and a channel where nothing but messages
// crosses, which is what proves the empty-envelop goodbye (§4.5) does the
// teardown by itself.
//
// Covers the reliable echo round trip with zero mode options (the TS side of
// the Go jsport pair), all four RPC shapes, ordering under load, the §4.4 size
// ceiling, the §4.5 teardown on close and on the goodbye, the post-close
// delivery ban, and that a malformed message is ignored rather than fatal.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../../conn'
import { Server } from '../../server'
import { Code, type StatusError } from '../../status'
import { echo, registerEcho, tick } from '../../testing'
import { FlagPing, frame } from '../../wire'
import { dialWorker, PortGateway, PortTransport, type PortLike, type PortOptions, type WorkerLike } from './index'

// Every real port opened by a test, closed in afterEach: a live MessagePort
// keeps node's event loop alive and the run would never exit.
const opened: MessagePort[] = []

function channel(): [MessagePort, MessagePort] {
  const ch = new MessageChannel()
  opened.push(ch.port1, ch.port2)
  return [ch.port1, ch.port2]
}

afterEach(() => {
  for (const p of opened.splice(0)) p.close()
})

// settle drains both queues: a real port delivers on a macrotask, and each
// delivered frame then unwinds through promise chains.
async function settle(rounds = 3): Promise<void> {
  for (let i = 0; i < rounds; i++) {
    await new Promise((res) => setTimeout(res, 0))
    await tick()
  }
}

// realEnds builds the whole shape over a real MessageChannel: a client Conn on
// one port, a Server behind a Gateway on the other.
function realEnds(opts: { client?: PortOptions } = {}) {
  const [a, b] = channel()
  const gateway = new PortGateway()
  const server = new Server(gateway)
  const counts = registerEcho(server)
  gateway.bind(b)
  const serving = gateway.servePeer(server, b)
  const transport = new PortTransport(a, opts.client)
  const conn = new Conn(transport) // attachConn starts the pump
  return { a, b, conn, server, gateway, counts, serving, transport }
}

// MockPort is a message-port stand-in: postMessage on one end delivers a
// Uint8Array 'message' to the other and nothing else ever crosses — closing
// one end is invisible to the peer, exactly like a wasm instance that stopped
// answering. `legacy: true` grows the older shape instead: no
// addEventListener and no start(), so the adapter must fall back to the on*
// properties.
class MockPort implements PortLike {
  peer: MockPort | undefined
  sent: Uint8Array[] = []
  transfers = 0
  starts = 0
  closes = 0
  refuseTransfer = false // a port whose postMessage rejects transfer lists
  throwAfterTransfer = false // ...and one that throws once the buffer is gone
  postError: Error | undefined
  onmessage: ((ev: never) => void) | null = null
  onmessageerror: ((ev: never) => void) | null = null
  addEventListener?: (type: string, fn: (ev: unknown) => void) => void
  removeEventListener?: (type: string, fn: (ev: unknown) => void) => void
  start?: () => void
  private live = true
  private readonly listeners = new Map<string, ((ev: unknown) => void)[]>()

  constructor(legacy = false) {
    if (legacy) return
    this.addEventListener = (type, fn) => {
      const l = this.listeners.get(type) ?? []
      l.push(fn)
      this.listeners.set(type, l)
    }
    this.removeEventListener = (type, fn) => {
      const l = (this.listeners.get(type) ?? []).filter((x) => x !== fn)
      if (l.length === 0) this.listeners.delete(type)
      else this.listeners.set(type, l)
    }
    this.start = () => {
      this.starts++
    }
  }

  // listenerCount is how the tests see whether the endpoint let go of a port
  // it no longer owns — a Worker and a worker's `self` outlive the endpoint.
  get listenerCount(): number {
    let n = 0
    for (const l of this.listeners.values()) n += l.length
    return n
  }

  emit(type: string, ev: unknown = {}): void {
    for (const fn of this.listeners.get(type) ?? []) fn(ev)
    const on = type === 'message' ? this.onmessage : type === 'messageerror' ? this.onmessageerror : null
    ;(on as ((ev: unknown) => void) | null)?.(ev)
  }

  postMessage(data: Uint8Array, transfer?: unknown[]): void {
    if (this.postError !== undefined) throw this.postError
    let msg: Uint8Array
    if (transfer === undefined) {
      // A detached view clones as empty here rather than throwing: V8 throws,
      // but a lenient host would put those 0 bytes on the wire — and 0 bytes
      // is the goodbye. Standing in for that host is what gives the "never
      // re-posts a detached buffer" assertion below something to catch.
      msg = data.byteLength === 0 ? new Uint8Array(0) : data.slice()
    } else {
      if (this.refuseTransfer) throw new Error('DataCloneError: transfer list not supported')
      this.transfers++
      msg = structuredClone(data, { transfer: transfer as Transferable[] }) // detaches, as a real port does
      if (this.throwAfterTransfer) throw new Error('port: post failed after the transfer')
    }
    this.sent.push(msg)
    const peer = this.peer
    if (peer !== undefined) {
      queueMicrotask(() => {
        if (peer.live) peer.emit('message', { data: msg })
      })
    }
  }

  close(): void {
    this.closes++
    this.live = false
  }
}

function mockPair(legacy = false): [MockPort, MockPort] {
  const a = new MockPort(legacy)
  const b = new MockPort(legacy)
  a.peer = b
  b.peer = a
  return [a, b]
}

function mockEnds(opts: { legacy?: boolean; client?: PortOptions } = {}) {
  const [a, b] = mockPair(opts.legacy)
  const gateway = new PortGateway()
  const server = new Server(gateway)
  const counts = registerEcho(server)
  gateway.bind(b)
  const serving = gateway.servePeer(server, b)
  const transport = new PortTransport(a, opts.client)
  const conn = new Conn(transport)
  return { a, b, conn, server, gateway, counts, serving, transport }
}

describe('reliable port echo (the jsport pair, TS side)', () => {
  it('echoes a unary call over a real MessageChannel, zero mode options', async () => {
    const net = realEnds()
    expect(net.conn.reliable).toBe(true) // discovered via TransportInfo, not configured
    expect(net.gateway.reliable()).toBe(true)
    expect(await net.conn.invoke(echo.once, { text: 'hello' })).toEqual({ text: 'echo:hello' })
    net.conn.close()
    await net.serving
  })

  it('runs all four RPC types', async () => {
    const net = realEnds()

    expect(await net.conn.invoke(echo.once, { text: 'u' })).toEqual({ text: 'echo:u' })

    const many = net.conn.newStream(echo.many, {})
    await many.send({ text: 'm', n: 3 })
    const got: string[] = []
    for await (const m of many) got.push(m.text)
    expect(got).toEqual(['m#0', 'm#1', 'm#2'])

    const count = net.conn.newStream(echo.count, {})
    await count.send({ text: 'a' })
    await count.send({ text: 'b' })
    count.closeSend()
    expect(await count.recv()).toEqual({ text: '2' })

    const live = net.conn.newStream(echo.live, {})
    await live.send({ text: 'x' })
    expect(await live.recv()).toEqual({ text: 'echo:x' })
    live.closeSend()
    expect(await live.recv()).toBeUndefined()

    net.conn.close()
    await net.serving
  })

  it('delivers a long stream in order, past the flow-control window', async () => {
    const net = realEnds()
    const n = 100 // > W_INIT (32): the run is paced by real WINDOW grants
    const many = net.conn.newStream(echo.many, {})
    await many.send({ text: 'seq', n })
    const got: string[] = []
    for await (const m of many) got.push(m.text)
    expect(got).toEqual(Array.from({ length: n }, (_, i) => `seq#${i}`))
    net.conn.close()
    await net.serving
  })

  it('carries a 256 KiB envelop: no size ceiling by default (§4.4)', async () => {
    const net = realEnds()
    const text = 'x'.repeat(256 * 1024)
    expect(await net.conn.invoke(echo.once, { text })).toEqual({ text: `echo:${text}` })
    net.conn.close()
    await net.serving
  })

  it('starts a port that has a start(), on both roles', async () => {
    // A browser MessagePort wired through addEventListener stays paused until
    // start(), and the symptom is silence rather than an error — node's port
    // happens to auto-start, so only a mock can pin this.
    const net = mockEnds()
    expect(net.a.starts).toBe(1)
    expect(net.b.starts).toBe(1)
    expect(await net.conn.invoke(echo.once, { text: 'started' })).toEqual({ text: 'echo:started' })
    net.conn.close()
    await net.serving
  })

  it('works on a port with only onmessage and no start()', async () => {
    const net = mockEnds({ legacy: true })
    expect(await net.conn.invoke(echo.once, { text: 'legacy' })).toEqual({ text: 'echo:legacy' })
    const live = net.conn.newStream(echo.live, {})
    await live.send({ text: 'x' })
    expect(await live.recv()).toEqual({ text: 'echo:x' })
    net.conn.close()
    await net.serving
  })
})

describe('one message per envelop, transferred (§4.1)', () => {
  it('hands the buffer over instead of copying it', async () => {
    const net = mockEnds()
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    expect(net.a.sent).toHaveLength(1) // one marshaled Envelop per message
    expect(net.a.transfers).toBe(1)
    net.conn.close()
    await net.serving
  })

  it('falls back to a copy on a port that refuses transfer lists', async () => {
    const [a, b] = mockPair()
    a.refuseTransfer = true
    b.refuseTransfer = true
    const gateway = new PortGateway()
    const server = new Server(gateway)
    registerEcho(server)
    gateway.bind(b)
    const serving = gateway.servePeer(server, b)
    const conn = new Conn(new PortTransport(a))
    expect(await conn.invoke(echo.once, { text: 'copy' })).toEqual({ text: 'echo:copy' })
    expect(a.transfers).toBe(0)
    conn.close()
    await serving
  })

  it('never re-posts a detached buffer: that would be a spurious goodbye', async () => {
    const [a] = mockPair()
    a.throwAfterTransfer = true
    const conn = new Conn(new PortTransport(a))
    const err = (await conn.invoke(echo.once, { text: 'x' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE) // the failed post is transport death
    // The retry would have posted the emptied view — a 0-byte message, which
    // IS the goodbye (§4.5). Nothing of the sort left this endpoint.
    expect(a.sent.some((m) => m.length === 0)).toBe(false)
  })

  it('turns off transfer on request', async () => {
    const net = mockEnds({ client: { transfer: false } })
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    expect(net.a.transfers).toBe(0)
    net.conn.close()
    await net.serving
  })
})

describe('message size (§4.4)', () => {
  it('refuses an oversize envelop and the call fails RESOURCE_EXHAUSTED', async () => {
    const net = realEnds({ client: { maxMessageSize: 128 } })
    const err = (await net.conn.invoke(echo.once, { text: 'x'.repeat(500) }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED)
    // The channel survives a refused send: a small call still works.
    expect(await net.conn.invoke(echo.once, { text: 'ok' })).toEqual({ text: 'echo:ok' })
    net.conn.close()
    await net.serving
  })
})

describe('teardown duty (§4.5)', () => {
  it('closing the client makes servePeer return and cancels the server handler', async () => {
    const [a, b] = channel()
    const gateway = new PortGateway()
    const server = new Server(gateway)
    let handlerErr: StatusError | undefined
    server.register(echo.live, async (stream) => {
      try {
        for await (const _ of stream) {
          /* consume until the peer goes away */
        }
      } catch (e) {
        handlerErr = e as StatusError
      }
    })
    gateway.bind(b)
    const serving = gateway.servePeer(server, b)
    const conn = new Conn(new PortTransport(a))

    const stream = conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    await settle()

    conn.close() // the goodbye goes out, then the port closes
    expect(await serving).toBeUndefined() // an orderly goodbye carries no cause
    await settle()
    expect(handlerErr?.code).toBe(Code.UNAVAILABLE) // servePeer ran disconnectPeer on exit
  })

  it('the empty-envelop goodbye is the whole teardown: nothing else crosses', async () => {
    const net = mockEnds()
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.gateway.close() // a wasm server shutting down
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    // The mock propagates no close and no error: the last thing the server
    // posted was a 0-byte envelop, and that alone failed the live call.
    expect(net.b.sent.at(-1)).toHaveLength(0)
    expect(await net.serving).toBeUndefined()
    await tick()
    expect(() => net.conn.newStream(echo.live, {})).toThrow(/connection is closed/)
  })

  it('close(cause) tells live calls why the endpoint died', async () => {
    const net = mockEnds()
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'x' })
    expect(await stream.recv()).toEqual({ text: 'echo:x' })

    net.transport.close('the wasm instance exited')
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/wasm instance exited/)
    // The peer still hears the goodbye, so its side tears down too.
    expect(net.a.sent.at(-1)).toHaveLength(0)
    expect(await net.serving).toBeUndefined()
  })

  it('a send after death fails UNAVAILABLE with the cause', async () => {
    const [a] = mockPair()
    const transport = new PortTransport(a)
    const conn = new Conn(transport)
    transport.close(new Error('worker terminated'))
    const err = (await conn.invoke(echo.once, { text: 'x' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    expect(err.message).toMatch(/worker terminated/)
  })

  it('takes its listeners off a port it no longer owns', async () => {
    const net = mockEnds()
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    expect(net.a.listenerCount).toBeGreaterThan(0)

    net.transport.close()
    await net.serving
    // A Worker, and a worker's `self`, outlive the endpoint and have no
    // close() that would release them: a listener left behind keeps this
    // endpoint — rx buffer included — alive for as long as the port is, and
    // would hand a later endpoint bound to the same port a duplicate of every
    // message.
    expect(net.a.listenerCount).toBe(0)
    expect(net.b.listenerCount).toBe(0)
  })

  it('clears the on* handlers of a legacy port', async () => {
    const net = mockEnds({ legacy: true })
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    expect(net.a.onmessage).not.toBeNull()

    net.conn.close()
    await net.serving
    expect(net.a.onmessage).toBeNull()
    expect(net.a.onmessageerror).toBeNull()
    expect(net.b.onmessage).toBeNull()
  })

  it("never closes the global scope: a worker's `self` is the host's to kill", () => {
    // `self` inside a dedicated worker is a port, but its close() TERMINATES
    // the worker, discarding every task still queued in it. One peer's §4.5
    // teardown must not take the whole instance with it — the goodbye still
    // goes out, and detaching is all the endpoint owes such a port.
    const g = globalThis as unknown as PortLike
    const sent: Uint8Array[] = []
    let closes = 0
    g.postMessage = (d: Uint8Array) => {
      sent.push(d)
    }
    g.close = () => {
      closes++
    }
    try {
      new PortTransport(g).close()
      expect(sent.map((m) => m.length)).toEqual([0]) // the goodbye, and only it
      expect(closes).toBe(0)
      expect(g.onmessage).toBeNull()
    } finally {
      for (const k of ['postMessage', 'close', 'onmessage', 'onmessageerror']) Reflect.deleteProperty(globalThis, k)
    }
  })

  it('close is idempotent and safe from both sides', async () => {
    const net = mockEnds()
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    net.transport.close()
    net.transport.close()
    net.conn.close() // Conn.close calls the transport's close again
    await net.serving
    net.gateway.close()
    expect(net.a.closes).toBe(1)
    expect(net.a.sent.filter((m) => m.length === 0)).toHaveLength(1) // exactly one goodbye
    expect(net.b.closes).toBe(1)
  })
})

describe('nothing is delivered after close (§4.5)', () => {
  it('a message arriving after the goodbye is never handed to the core', async () => {
    const net = mockEnds()
    expect(await net.conn.invoke(echo.once, { text: 'live' })).toEqual({ text: 'echo:live' })
    expect(net.counts.once).toBe(1)
    const replay = net.a.sent[0] // the OPEN envelop the server already handled
    expect(replay).toBeDefined()

    const spy = vi.spyOn(net.server, 'handle')
    net.transport.close()
    await net.serving

    net.b.emit('message', { data: replay!.slice() }) // late arrival
    await tick()
    expect(spy).not.toHaveBeenCalled()
    expect(net.counts.once).toBe(1) // the handler did not run a second time
  })

  it('a client that dies mid-flight delivers nothing further to its Conn', async () => {
    const [a] = mockPair()
    const transport = new PortTransport(a)
    const conn = { handle: vi.fn(), close: vi.fn() }
    transport.attachConn(conn as unknown as Conn)
    await tick()

    transport.close()
    await tick()
    expect(conn.close).toHaveBeenCalledTimes(1) // the §4.5 teardown, exactly once
    a.emit('message', { data: new Uint8Array([0x0a, 0x00]) }) // a well-formed envelop
    await tick()
    expect(conn.handle).not.toHaveBeenCalled()
  })
})

describe('malformed messages are ignored, never fatal (§4.2)', () => {
  it('survives a string, an object and undecodable bytes', async () => {
    const net = realEnds()
    expect(await net.conn.invoke(echo.once, { text: 'before' })).toEqual({ text: 'echo:before' })

    net.a.postMessage('hello' as unknown as Uint8Array) // some other library sharing the port
    net.a.postMessage({ kind: 'not-an-envelop' } as unknown as Uint8Array)
    net.a.postMessage(new Uint8Array([0xff, 0xff, 0xff])) // truncated varint
    await settle()

    expect(await net.conn.invoke(echo.once, { text: 'after' })).toEqual({ text: 'echo:after' })
    expect(net.counts.once).toBe(2)
    net.conn.close()
    await net.serving
  })

  it('is not fooled by a message that decodes to no frames: only 0 bytes is the goodbye', async () => {
    const net = mockEnds()
    expect(await net.conn.invoke(echo.once, { text: 'before' })).toEqual({ text: 'echo:before' })

    const exited = vi.fn()
    void net.serving.then(exited)
    // A well-formed protobuf whose only field is one the envelop does not
    // know — a v1.2 extension, or another library's message sharing the port.
    // decodeEnvelop skips unknown fields, so it yields zero frames; reading
    // THAT as the peer's close frame would tear a healthy channel down over
    // input §4.2 says to drop. The close frame is the empty message, nothing
    // else.
    net.a.postMessage(new Uint8Array([0x10, 0x01]))
    await settle()
    expect(exited).not.toHaveBeenCalled()

    expect(await net.conn.invoke(echo.once, { text: 'after' })).toEqual({ text: 'echo:after' })
    expect(net.counts.once).toBe(2)
    net.conn.close()
    await net.serving
  })

  it('ignores a messageerror: an undeserializable message is not death', async () => {
    const net = mockEnds()
    net.b.emit('messageerror', {})
    await tick()
    expect(await net.conn.invoke(echo.once, { text: 'alive' })).toEqual({ text: 'echo:alive' })
    net.conn.close()
    await net.serving
  })
})

describe('dialWorker: bring your own worker', () => {
  // A worker seen from the thread that made it, reduced to what dialWorker
  // touches — and, on the far side, the worker's own half: it takes the port
  // off the transfer list (where a transferred port always arrives, whatever
  // message came with it) and serves it off a real Server.
  class MockWorker implements WorkerLike {
    readonly messages: unknown[] = []
    readonly taken: MessagePort[] = []
    private readonly pending: MessagePort[] = []
    terminated = 0
    private readonly gateway = new PortGateway()
    readonly server = new Server(this.gateway)
    readonly counts = registerEcho(this.server)
    // Off, the port is held: an instance that has not bound what it was handed
    // yet, which is the normal case for a call opened on the same tick.
    autoServe = true

    postMessage(message: unknown, transfer?: unknown[]): void {
      this.messages.push(message)
      const port = transfer?.[0]
      if (port instanceof MessagePort) {
        this.taken.push(port)
        this.pending.push(port)
        opened.push(port)
        if (this.autoServe) this.serve()
      }
    }

    serve(): void {
      for (const port of this.pending.splice(0)) {
        this.gateway.bind(port)
        void this.gateway.servePeer(this.server, port)
      }
    }

    terminate(): void {
      this.terminated++
    }
  }

  it('connects over a transferred port, and again for a second peer', async () => {
    const worker = new MockWorker()
    const first = dialWorker(worker)
    const second = dialWorker(worker)
    expect(await first.invoke(echo.once, { text: 'a' })).toEqual({ text: 'echo:a' })
    expect(await second.invoke(echo.once, { text: 'b' })).toEqual({ text: 'echo:b' })
    // One port is one peer (§6.4), so these are two independent connections to
    // one worker, not two views of one.
    expect(worker.taken).toHaveLength(2)
    expect(worker.counts.once).toBe(2)
    // The default message is the word the shipped wasm worker answers; the
    // port is never in it, because a MessagePort cannot be cloned.
    expect(worker.messages).toEqual([{ drpc: 'serve' }, { drpc: 'serve' }])

    // Killing one is one teardown: the other keeps serving.
    second.close()
    await tick()
    expect(await first.invoke(echo.once, { text: 'still' })).toEqual({ text: 'echo:still' })
    first.close()
  })

  it('delivers a call opened before the worker binds the port', async () => {
    // The reason a port is transferred instead of the worker being handed to a
    // PortTransport: a MessagePort queues everything posted into it until its
    // owner binds it, so a call opened on this very tick is delivered late
    // rather than dropped. A worker's own global scope, wired through
    // onmessage, would have lost it.
    const worker = new MockWorker()
    worker.autoServe = false
    const conn = dialWorker(worker)
    const call = conn.invoke(echo.once, { text: 'early' })
    await tick()

    worker.serve()
    expect(await call).toEqual({ text: 'echo:early' })
    conn.close()
  })

  it('carries a message of your own, and the port options through', async () => {
    const worker = new MockWorker()
    const conn = dialWorker(worker, { message: { kind: 'rpc', id: 7 }, maxMessageSize: 128 })
    expect(worker.messages).toEqual([{ kind: 'rpc', id: 7 }])
    const err = (await conn.invoke(echo.once, { text: 'x'.repeat(500) }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.RESOURCE_EXHAUSTED) // §4.4, from the options given here
    expect(await conn.invoke(echo.once, { text: 'ok' })).toEqual({ text: 'echo:ok' })
    conn.close()
  })

  it('takes the Conn half of the options bag too', async () => {
    // One bag, two consumers, no key in common: maxMessageSize is the
    // adapter's ceiling (§4.4), `reliable` is the Conn's override of transport
    // discovery (§4.3). Nothing is called here — where each key landed is the
    // whole assertion, and a mode the far side does not share would not
    // survive a round trip anyway (§10.6).
    const worker = new MockWorker()
    const conn = dialWorker(worker, { maxMessageSize: 128, reliable: false })
    expect(conn.reliable).toBe(false)
    conn.close()
    await tick()
  })

  it('never terminates the worker, not even when the connection dies', async () => {
    // terminate() aborts a worker at once, discarding whatever is still queued
    // for it. Killing it is the host's decision, taken after its endpoints have
    // torn down — never a side effect of one peer's §4.5 teardown.
    const worker = new MockWorker()
    const conn = dialWorker(worker)
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })
    // One close is the whole teardown: Conn.close closes the transport, which
    // says goodbye and releases the port. The worker is not its to end.
    conn.close()
    await tick()
    expect(worker.terminated).toBe(0)
  })

  it('leaves nothing half-attached when the worker refuses the port', async () => {
    // A terminated worker throws on postMessage. Both ends of the channel are
    // released — a live MessagePort keeps node's event loop alive — and the
    // transport that was already listening on one of them is closed.
    const worker = {
      postMessage(): void {
        throw new Error('worker terminated')
      },
    }
    expect(() => dialWorker(worker)).toThrow(/worker terminated/)
  })
})

describe('gateway (§6.4)', () => {
  it('routes responses to the right peer and isolates disconnects', async () => {
    const gateway = new PortGateway()
    const server = new Server(gateway)
    registerEcho(server)

    const [a1, b1] = channel()
    const [a2, b2] = channel()
    gateway.bind(b1)
    gateway.bind(b2)
    const serving1 = gateway.servePeer(server, b1)
    void gateway.servePeer(server, b2)
    const conn1 = new Conn(new PortTransport(a1))
    const conn2 = new Conn(new PortTransport(a2))

    const [r1, r2] = await Promise.all([conn1.invoke(echo.once, { text: 'one' }), conn2.invoke(echo.once, { text: 'two' })])
    expect(r1).toEqual({ text: 'echo:one' })
    expect(r2).toEqual({ text: 'echo:two' })

    // Killing peer 1 leaves peer 2 untouched.
    conn1.close()
    await serving1
    expect(await conn2.invoke(echo.once, { text: 'still' })).toEqual({ text: 'echo:still' })
    conn2.close()
  })

  it('serves a port at most once and refuses a peer that is gone', async () => {
    const gateway = new PortGateway()
    const server = new Server(gateway)
    const ping = frame({ epoch: 1, flags: FlagPing })
    const [a, b] = mockPair()
    const serving = gateway.servePeer(server, b)
    await expect(gateway.servePeer(server, b)).rejects.toThrow(/already served/)
    a.postMessage(new Uint8Array(0)) // the peer's goodbye
    await tick()
    await serving
    await expect(gateway.handle(ping, { peer: 1 })).rejects.toThrow(/disconnected/)
    await expect(gateway.handle(ping)).rejects.toThrow(/no gateway peer/)
  })

  it('a signal aborts servePeer and releases the port', async () => {
    const gateway = new PortGateway()
    const server = new Server(gateway)
    registerEcho(server)
    const [a, b] = mockPair()
    const ctl = new AbortController()
    const serving = gateway.servePeer(server, b, { signal: ctl.signal })
    const conn = new Conn(new PortTransport(a))
    expect(await conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })

    ctl.abort()
    expect(await serving).toBeUndefined()
    expect(b.closes).toBe(1)
    await tick()
    // The client heard the goodbye and tore its own endpoint down.
    const err = (await conn.invoke(echo.once, { text: 'gone' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
  })

  it('gateway.close() tears every served port down', async () => {
    const net = realEnds()
    expect(await net.conn.invoke(echo.once, { text: 'hi' })).toEqual({ text: 'echo:hi' })

    net.gateway.close()
    await net.serving
    await settle()
    const err = (await net.conn.invoke(echo.once, { text: 'gone' }).catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
  })
})
