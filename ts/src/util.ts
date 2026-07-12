// Concurrency primitives translating the Go core's channels and goroutines
// into single-threaded async TypeScript. Synchronous code paths need no
// locks — an await point is the only place interleaving can happen.

import { DropPolicy } from './limits'
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
  // evicts the oldest to admit it.
  putDrop(f: Frame, policy: DropPolicy): void {
    if (this.tryPut(f)) return
    if (policy === DropPolicy.Oldest) {
      if (this.buf.shift() !== undefined) this.dropped++
      if (this.tryPut(f)) return
    }
    this.dropped++
  }

  // putBlocking delivers f, blocking until there is room: dropping would
  // violate the reliable-mode exact-sequence contract (PROTOCOL.md §14), so a
  // slow consumer stalls delivery instead and the stall propagates into the
  // adapter's own flow control (§4.2). Bounded by the stream ending (a frame
  // for a finished call is moot) and by the rx signal (adapter teardown); it
  // returns false only for the signal bound — the frame is lost while the
  // call is still live, which on a reliable channel must fail loud.
  async putBlocking(f: Frame, done: Latch, signal?: AbortSignal): Promise<boolean> {
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
