// Concurrency primitives translating the Go core's channels and goroutines
// into single-threaded async TypeScript. Synchronous code paths need no
// locks — an await point is the only place interleaving can happen.
//
// It also holds the two pieces of the 2026-07-25 round both endpoints share, so the
// client and the server cannot drift apart on them: flow control — the
// per-stream window and, beside it, the per-peer connection window
// (PROTOCOL.md §4.2.1, Go's flow.go) — and message compression with the
// per-call size caps (§12.1, §16 — Go's frame.go/callinfo.go).

import { DropPolicy } from './limits'
import { Code, StatusError } from './status'
import type { Frame } from './wire'

export const noop = (): void => {}

export function nowMs(): number {
  return Date.now()
}

// nonzeroEpoch draws an incarnation nonce (PROTOCOL.md §6.1). Zero is
// excluded: it marks an absent peer_epoch echo.
export function nonzeroEpoch(): number {
  const buf = new Uint32Array(1)
  for (;;) {
    globalThis.crypto.getRandomValues(buf)
    if (buf[0] !== 0) return buf[0]!
  }
}

// unref detaches a Node timer from the event-loop lifetime when supported
// (a live Conn/Server must not pin a Node process the way it would not pin a
// browser tab); a no-op in browsers.
export function unrefTimer(t: unknown): void {
  ;(t as { unref?: () => void }).unref?.()
}

// Latch is a one-shot broadcast: Go's `close(done)`.
export class Latch {
  tripped = false
  private readonly promise: Promise<void>
  private resolve!: () => void

  constructor() {
    this.promise = new Promise((res) => {
      this.resolve = res
    })
  }

  trip(): void {
    if (this.tripped) return
    this.tripped = true
    this.resolve()
  }

  wait(): Promise<void> {
    return this.promise
  }
}

// abortListener registers fn for a signal's abort and returns the disposer;
// long-lived signals see many short waits, so cleanup matters.
export function abortListener(signal: AbortSignal, fn: () => void): () => void {
  signal.addEventListener('abort', fn, { once: true })
  return () => signal.removeEventListener('abort', fn)
}

// FrameQueue is a bounded FIFO standing in for Go's buffered rx channel,
// with the §4.2 delivery modes: non-blocking drop-policy puts for unreliable
// mode and blocking puts for reliable mode.
export class FrameQueue {
  dropped = 0
  private buf: Frame[] = []
  private readWaiters: (() => void)[] = []
  private spaceWaiters: (() => void)[] = []
  // Tail of the FIFO chain that serializes putBlocking callers (see there).
  private putTail: Promise<void> = Promise.resolve()

  constructor(readonly cap: number) {}

  get size(): number {
    return this.buf.length
  }

  tryTake(): Frame | undefined {
    const f = this.buf.shift()
    if (f !== undefined) wake(this.spaceWaiters)
    return f
  }

  tryPut(f: Frame): boolean {
    if (this.buf.length >= this.cap) return false
    this.buf.push(f)
    wake(this.readWaiters)
    return true
  }

  // putDrop delivers f under the configured drop policy (unreliable mode,
  // PROTOCOL.md §4.2): Newest discards the arrival on a full buffer; Oldest
  // evicts the oldest to admit it. Returns how many frames this put dropped
  // — 0 or 1 — which is what the caller reports as a 'dropped' event (§14);
  // `dropped` keeps the running total.
  putDrop(f: Frame, policy: DropPolicy): number {
    if (this.tryPut(f)) return 0
    if (policy === DropPolicy.Oldest) {
      if (this.buf.shift() !== undefined) this.dropped++
      if (this.tryPut(f)) return 1
    }
    this.dropped++
    return 1
  }

  // putBlocking delivers f, blocking until there is room: dropping would
  // violate the reliable-mode exact-sequence contract (PROTOCOL.md §14), so a
  // slow consumer stalls delivery instead and the stall propagates into the
  // adapter's own flow control (§4.2). Bounded by the stream ending (a frame
  // for a finished call is moot) and by the rx signal (adapter teardown); it
  // returns false only for the signal bound — the frame is lost while the
  // call is still live, which on a reliable channel must fail loud.
  //
  // Putters are serialized in call order (the `putTail` chain), so a later
  // putter can never steal a freed slot from an earlier parked one: the
  // buffered channel this stands in for is a true FIFO. A conforming reliable
  // adapter delivers one frame per stream at a time (§4.2), so the chain is
  // uncontended — a single already-resolved await, effectively free — but the
  // guarantee holds even if an adapter delivers concurrently.
  async putBlocking(f: Frame, done: Latch, signal?: AbortSignal): Promise<boolean> {
    const prev = this.putTail
    let release = noop
    this.putTail = new Promise<void>((r) => {
      release = r
    })
    try {
      await prev // wait my turn: everything queued before me finishes first
      for (;;) {
        // A ready buffer always wins: a dead rx signal must not race delivery
        // (an adapter flushing its queue after transport death still delivers
        // every frame that fits).
        if (this.tryPut(f)) return true
        if (done.tripped) return true
        if (signal?.aborted) return false
        let dispose = noop
        const waits: Promise<unknown>[] = [this.space(), done.wait()]
        if (signal !== undefined) {
          waits.push(
            new Promise<void>((res) => {
              dispose = abortListener(signal, res)
            }),
          )
        }
        try {
          await Promise.race(waits)
        } finally {
          dispose()
        }
      }
    } finally {
      release()
    }
  }

  // readable resolves when the queue may have an element; callers re-check
  // with tryTake and loop.
  readable(): Promise<void> {
    if (this.buf.length > 0) return Promise.resolve()
    return new Promise((res) => this.readWaiters.push(res))
  }

  private space(): Promise<void> {
    return new Promise((res) => this.spaceWaiters.push(res))
  }
}

function wake(waiters: (() => void)[]): void {
  if (waiters.length === 0) return
  const ws = waiters.splice(0)
  for (const w of ws) w()
}

// Sweeper drives periodic work while there is any; it stops itself when idle
// and is kicked back to life by state mutations (PROTOCOL.md Appendix C).
export class Sweeper {
  private timer: ReturnType<typeof setInterval> | undefined
  private stopped = false

  kick(intervalMs: number, sweep: () => void, hasWork: () => boolean): void {
    if (this.stopped || this.timer !== undefined) return
    const t = setInterval(() => {
      sweep()
      if (this.timer === t && !hasWork()) {
        clearInterval(t)
        this.timer = undefined
      }
    }, intervalMs)
    unrefTimer(t)
    this.timer = t
  }

  // stop terminates the loop at once and prevents future kicks; Conn.close /
  // Server.stop use it instead of waiting for the last tombstone to expire.
  stop(): void {
    this.stopped = true
    if (this.timer !== undefined) {
      clearInterval(this.timer)
      this.timer = undefined
    }
  }
}

// ---------------------------------------------------------------------------
// flow control (PROTOCOL.md §4.2.1) — reliable mode only
// ---------------------------------------------------------------------------
//
// HTTP/2's per-stream windows, counted in messages. Without it the only
// back-pressure a receiver has is to stall its read loop, and a reliable
// adapter delivers every call's frames from ONE loop (§4.2), so one slow
// consumer would stall every call on the channel. In a browser that is worse
// than head-of-line blocking: the event loop the stalled delivery runs on is
// the same one that would have to produce the grant, so a blocking receive
// path is a deadlock, never a delay.
//
// Beside the per-stream windows sits the connection window (§4.2.1, §15),
// one per peer, bounding what that peer can pin across ALL of its calls as
// RFC 9113 §6.9.1's does: a data frame needs one credit from its stream
// window AND one from the peer's connection window. A receiver advertises
// it as Frame.connWindow — the client on every OPEN, the server on every H
// and T — and a sender adopts the first advertisement it hears from a peer
// incarnation (observe, once): absent means the peer does no connection flow
// control and the window is off. Only the client ever assumes: W_CONN from
// the Conn's construction to the server's first H or T; the server's sender
// is created by an OPEN that already carries the advertisement. WINDOW sid=0
// adds credit and never enables; a receiver returns one credit for every
// data frame it received once that frame stops occupying a buffer
// (PeerFlowRx).

// W_INIT is the initial per-stream window a sender assumes before the peer's
// advertisement arrives — the same value as the default rx buffer, so the
// assumption is exact for a default receiver. It is also the reliable-mode rx
// buffer floor: a receiver that buffered less could be overrun before its own
// advertisement landed.
export const W_INIT = 32

// W_CONN is the connection window the client assumes toward a server
// incarnation until that incarnation's advertisement arrives — its first H
// or T (§4.2.1, §10.1, Appendix B) — and the floor of Limits.maxPeerWindow
// for the same reason W_INIT floors the rx buffer: a client streams on the
// assumption before the server's first H or T, and a receiver holding less
// would be overrun by a conforming sender. A fixed protocol constant,
// 32 × W_INIT.
export const W_CONN = 1024

// DEFAULT_STALL_MS is T_stall (§10.1): how long a send may park for credit
// before the call fails UNAVAILABLE. Unlike the other timers it runs in
// reliable mode too — that is the mode flow control exists in, and a park
// there has no other bound (no protocol timers, and it happens before the
// adapter's write path).
export const DEFAULT_STALL_MS = 30_000

// reliableRxSize raises a configured rx buffer to the flow-control floor in
// reliable mode (§4.2.1). Unreliable mode is untouched: there a full buffer
// drops by policy and no window is ever advertised.
export function reliableRxSize(size: number, reliable: boolean): number {
  return reliable && size < W_INIT ? W_INIT : size
}

// FlowAcquire says why a parked sender stopped waiting.
export type FlowAcquire =
  // Credit was taken; the message may go on the wire.
  | 'ok'
  // The call ended underneath the sender (end-of-stream, not an error).
  | 'ended'
  // The caller's signal aborted.
  | 'aborted'
  // T_stall elapsed with no grant: the call fails UNAVAILABLE.
  | 'stalled'

// FlowAcquireBoth is acquireBoth's outcome: FlowAcquire plus which window a
// T_stall expiry found the sender parked on — 'stalled' names the stream,
// 'peer-stalled' the peer's connection window (§4.2.1, §14).
export type FlowAcquireBoth = FlowAcquire | 'peer-stalled'

// SenderState is a connection sender's position — on, observed (the
// once-only advertisement latch), granted and sent — carried across its
// container's eviction (PeerFlowRx.stash, §9.4, §15) so a recreated container
// continues where it left off.
export interface SenderState {
  on: boolean
  observed: boolean
  granted: number
  sent: number
}

// FlowSender is the sending half: how much the peer has allowed, how much has
// been sent, and a parking spot for the difference.
export class FlowSender {
  private on = false
  private observed = false
  private granted = 0
  private sent = 0
  private readonly waiters: (() => void)[] = []

  // assume starts flow control on the protocol's initial window, before the
  // peer has said anything (§4.2.1). Without it a client-streaming burst
  // could empty itself onto the wire before the ack it would be paced by. It
  // is the client's: a server's sender, per stream and per connection alike,
  // is created by an OPEN that already carries the advertisement. Refused
  // once the peer has advertised (observe) or the window is already on.
  assume(window: number): void {
    if (window <= 0 || this.observed || this.on) return
    this.on = true
    this.granted = window
  }

  // observe adopts the peer's advertised window — the per-stream one from
  // its OPEN or creation-ack H, the connection one from any OPEN (server
  // side) or any H or T (client side), §4.2.1: authoritative, replacing any
  // assumption and counted against what was already sent (a smaller window
  // simply parks the sender until the receiver drains). 0 — the field absent
  // — means the peer does no flow control on that window: off, whatever a
  // grant says afterwards. Once only: the first advertisement heard from a
  // peer incarnation wins and the rest are ignored; reassume re-arms the
  // latch for a new incarnation. Anyone parked is woken to re-race on the
  // advertised credit.
  observe(window: number): void {
    if (this.observed) return
    this.observed = true
    if (window <= 0) {
      this.on = false
    } else {
      this.on = true
      this.granted = window
    }
    wake(this.waiters)
  }

  // reassume restarts a connection window from scratch for a new peer
  // incarnation (§4.2.1, §10.6): assumed at window, unadvertised — the latch
  // re-armed for the new incarnation's first H or T — nothing sent.
  // A server that restarted on a surviving channel counts from zero, so the
  // cumulative sent count and any credit of the dead incarnation would never
  // line up with its grants again — a forever-park. Anyone parked is woken to
  // re-race on the fresh credit.
  reassume(window: number): void {
    this.on = window > 0
    this.observed = false
    this.granted = Math.max(window, 0)
    this.sent = 0
    wake(this.waiters)
  }

  // grant adds credit and wakes anyone parked. A grant never turns flow
  // control ON by itself: only an advertisement does (assume/observe).
  // Otherwise a stray, duplicated or injected WINDOW frame could park a
  // sender that was never flow-controlled — free of charge on a datagram
  // channel (§4.2.1, §15).
  grant(n: number): void {
    if (n <= 0 || !this.on) return
    // Saturating: a hostile or buggy peer's grants must not wrap the
    // accumulator into negative credit.
    this.granted = Math.min(this.granted + n, Number.MAX_SAFE_INTEGER)
    wake(this.waiters)
  }

  // undo returns one message of credit: the frame it was taken for never
  // reached the wire (a synchronous adapter refusal, §4.4, or a call that
  // ended between taking the credit and transmitting). Without it a caller
  // that ignores such errors leaks its whole window and parks forever — and
  // on the connection window, shared by every call to the peer and cumulative
  // for the incarnation's life, each such leak is a permanent shrink
  // (§4.2.1).
  undo(): void {
    if (this.sent > 0) this.sent--
    wake(this.waiters)
  }

  // release wakes every parked sender; the call is over.
  release(): void {
    this.on = false
    wake(this.waiters)
  }

  // tryAcquire consumes one message of credit without parking; false means
  // the sender must park (acquire). It exists so the whole send path stays
  // synchronous when there IS credit: an await between here and the frame's
  // seq allocation would let a racing abort take the number first.
  tryAcquire(): boolean {
    if (this.on && this.sent >= this.granted) return false
    this.sent++
    return true
  }

  // empty reports, taking nothing, whether a send would park here right now:
  // flow control is on and the credit is spent. acquireBoth asks the
  // connection window this when the stream window is short, so that a park
  // short on both is reported as the connection one (§4.2.1, §14).
  empty(): boolean {
    return this.on && this.sent >= this.granted
  }

  // state reads the position for a stash (SenderState); restore continues a
  // fresh sender from one. Nothing is parked on a fresh sender, so restore
  // has no one to wake.
  state(): SenderState {
    return { on: this.on, observed: this.observed, granted: this.granted, sent: this.sent }
  }

  restore(st: SenderState): void {
    this.on = st.on
    this.observed = st.observed
    this.granted = st.granted
    this.sent = st.sent
  }

  // acquire consumes one message of credit, parking until there is some. The
  // park is bounded by the call ending, by the caller's signal, and by
  // T_stall — the last one is load-bearing in reliable mode, where nothing
  // else would ever break it (§4.2.1).
  //
  // Which bound woke the park decides the outcome, the way Go's select does:
  // only a grant loops back to take credit. The distinction matters because
  // release() — the call ending — also wakes a parked sender and leaves
  // tryAcquire() willing, so without it a sender unparked by its call's END
  // would read 'ok', carry on as if credited, and be reported as a flow
  // resume (§14) that never happened. Ends are checked before credit for the
  // same reason: a call that is over has no send to make.
  async acquire(done: Latch, stallMs: number, signal?: AbortSignal): Promise<FlowAcquire> {
    let timer: ReturnType<typeof setTimeout> | undefined
    let dispose = noop
    let bounds: Promise<FlowAcquire>[] | undefined
    try {
      for (;;) {
        if (done.tripped) return 'ended'
        if (signal?.aborted) return 'aborted'
        if (this.tryAcquire()) return 'ok'
        if (bounds === undefined) {
          // Armed once, at the first park: T_stall measures the whole wait,
          // not the interval between two partial grants.
          bounds = [done.wait().then(() => 'ended' as const)]
          if (stallMs > 0) {
            bounds.push(
              new Promise<FlowAcquire>((res) => {
                const t = setTimeout(() => res('stalled'), stallMs)
                unrefTimer(t)
                timer = t
              }),
            )
          }
          if (signal !== undefined) {
            bounds.push(
              new Promise<FlowAcquire>((res) => {
                dispose = abortListener(signal, () => res('aborted'))
              }),
            )
          }
        }
        const why = await Promise.race([this.parked().then(() => 'grant' as const), ...bounds])
        if (why !== 'grant') return why
      }
    } finally {
      if (timer !== undefined) clearTimeout(timer)
      dispose()
    }
  }

  // parked resolves on the next grant, undo, advertisement or release — the
  // channel Go's tryAcquire hands back; callers re-check with tryAcquire and
  // loop.
  parked(): Promise<void> {
    return new Promise((res) => this.waiters.push(res))
  }
}

// takeBoth takes one credit from the stream window and, when there is a
// connection window, one from it — stream first (see acquireBoth): on a
// connection shortfall the stream credit is refunded before reporting which
// window was short. Synchronous, like tryAcquire, for the same reason.
function takeBoth(stream: FlowSender, peer: FlowSender | undefined): 'ok' | 'stream' | 'peer' {
  if (!stream.tryAcquire()) return 'stream'
  if (peer !== undefined && !peer.tryAcquire()) {
    stream.undo()
    return 'peer'
  }
  return 'ok'
}

// tryAcquireBoth is acquireBoth's synchronous fast path: both credits taken,
// or nothing held; false means the sender must park (acquireBoth). It is the
// tryAcquire of the pair, for the same reason — the send path stays
// synchronous when there IS credit.
export function tryAcquireBoth(stream: FlowSender, peer: FlowSender | undefined): boolean {
  return takeBoth(stream, peer) === 'ok'
}

// acquireBoth consumes one message of credit from the stream window AND one
// from the peer's connection window (§4.2.1), parking until both are there.
// An undefined peer means no connection window (unreliable mode): it is then
// FlowSender.acquire.
//
// Stream credit is taken first; if the connection is then short the stream
// credit is refunded (undo) before parking. The order is load-bearing:
// connection-first would let streams parked on their own window hoard the
// shared budget until every stream parks — a mutual T_stall. Never holding
// one credit while parked on the other is what keeps a stuck stream from
// starving the healthy ones.
//
// A call that has already ended spends nothing: ends are checked right before
// the credits are taken, with no yield in between — the same guarantee Go's
// take-then-re-check-then-refund gives across its mutex gap — so a dead call
// cannot spend the connection window, shared by every call to the peer and
// cumulative for the incarnation's life, on a frame that will never go out.
// The callers refund at their own late exits for the same reason.
//
// One T_stall timer, armed at the first park, bounds the whole wait across
// both windows: §10.1 makes T_stall the longest a send may wait for credit,
// and two budgets would silently make it 2 × T_stall. onStall is called
// once, at the first park — synchronously, before this function first
// yields, so a stall counter reads it while the sender is still parked —
// with peer = true when the connection window is the one that is empty, and
// when both are, since a sender short on both waits on the peer's whole
// budget, not on one consumer (§14). On expiry the outcome names the window
// by the same rule: 'peer-stalled' or 'stalled'. Which bound woke the park
// decides the outcome, as in acquire: only a grant loops back to take
// credit.
export async function acquireBoth(
  stream: FlowSender,
  peer: FlowSender | undefined,
  done: Latch,
  stallMs: number,
  signal?: AbortSignal,
  onStall?: (peer: boolean) => void,
): Promise<FlowAcquireBoth> {
  let timer: ReturnType<typeof setTimeout> | undefined
  let dispose = noop
  let bounds: Promise<FlowAcquireBoth>[] | undefined
  try {
    for (;;) {
      if (done.tripped) return 'ended'
      if (signal?.aborted) return 'aborted'
      const short = takeBoth(stream, peer)
      if (short === 'ok') return 'ok'
      // Short on both: the connection window is the one to name.
      const onPeer = short === 'peer' || (peer !== undefined && peer.empty())

      if (bounds === undefined) {
        bounds = [done.wait().then(() => 'ended' as const)]
        if (stallMs > 0) {
          bounds.push(
            new Promise<FlowAcquireBoth>((res) => {
              const t = setTimeout(() => res('stalled'), stallMs)
              unrefTimer(t)
              timer = t
            }),
          )
        }
        if (signal !== undefined) {
          bounds.push(
            new Promise<FlowAcquireBoth>((res) => {
              dispose = abortListener(signal, () => res('aborted'))
            }),
          )
        }
        onStall?.(onPeer)
      }
      const wait = short === 'peer' ? peer!.parked() : stream.parked()
      const why = await Promise.race([wait.then(() => 'grant' as const), ...bounds])
      if (why === 'grant') continue
      if (why === 'stalled') return onPeer ? 'peer-stalled' : 'stalled'
      return why
    }
  } finally {
    if (timer !== undefined) clearTimeout(timer)
    dispose()
  }
}

// FlowReceiver is the receiving half: it counts messages the application has
// consumed and says when to send a grant. Grants are batched at half the
// window, as HTTP/2 stacks do, so a steady stream costs one small frame per
// window/2 messages.
export class FlowReceiver {
  private on = false
  private window = 0
  private pending = 0

  enable(window: number): void {
    this.on = window > 0
    this.window = window
  }

  // active reports whether this side grants credit, i.e. whether the peer is
  // expected to respect a window — and therefore whether a full buffer is the
  // peer's contract violation rather than this side's slowness.
  get active(): boolean {
    return this.on
  }

  // consumed reports that n messages left the buffer and returns the credit
  // to grant now (0 = nothing to send yet).
  consumed(n: number): number {
    if (!this.on || n <= 0) return 0
    this.pending += n
    if (this.pending * 2 < this.window) return 0
    const grant = this.pending
    this.pending = 0
    return grant
  }
}

// U32_MAX bounds the ledger's counters and the advertisement a peer's frame
// carries: both are uint32 on the wire (§5, §7), and a hostile peer's returns
// must saturate, never wrap.
export const U32_MAX = 0xffff_ffff

// PeerFlowRx is the receiving half of the connection window (§4.2.1, §15):
// one per transport peer on the server, one per Conn on the client. It is a
// physical ledger — outstanding counts the peer's data frames sitting in this
// endpoint's buffers, pending the credit retired and not yet granted — so
// junk cannot desync it: only a frame this ledger admitted can raise
// outstanding, and the bound is enforced on outstanding, never on what was
// granted.
//
// Every reliable-mode data frame received returns exactly one credit once it
// stops occupying a buffer: consumed, discarded at its call's end, or never
// buffered at all (off-shape, overrun-refused, RESET-drawn, tombstone drop).
// Grants are batched at half the window like FlowReceiver's, plus the
// starvation clause the §4.2.1 MUST requires here: with stuck consumers
// pinning most of the window, pending may never reach half of it while the
// sender is out of credit, so a grant also fires whenever outstanding +
// pending reaches the window — whatever is pending is everything the sender
// could still be waiting for.
//
// outstanding is one number: it is the bound, and the memory it bounds is
// pinned by the transport peer whatever incarnation sent it. pending is per
// incarnation, because a grant is addressed to one (peer_epoch, §6.1): two
// incarnations can coexist on one key — a client restarted at the same
// address on a datagram channel forced reliable, where no disconnectPeer
// fires — and credit the live one's frames returned, batched into a grant
// addressed to the dead one because its finished call tipped the batch,
// would be dropped by the client and lost for good: a permanent shrink of
// the live sender. So each incarnation is granted exactly what its own
// frames returned. The starvation clause reads the shared outstanding
// against one incarnation's pending, which can only fire early, never late.
export class PeerFlowRx {
  private window = 0 // maxPeerWindow; 0 = off (unreliable mode)
  private outstanding = 0 // admitted − retired: the peer's frames in our buffers
  private readonly pending = new Map<number, number>() // retired − granted, per incarnation (epoch)

  // The sending half of the containers the maxDeadPeers cap evicted (§9.4,
  // §15), by client epoch, in eviction order (a Map iterates in insertion
  // order), at most evictCap of them. An evicted incarnation may be idle
  // rather than dead — a client holding several Conns on one socket — and a
  // sender recreated with a full window from its next OPEN's advertisement,
  // against a client whose buffers still hold the evicted one's frames,
  // could overrun it (§4.2.1 Overrun). So the position — its window and its
  // credit — is kept, bounded like the containers are, and a grant addressed
  // to an evicted incarnation still lands on it, as does the credit of a
  // RESET-drawn data frame it sent. Past the cap the oldest is dropped,
  // credit held back for it included, and that incarnation starts over as a
  // new one would: at the window its OPEN advertises with nothing sent,
  // over-credited by whatever the dropped position had spent (§16).
  private readonly evicted = new Map<number, SenderState>()
  private evictCap = 0

  // enable sizes the ledger. evictCap is how many evicted senders it keeps
  // (server: maxDeadPeers; the client evicts nothing). The window saturates
  // at U32_MAX, what Go's uint32 parameter can hold: it is what the
  // advertisement puts on the wire (§5, §7) and what the grant rule measures
  // against, and an unbounded one would do neither (limits.ts clamps first;
  // this keeps the ledger honest on its own).
  enable(window: number, evictCap: number): void {
    this.window = Math.min(window, U32_MAX)
    this.evictCap = evictCap
  }

  // active reports whether this side bounds the peer, i.e. whether a data
  // frame must pass admit before it is buffered.
  get active(): boolean {
    return this.window > 0
  }

  // admit charges one frame about to be buffered. It refuses — false,
  // nothing charged — when the frame would take outstanding past the window:
  // that is the overrun the receiver fails the offending call INTERNAL for
  // (§4.2, §15). Off, everything is admitted and nothing counted.
  admit(): boolean {
    if (this.window <= 0) return true
    if (this.outstanding >= this.window) return false
    this.outstanding++
    return true
  }

  // retire reports that n admitted frames of incarnation epoch stopped
  // occupying a buffer and returns the credit to grant it now on sid 0 (0 =
  // nothing to send yet). credit = false retires without returning anything:
  // the frames came from a server incarnation the client has moved past,
  // whose calls are RESET-failed anyway (§10.6), so their credit has no one
  // to go to.
  retire(epoch: number, n: number, credit: boolean): number {
    if (this.window <= 0) return 0
    this.outstanding -= Math.min(n, this.outstanding)
    if (!credit) return 0
    this.hold(epoch, n)
    return this.due(epoch)
  }

  // unadmitted returns the credit of n frames incarnation epoch sent that
  // were never admitted — a data frame for an unknown, finished or
  // tombstoned sid, which draws a RESET (§9.3, §10.6) — without touching
  // outstanding: they never occupied a buffer, but the sender spent credit on
  // them and a window that never gets it back is a permanent shrink. Same
  // batching as retire.
  unadmitted(epoch: number, n: number): number {
    if (this.window <= 0) return 0
    this.hold(epoch, n)
    return this.due(epoch)
  }

  // unadmittedEvicted is unadmitted for an incarnation whose container the
  // maxDeadPeers cap evicted and whose sender position this ledger holds
  // (§9.4): a data frame it still had in flight for a call this side has
  // finished draws its RESET like any other, and its credit goes back to
  // that incarnation — the stash keeps the server→client direction exact,
  // and this keeps the other one. Held nowhere, nothing is held back: junk
  // creates no state, and a dropped position takes its held-back credit
  // with it.
  unadmittedEvicted(epoch: number, n: number): number {
    if (this.window <= 0 || !this.evicted.has(epoch)) return 0
    this.hold(epoch, n)
    return this.due(epoch)
  }

  // hold holds back n retired credits for incarnation epoch, saturating.
  private hold(epoch: number, n: number): void {
    this.pending.set(epoch, Math.min((this.pending.get(epoch) ?? 0) + n, U32_MAX))
  }

  // due applies the grant rule to one incarnation's held-back credit: half
  // the window, or the starvation clause against the shared outstanding.
  private due(epoch: number): number {
    const pending = this.pending.get(epoch) ?? 0
    if (pending === 0) return 0
    if (pending * 2 < this.window && this.outstanding + pending < this.window) return 0
    this.pending.delete(epoch)
    return pending
  }

  // renew moves the ledger past a dead peer incarnation (§4.2.1 Restart):
  // whatever was held back for it has no one left to go to and is dropped,
  // while the ledger itself carries over — outstanding still counts the dead
  // incarnation's frames until they drain, uncredited. Client only: a Conn
  // faces one server incarnation at a time, and the new one advertises its
  // own window on its first H or T.
  renew(): void {
    this.pending.clear()
  }

  // stash keeps the sender position of a container the maxDeadPeers cap is
  // evicting (see the field comment). Whatever the ledger holds back for
  // that incarnation stays with it.
  stash(epoch: number, state: SenderState): void {
    if (this.evictCap <= 0) {
      this.pending.delete(epoch)
      return
    }
    // A re-stash keeps its place in the eviction order, as a Map does.
    this.evicted.set(epoch, { ...state })
    while (this.evicted.size > this.evictCap) {
      const oldest = this.evicted.keys().next().value!
      this.evicted.delete(oldest)
      this.pending.delete(oldest)
    }
  }

  // unstash hands back the held position of an incarnation whose container
  // is being recreated, if it is still held — once.
  unstash(epoch: number): SenderState | undefined {
    const e = this.evicted.get(epoch)
    if (e === undefined) return undefined
    this.evicted.delete(epoch)
    return e
  }

  // creditEvicted applies a sid-0 grant addressed to an evicted incarnation
  // to its held position: the client is returning credit for frames that
  // were in flight or buffered at the eviction. Same rule as
  // FlowSender.grant — it never enables — and a grant for an incarnation
  // held nowhere is dropped.
  creditEvicted(epoch: number, n: number): void {
    if (n <= 0) return
    const e = this.evicted.get(epoch)
    if (e === undefined || !e.on) return
    e.granted = Math.min(e.granted + n, Number.MAX_SAFE_INTEGER)
  }
}

// ---------------------------------------------------------------------------
// per-call size caps (PROTOCOL.md §16, grpc-go parity)
// ---------------------------------------------------------------------------

// gRPC's own defaults (grpc-go rpc_util.go): 4 MiB received per message,
// effectively unlimited sent.
export const DEFAULT_MAX_RECV_MSG_SIZE = 4 * 1024 * 1024
export const DEFAULT_MAX_SEND_MSG_SIZE = 0x7fff_ffff

// sizeOr resolves an optional size limit: an explicitly configured value —
// including 0, which grpc-go reads as "reject everything" — wins over the
// default. Truthiness would turn a deliberate lockdown into an open door.
export function sizeOr(v: number | undefined, def: number): number {
  return v === undefined ? def : v
}

// checkSendSize enforces maxCallSendMsgSize with grpc-go's status and wording,
// measured on the bytes that go on the wire (i.e. after compression, §12.1).
export function checkSendSize(n: number, limit: number): void {
  if (n > limit) {
    throw new StatusError(Code.RESOURCE_EXHAUSTED, `drpc: trying to send message larger than max (${n} vs. ${limit})`)
  }
}

// checkRecvSize is the receive twin, measured on the DECOMPRESSED message.
export function checkRecvSize(n: number, limit: number): void {
  if (n > limit) {
    throw new StatusError(Code.RESOURCE_EXHAUSTED, `drpc: received message larger than max (${n} vs. ${limit})`)
  }
}

// ---------------------------------------------------------------------------
// message compression (PROTOCOL.md §12.1)
// ---------------------------------------------------------------------------
//
// Named on the OPEN, governing the whole call in both directions like the
// codec. Go plugs into grpc-go's encoding.Compressor registry; the browser has
// no registry, so the platform's CompressionStream/DecompressionStream are the
// implementation and the table below is the registry. A call that names
// something this runtime cannot provide fails loudly at creation — sending raw
// bytes under a compressor name the peer honors would corrupt every message.

// Compressor is one named message compressor. It is per message and
// stateless: a shared stream dictionary is forbidden, since in unreliable
// mode one lost message would make every later one undecodable (§12.1).
//
// Both halves may be async so the platform's CompressionStream (which has no
// synchronous form) plugs in unchanged next to a synchronous node:zlib. This
// is the same shape ServerOptions.compressors takes, so one registry entry
// serves both endpoints.
export interface Compressor {
  compress(data: Uint8Array): Uint8Array | Promise<Uint8Array>
  // decompress expands data, bounded by maxBytes: an implementation SHOULD
  // stop reading past it (that is what makes a decompression bomb cost
  // nothing) and MAY return up to maxBytes bytes. The core fails the call
  // with RESOURCE_EXHAUSTED whenever the result exceeds the call's receive
  // cap, so a truncated buffer can never be mistaken for a valid message.
  decompress(data: Uint8Array, maxBytes: number): Uint8Array | Promise<Uint8Array>
}

// WirePayload is one message as it reaches a frame: the bytes plus whether
// FlagCompressed must ride with them.
export interface WirePayload {
  bytes: Uint8Array
  compressed: boolean
}

export const rawPayload = (bytes: Uint8Array): WirePayload => ({ bytes, compressed: false })

// The compressor names this runtime can serve. "gzip" is the interop baseline
// (§12.1); "deflate" comes free with the same platform API. A name outside the
// table is unknown here — the client refuses the call, the server answers
// T{UNIMPLEMENTED}.
const FORMATS: Record<string, CompressionFormat> = { gzip: 'gzip', deflate: 'deflate' }

const compressorCache = new Map<string, Compressor | undefined>()

// getCompressor resolves a compressor name against the platform's streams, or
// undefined when this runtime cannot provide it ('' — no compression — is
// also undefined). Register the result as a Server compressor to make a
// TS server speak the same baseline:
//
//   new Server(tx, { compressors: { gzip: getCompressor('gzip')! } })
export function getCompressor(name: string): Compressor | undefined {
  if (name === '') return undefined
  const hit = compressorCache.get(name)
  if (hit !== undefined || compressorCache.has(name)) return hit
  const c = makeCompressor(name)
  compressorCache.set(name, c)
  return c
}

function makeCompressor(name: string): Compressor | undefined {
  const format = FORMATS[name]
  if (format === undefined) return undefined
  if (typeof CompressionStream === 'undefined' || typeof DecompressionStream === 'undefined') return undefined
  try {
    // Probe once: a runtime may ship the constructor without every format.
    new CompressionStream(format)
    new DecompressionStream(format)
  } catch {
    return undefined
  }
  return new StreamCompressor(format)
}

class StreamCompressor implements Compressor {
  constructor(private readonly format: CompressionFormat) {}

  async compress(data: Uint8Array): Promise<Uint8Array> {
    try {
      return await runTransform(new CompressionStream(this.format), data, Number.MAX_SAFE_INTEGER)
    } catch (err) {
      if (err instanceof StatusError) throw err
      throw new StatusError(Code.INTERNAL, `drpc: compressor: ${errMsg(err)}`)
    }
  }

  async decompress(data: Uint8Array, maxRecv: number): Promise<Uint8Array> {
    // The expansion is bounded exactly as grpc-go bounds it: one byte past
    // the cap fails with ResourceExhausted, so a bomb cannot allocate without
    // limit. A cap of 0 or less has no meaningful bound left, so the default
    // stands in (Go does the same).
    const limit = maxRecv > 0 ? maxRecv : DEFAULT_MAX_RECV_MSG_SIZE
    try {
      return await runTransform(new DecompressionStream(this.format), data, limit)
    } catch (err) {
      if (err instanceof StatusError) throw err
      throw new StatusError(Code.INTERNAL, `drpc: decompress: ${errMsg(err)}`)
    }
  }
}

// runTransform feeds data through one (de)compression transform and collects
// the output, refusing to accumulate more than limit bytes.
async function runTransform(ts: GenericTransformStream, data: Uint8Array, limit: number): Promise<Uint8Array> {
  const writer = (ts.writable as WritableStream<Uint8Array>).getWriter()
  const reader = (ts.readable as ReadableStream<Uint8Array>).getReader()
  // Write and read concurrently: the transform's internal queue is small, so
  // a large write only completes while the reader drains. The write's failure
  // is captured rather than left dangling — an unhandled rejection in a
  // browser is a console error the application cannot catch.
  let writeErr: unknown
  const written = (async () => {
    await writer.write(data)
    await writer.close()
  })().catch((err: unknown) => {
    writeErr = err
  })

  const chunks: Uint8Array[] = []
  let total = 0
  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      if (value === undefined) continue
      total += value.length
      if (total > limit) {
        throw new StatusError(Code.RESOURCE_EXHAUSTED, `drpc: received message after decompression larger than max (> ${limit})`)
      }
      chunks.push(value)
    }
  } catch (err) {
    // Cancelling the readable errors the writable, which settles the pending
    // write; it is never awaited here — a runtime that failed to propagate
    // would hang the call instead of failing it.
    reader.cancel().catch(noop)
    throw err
  }
  await written
  if (writeErr !== undefined) throw writeErr

  if (chunks.length === 1) return chunks[0]!
  const out = new Uint8Array(total)
  let at = 0
  for (const c of chunks) {
    out.set(c, at)
    at += c.length
  }
  return out
}

// compressPayload prepares one message for the wire. An empty payload is never
// compressed — a 0-byte message is meaningful (§5, §7) and gains nothing from
// a codec header — and compression that would EXPAND the payload is skipped:
// it would push the message past the channel's ceiling for nothing (§4.4).
// The per-frame flag makes either decision invisible to the receiver.
export async function compressPayload(comp: Compressor, payload: Uint8Array): Promise<WirePayload> {
  if (payload.length === 0) return rawPayload(payload)
  const out = await comp.compress(payload)
  if (out.length >= payload.length) return rawPayload(payload)
  return { bytes: out, compressed: true }
}

// decompressPayload is the receive twin: the message bytes a COMPRESSED frame
// carries, expanded under the receive cap. A frame marked COMPRESSED on a call
// with no compressor is unreadable — fail rather than hand the codec garbage.
export async function decompressPayload(comp: Compressor | undefined, payload: Uint8Array, maxRecv: number): Promise<Uint8Array> {
  if (comp === undefined) {
    throw new StatusError(Code.INTERNAL, 'drpc: frame is compressed but the call has no compressor')
  }
  return comp.decompress(payload, maxRecv)
}

function errMsg(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}
