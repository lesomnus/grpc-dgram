// Per-stream flow control (PROTOCOL.md §4.2.1, reliable mode only) — the TS
// twin of the Go flow_test.go coverage:
//
//   - the head-of-line fix it exists for: one call's consumer may stall
//     without touching any other call on the same channel (§4.2, §4.2.1);
//   - the park/resume boundary of a sender: exactly the credit it was given
//     reaches the wire, in order, no message lost or duplicated;
//   - the advertisement path on the wire — the client's OPEN, the server's
//     creation-ack H, and the WINDOW grant frame (§4.2.1, §7, §8);
//   - the T_stall bound of a park (§4.2.1, §10.1);
//   - grants that stop at the call's end, and a WINDOW for an unknown or
//     finished sid dropped in silence — never answered with a RESET (§9.3);
//   - unreliable mode ignoring window/WINDOW entirely (§4.2.1 scope);
//   - the reliable-mode rx-buffer floor of W_init that makes the sender's
//     assumption safe, and the overrun past it failing the call INTERNAL
//     instead of blocking (§4.2, §4.2.1).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Conn } from '../src/conn'
import { Server } from '../src/server'
import { Code, MessageTooLargeError, type StatusError } from '../src/status'
import { W_INIT } from '../src/util'
import { FlagClose, FlagOpen, FlagWindow, frame, isData, isHeaderFrame, isOpen, isReset, isTerminal, shapeOf, type Frame } from '../src/wire'
import { echo, makeNet, registerEcho, tick, wireClone, type TestRes } from '../src/testing'

const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v))
const dec = (f: Frame): TestRes => JSON.parse(new TextDecoder().decode(f.payload ?? new Uint8Array())) as TestRes

// isGrant recognizes a well-formed WINDOW frame: shape WINDOW alone, seq 0,
// no payload (PROTOCOL.md §7, §9.1).
const isGrant = (f: Frame): boolean => shapeOf(f) === FlagWindow && f.seq === 0 && f.payload === undefined

// ---------------------------------------------------------------------------
// a single-delivery-loop transport
// ---------------------------------------------------------------------------

// Loop is one direction of a reliable adapter: frames are queued and handed
// to the peer by ONE pump, in order, awaiting each delivery (§4.2). That
// single loop is what makes head-of-line blocking possible at all — and
// therefore what the flow-control fix has to be measured against. makeNet's
// direct hand-off could never show it: there every call has its own caller.
class Loop {
  private readonly q: Frame[] = []
  private pumping = false
  private deliver: (f: Frame) => Promise<void> = async () => {}
  // Every frame offered to this direction, and how many the pump has handed
  // over so far (the quiescence signal `settle` waits on).
  readonly sent: Frame[] = []
  delivered = 0

  to(deliver: (f: Frame) => Promise<void>): void {
    this.deliver = deliver
  }

  get pending(): number {
    return this.q.length
  }

  push(f: Frame): void {
    const g = wireClone(f)
    this.sent.push(g)
    this.q.push(g)
    if (!this.pumping) void this.pump()
  }

  private async pump(): Promise<void> {
    this.pumping = true
    try {
      for (;;) {
        const f = this.q.shift()
        if (f === undefined) return
        try {
          await this.deliver(f)
        } catch {
          // Frame-level errors never tear the channel down (§4.2).
        }
        this.delivered++
      }
    } finally {
      this.pumping = false
    }
  }
}

interface LoopNet {
  conn: Conn
  server: Server
  counts: ReturnType<typeof registerEcho>
  c2s: Loop
  s2c: Loop
  // settle runs microtask turns until both loops are drained and idle.
  settle: () => Promise<void>
}

function makeLoopNet(): LoopNet {
  const peer = 'peer-1'
  const c2s = new Loop()
  const s2c = new Loop()
  const server = new Server({ handle: (f: Frame) => s2c.push(f) }, { reliable: true })
  const counts = registerEcho(server)
  const conn = new Conn({ handle: (f: Frame) => c2s.push(f) }, { reliable: true })
  c2s.to((f) => server.handle(f, { peer }))
  s2c.to((f) => conn.handle(f, {}))

  const settle = async (): Promise<void> => {
    for (let i = 0; i < 500; i++) {
      const before = c2s.delivered + s2c.delivered
      await tick()
      if (c2s.pending === 0 && s2c.pending === 0 && c2s.delivered + s2c.delivered === before) return
    }
  }
  return { conn, server, counts, c2s, s2c, settle }
}

// ---------------------------------------------------------------------------
// §4.2 / §4.2.1: the head-of-line fix. A reliable adapter delivers every
// call's frames from ONE loop, so before v1.1 a consumer that stopped reading
// blocked that loop and with it every other call on the channel. Now the
// producer parks on credit instead, and the channel stays live.
// ---------------------------------------------------------------------------

describe('head-of-line blocking (§4.2.1)', () => {
  it('a stalled consumer parks its producer and never blocks another call', async () => {
    const net = makeLoopNet()
    const burst = 200 // far past the client's 32-message window

    const stalled = net.conn.newStream(echo.many, {})
    await stalled.send({ text: 'm', n: burst })

    // Nothing is read from `stalled`: its buffer fills and the producing
    // handler parks on flow control — a park in the SENDER, which is the whole
    // point. Exactly the advertised window reached the wire, not one frame
    // more.
    await net.settle()
    expect(net.s2c.sent.filter(isData)).toHaveLength(W_INIT)

    // The shared delivery loop is therefore free, and a second call on the
    // same channel completes. Under the pre-v1.1 core this never returned: the
    // pump was blocked handing frame 33 to the stalled call.
    expect(await net.conn.invoke(echo.once, { text: 'abc' })).toEqual({ text: 'echo:abc' })

    // And the parked call is only parked: as the application consumes, the
    // grants resume it and every message arrives, in order (§14).
    const got: string[] = []
    for await (const res of stalled) got.push(res.text)
    expect(got).toEqual(Array.from({ length: burst }, (_, i) => `m#${i}`))
    net.conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 park/resume boundary, pinned against a crafted peer so the window is
// exact: the advertisement is authoritative, a sender emits exactly the credit
// it holds and not one frame more, and each grant releases exactly what it
// credits — in order, once each.
// ---------------------------------------------------------------------------

const PEER_EPOCH = 0x5eed

// FlowPeer is a crafted server for one client-streaming call: it acks the
// OPEN with an H advertising `window`, records every client frame, and grants
// credit only when the test says so. `auto: false` withholds the ack, so a
// test can watch the sender run on the assumed W_init first.
class FlowPeer {
  conn!: Conn
  readonly frames: Frame[] = []
  private epoch = 0
  private sid = 0

  constructor(
    private readonly window: number,
    private readonly auto = true,
  ) {}

  async handle(f: Frame): Promise<void> {
    const g = wireClone(f)
    this.frames.push(g)
    if (!isOpen(g)) return
    this.epoch = g.epoch
    this.sid = g.sid
    if (this.auto) await this.ack()
  }

  // ack is the creation ack, carrying this side's advertisement (§4.2.1, §8).
  ack(): Promise<void> {
    return this.conn.handle(frame({ epoch: PEER_EPOCH, peerEpoch: this.epoch, sid: this.sid, seq: 1, window: this.window }), {})
  }

  // grant sends a WINDOW frame adding n messages of credit (§4.2.1, §7). A
  // client-bound frame must name the incarnation it addresses (§6.1).
  grant(n: number): Promise<void> {
    return this.conn.handle(frame({ epoch: PEER_EPOCH, peerEpoch: this.epoch, sid: this.sid, flags: FlagWindow, window: n }), {})
  }

  // messages returns the payloads of the data frames that reached the wire.
  messages(): string[] {
    return this.frames.filter(isData).map((f) => dec(f).text)
  }
}

describe('sender parking (§4.2.1)', () => {
  it('paces itself by W_init until the advertisement lands, which then counts what was already sent', async () => {
    const peer = new FlowPeer(2, false)
    const conn = new Conn(peer, { reliable: true })
    peer.conn = conn

    const stream = conn.newStream(echo.count, {})
    await tick()
    // Nothing has been advertised yet: the sender runs on the assumed W_init,
    // which is why a client-streaming burst cannot empty itself onto the wire
    // before the ack it would be paced by.
    for (let i = 0; i < 3; i++) await stream.send({ text: `m${i}` })
    expect(peer.messages()).toEqual(['m0', 'm1', 'm2'])

    // The ack advertises 2 — fewer than the three already sent. It is
    // authoritative and counted against them, so the sender is over its window
    // and the next message parks instead of going out.
    await peer.ack()
    let released = false
    void stream
      .send({ text: 'm3' })
      .then(() => (released = true))
      .catch(() => undefined)
    await tick()
    expect(released).toBe(false)
    expect(peer.messages()).toEqual(['m0', 'm1', 'm2'])

    // Two messages of credit put the allowance at 4 against 3 sent: exactly
    // one message is released.
    await peer.grant(2)
    await tick()
    expect(released).toBe(true)
    expect(peer.messages()).toEqual(['m0', 'm1', 'm2', 'm3'])
    conn.close()
  })

  it('emits exactly the credit it was given and resumes on each grant, in order', async () => {
    const peer = new FlowPeer(4)
    const conn = new Conn(peer, { reliable: true })
    peer.conn = conn

    const stream = conn.newStream(echo.count, {}) // the eager OPEN is never credited
    await tick()

    const burst = 8
    const sends = (async () => {
      for (let i = 0; i < burst; i++) await stream.send({ text: `m${i}` })
    })()

    // The ack advertised 4 — authoritative, replacing the assumed W_init — so
    // exactly four messages reach the wire and the fifth parks.
    await tick()
    expect(peer.messages()).toEqual(['m0', 'm1', 'm2', 'm3'])

    // A grant releases exactly what it credits, and the order is the
    // application's, unchanged (§14).
    await peer.grant(2)
    await tick()
    expect(peer.messages()).toEqual(['m0', 'm1', 'm2', 'm3', 'm4', 'm5'])

    await peer.grant(16)
    await sends
    expect(peer.messages()).toEqual(Array.from({ length: burst }, (_, i) => `m${i}`))
    conn.close()
  })

  it('credit taken for a frame the adapter refused is refunded (§4.4)', async () => {
    // A handler that ignores what send() returns — legal, and the case the
    // refund exists for: without it these 40 refusals would drain the whole
    // window and every later message would park until T_stall.
    const refused = 40
    const big = 'x'.repeat(128)
    let conn!: Conn
    const server = new Server(
      {
        handle: (f: Frame) => {
          if ((f.payload?.length ?? 0) > 64) throw new MessageTooLargeError(`refused ${f.payload?.length ?? 0} bytes`)
          return conn.handle(wireClone(f), {})
        },
      },
      { reliable: true },
    )
    server.register(echo.many, async (_req, stream) => {
      for (let i = 0; i < refused; i++) {
        try {
          await stream.send({ text: big })
        } catch {
          // Ignored, as gRPC allows.
        }
      }
      for (let i = 0; i < 3; i++) await stream.send({ text: `ok${i}` })
    })
    conn = new Conn({ handle: (f: Frame) => server.handle(wireClone(f), { peer: 'p' }) }, { reliable: true })

    const stream = conn.newStream(echo.many, {})
    await stream.send({ text: 'go', n: 0 })
    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toEqual(['ok0', 'ok1', 'ok2'])
    conn.close()
    await server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 advertisement path, asserted on the wire: the client advertises its
// per-call buffer on the OPEN, the server on its creation-ack H, and consumed
// messages come back as WINDOW grants — shape 16, seq 0, no payload (§7).
// ---------------------------------------------------------------------------

describe('advertisements and grants on the wire (§4.2.1, §7, §8)', () => {
  it('the OPEN carries the client window, the ack the server’s, and consumption grants credit', async () => {
    const clientWindow = 48
    const serverWindow = 64 // both above the W_init floor
    const net = makeNet({
      reliable: true,
      connOpts: { rxBuffer: { size: clientWindow } },
      serverOpts: { rxBuffer: { size: serverWindow } },
    })
    // A bidi handler that answers each request with n responses and returns on
    // the half-close, so the grant is on the wire BEFORE the terminal.
    net.server.register(echo.live, async (stream) => {
      for await (const req of stream) {
        for (let i = 0; i < (req.n ?? 0); i++) await stream.send({ text: `${req.text}#${i}` })
      }
    })

    // The grant is batched at window/2, so consuming exactly that many elicits
    // exactly one.
    const responses = clientWindow / 2
    const stream = net.conn.newStream(echo.live, {})
    await stream.send({ text: 'm', n: responses })
    for (let i = 0; i < responses; i++) expect(await stream.recv()).toEqual({ text: `m#${i}` })
    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()

    const open = net.sentC2S.find(isOpen)
    expect(open).toBeDefined()
    expect(open!.window).toBe(clientWindow)

    const ack = net.sentS2C.find(isHeaderFrame)
    expect(ack).toBeDefined() // a streaming call must be acked (§8)
    expect(ack!.window).toBe(serverWindow)

    const grants = net.sentC2S.filter(isGrant)
    expect(grants).toHaveLength(1)
    expect(grants[0]!.sid).toBe(open!.sid)
    expect(grants[0]!.window).toBe(responses)
  })

  it('a receiver never grants after its call ended (§4.2.1)', async () => {
    // The synchronous pipe hands the whole response burst — and the terminal
    // behind it — to the client before the application reads anything, so
    // every consumed message here belongs to a call that is already over. The
    // peer has forgotten the sid; a grant for it would only draw a RESET.
    const net = makeNet({ reliable: true })
    const responses = 20 // > W_init/2: a live call would grant twice over
    const stream = net.conn.newStream(echo.many, {})
    await stream.send({ text: 'm', n: responses })
    for (let i = 0; i < 100 && !net.sentS2C.some(isTerminal); i++) await tick()
    expect(net.sentS2C.some(isTerminal)).toBe(true) // the call is over before a single recv

    const got: string[] = []
    for await (const res of stream) got.push(res.text)
    expect(got).toHaveLength(responses)
    expect(net.sentC2S.filter(isGrant)).toEqual([])
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §10.1: a park is bounded by T_stall. Reliable mode runs no protocol
// timers and the park sits before the adapter's write path, so this bound is
// the only thing that can break a sender whose peer never grants — the call
// fails UNAVAILABLE, it does not hang.
// ---------------------------------------------------------------------------

describe('T_stall (§4.2.1, §10.1)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('a starved sender fails UNAVAILABLE exactly at T_stall', async () => {
    const stallMs = 2000
    // A peer that advertises one message of credit and then goes quiet: no
    // grants, no terminal, nothing.
    const peer = new FlowPeer(1)
    const conn = new Conn(peer, { reliable: true, timing: { stallMs } })
    peer.conn = conn

    const stream = conn.newStream(echo.count, {})
    await tick()
    await stream.send({ text: 'm' }) // spends the single advertised credit

    let settled = false
    const parked = stream
      .send({ text: 'm' })
      .then(() => undefined)
      .catch((e) => e as StatusError)
      .finally(() => {
        settled = true
      })

    await vi.advanceTimersByTimeAsync(stallMs - 1)
    expect(settled).toBe(false) // the park must not end early
    await vi.advanceTimersByTimeAsync(2)

    const err = await parked
    expect(err).toBeDefined()
    expect(err!.code).toBe(Code.UNAVAILABLE)
    expect(err!.desc).toContain('flow-control stall')
    expect(peer.messages()).toEqual(['m']) // the parked message never reached the wire
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 / §9.3: a grant legitimately races the end of its call, so a WINDOW
// for an unknown, finished or tombstoned sid is dropped SILENTLY. Answering it
// with a RESET would turn every well-behaved stream's last grant into a RESET
// exchange — and hand an off-path attacker a free amplifier (§15).
// ---------------------------------------------------------------------------

describe('a WINDOW for an unknown or finished sid is silent (§4.2.1, §9.3)', () => {
  it('at the server', async () => {
    const sent: Frame[] = []
    const server = new Server({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: true })
    const counts = registerEcho(server)
    const epoch = 0xc0ffee

    await server.handle(frame({ epoch, sid: 42, flags: FlagWindow, window: 4 }), { peer: 'p' })
    expect(sent).toEqual([]) // no call, no tombstone, no answer

    // Control: the same unknown sid DOES draw a RESET for a data frame, so the
    // silence above is the rule at work, not a deaf harness (§9.3).
    await server.handle(frame({ epoch, sid: 42, seq: 2, payload: enc({ text: 'x' }) }), { peer: 'p' })
    expect(sent.filter(isReset)).toHaveLength(1)

    // A finished call's sid is just as silent: the grant the client sent for
    // its last consumed message arrives after the terminal.
    await server.handle(frame({ epoch, sid: 7, seq: 1, flags: FlagOpen | FlagClose, method: echo.once.path, payload: enc({ text: 'hi' }) }), { peer: 'p' })
    await tick()
    expect(counts.once).toBe(1)
    const before = sent.length
    await server.handle(frame({ epoch, sid: 7, flags: FlagWindow, window: 4 }), { peer: 'p' })
    expect(sent).toHaveLength(before)
    await server.stop()
  })

  it('at the client', async () => {
    const sent: Frame[] = []
    const conn = new Conn({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: true })
    const stream = conn.newStream(echo.live, {})
    await tick()
    const open = sent.find(isOpen)!

    // An unknown sid: no call, no tombstone, nothing.
    await conn.handle(frame({ epoch: PEER_EPOCH, peerEpoch: open.epoch, sid: open.sid + 1, flags: FlagWindow, window: 4 }), {})
    expect(sent.filter(isReset)).toEqual([])

    // And the call's own sid once it has finished: the abort ends it, and a
    // grant that was already in flight must not restart the exchange.
    const before = sent.length
    stream.cancel()
    await tick()
    expect(sent.filter(isTerminal)).toHaveLength(1) // the cancelled call aborts (§10.3)
    const after = sent.length
    expect(after).toBe(before + 1)
    await conn.handle(frame({ epoch: PEER_EPOCH, peerEpoch: open.epoch, sid: open.sid, flags: FlagWindow, window: 4 }), {})
    expect(sent).toHaveLength(after)
    conn.close()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 scope: unreliable mode ignores window and WINDOW entirely — a peer
// that cannot be trusted to retransmit cannot be paced, and there a full
// buffer drops by policy (§4.2). So the OPEN carries no advertisement, this
// side never grants, and an injected grant must change nothing: it cannot
// switch flow control on (§15), and it draws no RESET.
// ---------------------------------------------------------------------------

describe('unreliable mode ignores flow control (§4.2.1)', () => {
  it('advertises nothing, grants nothing, and a forged WINDOW neither paces nor answers', async () => {
    const net = makeNet({ reliable: false, serverOpts: { rxBuffer: { size: 128 } } })
    const stream = net.conn.newStream(echo.count, {})
    await tick()
    const open = net.sentC2S.find(isOpen)!
    expect(open.window).toBe(0) // an unreliable OPEN carries no advertisement

    // A forged grant of one message, addressed at the live call, delivered
    // before anything is sent: a sender that had (wrongly) adopted it would
    // park after one message and fail at T_stall.
    await net.conn.handle(frame({ epoch: PEER_EPOCH, peerEpoch: open.epoch, sid: open.sid, flags: FlagWindow, window: 1 }), {})

    for (let i = 0; i < 40; i++) await stream.send({ text: `m${i}` }) // well past W_init
    stream.closeSend()
    expect(await stream.recv()).toEqual({ text: '40' })

    expect(net.sentC2S.filter(isGrant)).toEqual([]) // this side never grants credit it does not track
    expect(net.sentC2S.filter(isReset)).toEqual([]) // the forged grant was dropped in silence (§9.3)
    net.conn.close()
    await net.server.stop()
  })
})

// ---------------------------------------------------------------------------
// §4.2.1 initial window: a sender paces itself by W_init until the peer's
// advertisement lands, so a reliable-mode receiver MUST buffer at least
// W_init — a smaller configured buffer is raised to it. And once flow control
// is on the receiver NEVER blocks: a full buffer means the peer overran the
// window it was granted, which fails THAT call with INTERNAL (§4.2).
// ---------------------------------------------------------------------------

describe('the reliable rx buffer floor and its overrun (§4.2, §4.2.1)', () => {
  it('raises a smaller buffer to W_init and fails the call INTERNAL one message past it', async () => {
    const sent: Frame[] = []
    const server = new Server({ handle: (f: Frame) => void sent.push(wireClone(f)) }, { reliable: true, rxBuffer: { size: 2 } })
    // The handler never consumes, so nothing is ever granted and the buffer
    // can only fill.
    server.register(echo.count, async (_stream, ctx) => {
      await new Promise<void>((res) => ctx.signal.addEventListener('abort', () => res()))
      return { text: 'never' }
    })

    const epoch = 1
    const sid = 3
    await server.handle(frame({ epoch, sid, seq: 1, flags: FlagOpen, method: echo.count.path, window: W_INIT }), { peer: 'p' })
    await tick()
    const ack = sent.find(isHeaderFrame)
    expect(ack).toBeDefined() // a streaming call must be acked (§8)
    expect(ack!.window).toBe(W_INIT) // the configured 2 is raised to the floor

    // A client running on the assumption may send W_init messages before that
    // ack can reach it: every one of them must fit.
    for (let i = 0; i < W_INIT; i++) {
      await server.handle(frame({ epoch, sid, seq: 2 + i, payload: enc({ text: `m${i}` }) }), { peer: 'p' })
    }
    await tick()
    expect(sent.filter(isTerminal)).toEqual([])

    // One past the window is a contract violation. The receiver does not block
    // — blocking is what flow control removes — it fails that call loudly.
    await server.handle(frame({ epoch, sid, seq: 2 + W_INIT, payload: enc({ text: 'overrun' }) }), { peer: 'p' })
    await tick()
    const term = sent.find(isTerminal)
    expect(term).toBeDefined()
    expect(shapeOf(term!)).toBe(FlagClose)
    expect(term!.code).toBe(Code.INTERNAL)
    await server.stop()
  })
})
