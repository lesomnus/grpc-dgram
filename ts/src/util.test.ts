// The concurrency primitives (src/util.ts). The FIFO test regression-pins the
// audit finding that putBlocking could let a later putter steal a freed slot
// from an earlier parked one, reordering reliable-mode delivery (§14).

import { describe, expect, it } from 'vitest'
import { FrameQueue, Latch } from './util'
import { frame, type Frame } from './wire'

const F = (seq: number): Frame => frame({ seq })

describe('Latch', () => {
  it('is a one-shot broadcast with no lost wakeup (late waiters see it tripped)', async () => {
    const l = new Latch()
    expect(l.tripped).toBe(false)
    const early = l.wait()
    l.trip()
    l.trip() // idempotent
    expect(l.tripped).toBe(true)
    await early
    await l.wait() // a waiter registered after the trip still resolves
  })
})

describe('FrameQueue.putBlocking FIFO (§14)', () => {
  it('preserves call order when multiple putters block on a full buffer', async () => {
    const q = new FrameQueue(1)
    const done = new Latch()
    expect(q.tryPut(F(0))).toBe(true) // fill the single slot

    // Two putters block: with the pre-fix wake-all+race, F2 could win the slot
    // freed for F1; the FIFO chain forbids it.
    const p1 = q.putBlocking(F(1), done)
    const p2 = q.putBlocking(F(2), done)

    const taken: number[] = []
    taken.push(q.tryTake()!.seq) // remove F0 → frees the slot for the head putter
    expect(await p1).toBe(true)
    taken.push(q.tryTake()!.seq) // F1 (the head putter), not F2
    expect(await p2).toBe(true)
    taken.push(q.tryTake()!.seq) // F2

    expect(taken).toEqual([0, 1, 2])
  })

  it('a blocked putter returns true when the stream ends (done)', async () => {
    const q = new FrameQueue(1)
    const done = new Latch()
    q.tryPut(F(0))
    const p = q.putBlocking(F(1), done)
    done.trip() // the call finished; the frame is moot
    expect(await p).toBe(true)
  })

  it('a blocked putter returns false when its rx signal aborts (teardown)', async () => {
    const q = new FrameQueue(1)
    const done = new Latch()
    const ac = new AbortController()
    q.tryPut(F(0))
    const p = q.putBlocking(F(1), done, ac.signal)
    ac.abort()
    expect(await p).toBe(false) // fail-loud path: the frame is lost mid-delivery
  })
})

describe('FrameQueue.putDrop', () => {
  it('DropNewest keeps the buffered prefix and counts the drop', () => {
    const q = new FrameQueue(2)
    q.putDrop(F(1), 0 /* Newest */)
    q.putDrop(F(2), 0)
    q.putDrop(F(3), 0) // dropped
    expect([q.tryTake()!.seq, q.tryTake()!.seq]).toEqual([1, 2])
    expect(q.dropped).toBe(1)
  })

  it('DropOldest evicts the oldest to admit the newest', () => {
    const q = new FrameQueue(2)
    q.putDrop(F(1), 1 /* Oldest */)
    q.putDrop(F(2), 1)
    q.putDrop(F(3), 1) // evicts F1
    expect([q.tryTake()!.seq, q.tryTake()!.seq]).toEqual([2, 3])
    expect(q.dropped).toBe(1)
  })
})
