// Cross-language conformance over a JS message port: a REAL Go drpc.Server —
// conformance/wasmserver, compiled to GOOS=js GOARCH=wasm — driven by the
// TypeScript client across a MessageChannel, both ends inside this one node
// process. Nothing is mocked and nothing is stubbed: the Go core dispatches
// the methods, internal/echo's handlers produce the answers, and every byte
// between them is a marshaled Envelop (§4.1) posted through the port.
//
// The wiring is the shipped pair and not a fixture of its own —
// jsport.Gateway.Serve publishing the entry point, open() awaiting it and
// dialling the channel — so the handshake the two halves agree on is under
// test here too, in the only place where both of them are real. The page's
// own two lines, with `{ worker: false }` because node has no DOM Worker:
// everything else about that path is what a browser runs.
//
// This is the counterpart to test/conformance.test.ts, which drives the same
// service over loopback UDP. Two things are only provable here. First, the
// channel is GENUINELY reliable — a port neither loses, duplicates nor
// reorders — so reliable mode is discovered from the adapter on both sides
// (§4.3) instead of being forced frame by frame the way the UDP fixture has
// to, and per-stream flow control (§4.2.1), which exists in reliable mode
// only, is exercised across implementations for the first time on a channel
// that earns it. Second, teardown: with every protocol timer off (§10.6) the
// adapter's §4.5 duty is the ONLY thing that can ever unblock a live call, and
// both of its halves are Go/TS handshakes — the empty-envelop goodbye, and the
// host reporting a death the port cannot see.
//
// Skipped when `go` is unavailable, as the UDP fixture is.

import { create } from '@bufbuild/protobuf'
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { Conn } from '../src/conn'
import type { FrameHandler } from '../src/seam'
import { Code, type StatusError } from '../src/status'
import { fromService } from '../src/transport/protobuf-es'
import { open, type WasmSock } from '../src/wasm'
import { FlagCompressed, FlagOpen, FlagWindow, shapeOf, type Frame } from '../src/wire'
import { EchoRequestSchema, EchoService } from '../src/testing/gen/echo/echo_pb.js'

const repoRoot = resolve(process.cwd(), '..')
const Echo = fromService(EchoService)

// gzipBody is ~2 kB of highly compressible text. A port has no size ceiling,
// so unlike the UDP suite nothing here forces compression — what proves it
// happened is the recorded wire, below.
const gzipBody = 'drpc-wasm-gzip-'.repeat(140)

const canGzip = typeof CompressionStream !== 'undefined' && typeof DecompressionStream !== 'undefined'

// Same probe as test/conformance.test.ts: the fixture is built on demand, so
// without a toolchain there is nothing to run against.
function hasGo(): boolean {
  try {
    execFileSync('go', ['version'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
}

// ---------------------------------------------------------------------------
// the wasm host
// ---------------------------------------------------------------------------

// The two teardown globals conformance/wasmserver installs. The third,
// drpcServe, is never named here: open() waits for it, hands it one end of a
// channel it made itself on every dial(), and takes the property back off
// globalThis before it returns.
declare global {
  var drpcStop: () => void
  var drpcExit: () => void
}

// loadGoRuntime evaluates wasm_exec.js, which defines globalThis.Go. It is
// read from GOROOT rather than vendored because it is version-coupled to the
// compiler that produced the module, and it is a classic script — hence
// `new Function` rather than an import, which is how a non-module script is
// evaluated from ESM.
function loadGoRuntime(): void {
  const goroot = execFileSync('go', ['env', 'GOROOT']).toString().trim()
  new Function(readFileSync(join(goroot, 'lib', 'wasm', 'wasm_exec.js'), 'utf8'))()
}

// Instance is one running wasm server: the sock, one connection to it, and the
// two globals it installed. Those are captured at readiness on purpose — a
// second instance overwrites them, and each of the two lifecycles below has to
// keep driving its own server.
interface Instance {
  sock: WasmSock
  conn: Conn
  stop: () => void
  exit: () => void
}

// startInstance is the page's own two lines (examples/browser-wasm/web), run
// against the fixture: open() instantiates the module and waits for
// Gateway.Serve to publish drpcServe — publishing it IS the readiness signal,
// and it is raced against the instance dying on the way up, which is why a
// fixture that panics in main reports that instead of timing this suite out —
// and dial() makes the MessageChannel. go.run()'s promise is wired to closing
// every connection with the cause; that wiring is the host's half of §4.5 and
// it is load-bearing for every case here, not just the exit case: a handler
// that panics kills the instance exactly as os.Exit does, and with every
// protocol timer off (§10.6) nothing else would ever fail the call that was in
// flight.
async function startInstance(mod: WebAssembly.Module): Promise<Instance> {
  // The source is the compiled Module the suite already holds — nothing to
  // fetch here, the fixture came off disk. `{ worker: false }` runs it in this
  // realm: node has no DOM Worker, and what is under test is the Go/TS
  // handshake, which is the same one either way.
  const sock = await open(mod, { worker: false })
  return { sock, conn: sock.dial(), stop: globalThis.drpcStop, exit: globalThis.drpcExit }
}

// ---------------------------------------------------------------------------
// wire recorder
// ---------------------------------------------------------------------------

// The same tap test/conformance.test.ts uses, and for the same reason: flags
// and windows are invisible from the API surface, and they are exactly what
// the two implementations have to agree on — "the call succeeded" cannot tell
// a missing window advertisement or a skipped compression from a correct one.
interface Wire {
  tx: Frame[]
  rx: Frame[]
}

function record(conn: Conn): Wire {
  const w: Wire = { tx: [], rx: [] }
  // The send side is the transport the Conn was dialled over. A page never
  // needs it — open() makes it, attaches it and closes it — so this suite
  // reaches for it, which is the same tap test/conformance.test.ts puts on the
  // transport it built by hand.
  const tx = (conn as unknown as { tx: FrameHandler }).tx
  const send = tx.handle.bind(tx)
  tx.handle = (f, ctx) => {
    w.tx.push({ ...f }) // snapshot: flags may still be mutated by the sender
    return send(f, ctx)
  }
  const recv = conn.handle.bind(conn)
  conn.handle = (f, ctx) => {
    w.rx.push({ ...f })
    return recv(f, ctx)
  }
  return w
}

const openOf = (fs: readonly Frame[]): Frame => {
  const f = fs.find((x) => (x.flags & FlagOpen) !== 0)
  if (f === undefined) throw new Error('no OPEN frame was recorded')
  return f
}

// The server's creation ack: an H frame — no shape bits, no payload (§7).
// §8 makes one mandatory for every STREAMING call in reliable mode, because
// it is the frame the server's flow-control window rides on (§4.2.1); unary
// is exempt, so there is no ack to look for on one.
const ackOf = (fs: readonly Frame[]): Frame => {
  const f = fs.find((x) => shapeOf(x) === 0 && x.payload === undefined)
  if (f === undefined) throw new Error('no creation-ack H frame was recorded')
  return f
}

const grantsOf = (fs: readonly Frame[]): Frame[] => fs.filter((f) => shapeOf(f) === FlagWindow)

describe.skipIf(!hasGo())('cross-language conformance (TS client ↔ Go wasm server over a MessagePort)', () => {
  let mod: WebAssembly.Module
  let server: Instance
  let conn: Conn
  let wire: Wire

  beforeAll(async () => {
    const bin = join(mkdtempSync(join(tmpdir(), 'drpc-wasm-')), 'wasmserver.wasm')
    execFileSync('go', ['build', '-o', bin, './conformance/wasmserver'], {
      cwd: repoRoot,
      env: { ...process.env, GOOS: 'js', GOARCH: 'wasm' },
      stdio: 'pipe',
    })
    loadGoRuntime()
    // Compiled once, instantiated per lifecycle: the teardown case below runs
    // a second server from these same bytes.
    mod = await WebAssembly.compile(readFileSync(bin))

    server = await startInstance(mod)
    // Zero options on either side: mode comes from the adapter (§4.3).
    conn = server.conn
    wire = record(conn)
  }, 60_000)

  afterAll(() => {
    // The first instance outlives every case (the exit case uses its own), so
    // end it here, in this order: the goodbye first, while there is still a
    // runtime to receive it, then the instance. Closing the Conn closes this
    // end of the channel, and in node that closes the Go end with it — which
    // is what lets the process exit at all, since a MessagePort with a
    // listener on it keeps the event loop alive and a vitest run that never
    // ends is the one thing this hook exists to prevent.
    server?.sock.close()
    server?.exit()
  })

  it('unary Once: the Go handler applies CircularShift', async () => {
    // Go CircularShift("hello", 2) = "hello"[2:] + "hello"[:2] = "llohe".
    const res = await conn.invoke(Echo.once, create(EchoRequestSchema, { message: 'hello', circularShift: 2 }))
    expect(res.message).toBe('llohe')
    expect(res.sequence).toBe(0)
    // Go sets DateCreated = timestamppb.Now(); protobuf-es decodes it here.
    expect(res.dateCreated?.seconds).toBeGreaterThan(0n)
  })

  it('server-streaming Many: repeated shifts with ascending sequence', async () => {
    // v="abc"; each of 3 iterations shifts left by 1: bca, cab, abc.
    const stream = conn.newStream(Echo.many, {})
    await stream.send(create(EchoRequestSchema, { message: 'abc', repeat: 3, circularShift: 1 }))
    const got: Array<{ m: string; s: number }> = []
    for await (const res of stream) got.push({ m: res.message, s: res.sequence })
    expect(got).toEqual([
      { m: 'bca', s: 0 },
      { m: 'cab', s: 1 },
      { m: 'abc', s: 2 },
    ])
  })

  it('client-streaming Buff: batch accumulates across sends with shared sequence', async () => {
    const stream = conn.newStream(Echo.buff, {})
    await stream.send(create(EchoRequestSchema, { message: 'ab', repeat: 1, circularShift: 1 }))
    await stream.send(create(EchoRequestSchema, { message: 'xy', repeat: 1, circularShift: 1 }))
    stream.closeSend()
    const res = await stream.recv()
    expect(res?.items.map((i) => ({ m: i.message, s: i.sequence }))).toEqual([
      { m: 'ba', s: 0 },
      { m: 'yx', s: 1 },
    ])
    expect(await stream.recv()).toBeUndefined()
  })

  it('bidi Live: interleaved echo with ascending sequence, EOF after half-close', async () => {
    const stream = conn.newStream(Echo.live, {})
    await stream.send(create(EchoRequestSchema, { message: 'hi', repeat: 1, circularShift: 1 }))
    expect(await stream.recv()).toMatchObject({ message: 'ih', sequence: 0 })
    await stream.send(create(EchoRequestSchema, { message: 'yo', repeat: 1, circularShift: 1 }))
    expect(await stream.recv()).toMatchObject({ message: 'oy', sequence: 1 })
    stream.closeSend()
    expect(await stream.recv()).toBeUndefined()
  })

  it('a non-OK status from the Go handler arrives as a StatusError (§7)', async () => {
    // req.status is returned verbatim by the handler (EchoRequest.Error), so
    // this is the CLOSE code+desc channel, which every OK call leaves untested.
    const err = (await conn
      .invoke(Echo.once, create(EchoRequestSchema, { message: 'x', status: { code: Code.NOT_FOUND, message: 'not here' } }))
      .catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.NOT_FOUND)
    expect(err.desc).toBe('not here')
  })

  it.skipIf(!canGzip)('gzip crosses the port in both directions (§12.1)', async () => {
    // No datagram limit forces compression here, so the proof is the recorded
    // wire: what left this endpoint and what came back are both a fraction of
    // the 2 kB message, and both are flagged COMPRESSED.
    const txAt = wire.tx.length
    const rxAt = wire.rx.length
    const res = await conn.invoke(Echo.once, create(EchoRequestSchema, { message: gzipBody, circularShift: 0 }), { compressor: 'gzip' })
    expect(res.message).toBe(gzipBody)

    const open = openOf(wire.tx.slice(txAt))
    expect(open.compressor).toBe('gzip') // named once, for the whole call
    expect(open.flags & FlagCompressed).toBe(FlagCompressed)
    expect(open.payload!.length).toBeLessThan(400)
    const term = wire.rx.slice(rxAt).find((f) => f.payload !== undefined)!
    expect(term.flags & FlagCompressed).toBe(FlagCompressed)
    expect(term.compressor).toBe('') // no server frame repeats the name
    expect(term.payload!.length).toBeLessThan(400)
  })

  // -------------------------------------------------------------------------
  // reliable mode (§4.2.1, §4.3, §10.6)
  // -------------------------------------------------------------------------

  it('reliable mode is discovered from the port, and both sides advertise a window', async () => {
    // Neither side was given a mode option: jsport.Gateway and PortTransport
    // both report Reliable(), and each core reads it off its own adapter.
    expect(conn.reliable).toBe(true)
    const txAt = wire.tx.length
    const rxAt = wire.rx.length
    // Its own call, not the record left by the cases above, so this fails for
    // its own reason and passes when run alone. Server-streaming, because §8
    // exempts unary from the creation ack and makes it mandatory for SS in
    // reliable mode for exactly the reason under test: it is the only frame
    // the server's window can ride on.
    const stream = conn.newStream(Echo.many, {})
    await stream.send(create(EchoRequestSchema, { message: 'ab', repeat: 1, circularShift: 1 }))
    const got: string[] = []
    for await (const res of stream) got.push(res.message)
    expect(got).toEqual(['ba'])

    const open = openOf(wire.tx.slice(txAt))
    // The client's rx buffer, floored at W_init = 32 (§4.2.1). Over UDP this
    // field is always 0 — flow control does not exist in unreliable mode.
    expect(open.window).toBe(32)
    // Reliable mode propagates no default deadline: T_call is an
    // unreliable-mode timer (§10.2, §10.6).
    expect(open.timeoutMs).toBeUndefined()
    // The Go server's own advertisement, on the creation ack for this same
    // call — which is not a header flush, so its header field stays absent
    // (§8).
    const ack = ackOf(wire.rx.slice(rxAt))
    expect(ack.sid).toBe(stream.sid)
    expect(ack.window).toBe(32)
    expect(ack.header).toBeUndefined()
  })

  it('client→server: Go grants credit for what its handler consumed (§4.2.1)', async () => {
    const rxAt = wire.rx.length
    // 40 messages past the 32-message window the ack advertised: a sender
    // that received no credit parks at 32 and the call dies at T_stall, so
    // completing at all means Go granted and TS consumed the grants.
    // repeat:0 keeps the batch response empty — this is about the credit.
    const stream = conn.newStream(Echo.buff, {})
    for (let i = 0; i < 40; i++) {
      await stream.send(create(EchoRequestSchema, { message: 'x', repeat: 0 }))
    }
    stream.closeSend()
    expect((await stream.recv())?.items).toEqual([])
    expect(await stream.recv()).toBeUndefined()

    // The window that paced those 40 sends is the SERVER's, on the creation
    // ack §8 makes mandatory for a client-streaming call; the client's own
    // OPEN window paces the other direction and says nothing about this one.
    expect(ackOf(wire.rx.slice(rxAt)).window).toBe(32)
    const grants = grantsOf(wire.rx.slice(rxAt))
    expect(grants.length).toBeGreaterThanOrEqual(1)
    for (const g of grants) {
      // A WINDOW frame is stateless: this sid, no seq, no payload, credit > 0.
      expect(g.sid).toBe(stream.sid)
      expect(g.seq).toBe(0)
      expect(g.payload).toBeUndefined()
      expect(g.window).toBeGreaterThan(0)
    }
    // Grants are batched at half the window, as HTTP/2 stacks do.
    expect(grants.reduce((n, g) => n + g.window, 0)).toBeGreaterThanOrEqual(8)
  }, 30_000)

  it('server→client: the TS client grants credit back (§4.2.1)', async () => {
    const txAt = wire.tx.length
    // 40 responses past the client's 32-message window: the Go server has to
    // park and wait for TS grants to finish the stream.
    const stream = conn.newStream(Echo.many, {})
    await stream.send(create(EchoRequestSchema, { message: 'ab', repeat: 40, circularShift: 1 }))
    const got: string[] = []
    for await (const res of stream) got.push(res.message)
    expect(got).toHaveLength(40)
    expect(got.slice(0, 3)).toEqual(['ba', 'ab', 'ba'])

    const grants = grantsOf(wire.tx.slice(txAt))
    expect(grants.length).toBeGreaterThanOrEqual(2) // 40 consumed, batched at 16
    for (const g of grants) {
      expect(g.sid).toBe(stream.sid)
      expect(g.seq).toBe(0)
      expect(g.payload).toBeUndefined()
      expect(g.window).toBeGreaterThan(0)
    }
  }, 30_000)

  // -------------------------------------------------------------------------
  // teardown (§4.5) — the two halves, one per server lifecycle
  // -------------------------------------------------------------------------
  //
  // With protocol timers off there is no deadline that would ever fail a
  // hanging call, so these are not "nice to have" paths: they are the only
  // mechanism by which a call outlives its server at all.

  it('a second instance dying without a word: go.run() resolves, the host says why (§4.5)', async () => {
    // Its own instance and its own channel, because it ends with a dead
    // server: the one the cases above share is untouched and still serving.
    // Also a second open() on the same entry point, which only works because
    // the first gave the name back once it had caught its publish.
    const inst = await startInstance(mod)
    const conn2 = inst.conn

    const stream = conn2.newStream(Echo.live, {})
    await stream.send(create(EchoRequestSchema, { message: 'hi', repeat: 1, circularShift: 1 }))
    expect(await stream.recv()).toMatchObject({ message: 'ih' })

    // os.Exit runs nothing — no deferred code, no goodbye — and a MessagePort
    // whose peer stopped existing looks exactly like one whose peer is merely
    // quiet, so go.run()'s promise is the only evidence that ever arrives.
    // open() wired it to closing every connection with the cause; without
    // that, this recv hangs forever.
    inst.exit()
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    // The cause survives, which is what says this was the host's report and
    // not a goodbye: the peer's empty envelop trips the death latch with no
    // cause at all, and the first cause wins.
    expect(err.message).toMatch(/the wasm instance exited/)
  })

  // Last, because it ends the shared server: everything above needs it alive.
  it('drpcStop(): the Go gateway says goodbye and the TS pump tears down (§4.5)', async () => {
    const stream = conn.newStream(Echo.live, {})
    await stream.send(create(EchoRequestSchema, { message: 'hi', repeat: 1, circularShift: 1 }))
    expect(await stream.recv()).toMatchObject({ message: 'ih' })

    const at = wire.rx.length
    server.stop() // jsport.Gateway.Close: one 0-byte message per served port
    const err = (await stream.recv().catch((e) => e)) as StatusError
    expect(err.code).toBe(Code.UNAVAILABLE)
    // Nothing about that was a frame: the goodbye is the empty envelop
    // itself, so the recorder — which only ever sees decoded frames — saw the
    // call die with nothing delivered after it.
    expect(wire.rx.slice(at)).toEqual([])
    // The pump exited and ran conn.close on the way out, so the Conn is done.
    expect(() => conn.newStream(Echo.once, {})).toThrow(/connection is closed/)
  })
})
