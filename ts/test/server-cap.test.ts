// Regression for the audit finding that a call finishing after its peer was
// disconnected and the same peer key reused decremented the WRONG slot's
// live-call counter, under-enforcing the §15 per-peer MaxLiveCalls cap.

import { describe, expect, it } from 'vitest'
import { Server } from '../src/server'
import { Code } from '../src/status'
import { FlagOpen, isTerminal, type Frame } from '../src/wire'
import { echo, registerEcho, tick } from './helpers'

function openLive(epoch: number, sid: number): Frame {
  return { epoch, sid, seq: 1, flags: FlagOpen, method: echo.live.path, codec: '', desc: '', peerEpoch: 0 }
}

describe('§15 MaxLiveCalls across a disconnect + same-key reuse', () => {
  it('a post-disconnect finish does not under-count the reused peer, so the cap still holds', async () => {
    const sent: Frame[] = []
    const server = new Server({ handle: (f: Frame) => void sent.push(f) }, { reliable: false, limits: { maxLiveCalls: 1 } })
    registerEcho(server) // echo.live blocks on recv until aborted
    const P = 'stable-peer'

    // Call A on incarnation 1: one live call on this peer's slot.
    await server.handle(openLive(1, 1), { peer: P })
    await tick()

    // The transport hiccups on a stable-key adapter: the slot is deleted, A is
    // cancelled but still unwinding. A new incarnation reuses the same key
    // BEFORE A's finish runs (synchronous handle, no await between).
    server.disconnectPeer(P, new Error('hiccup'))
    await server.handle(openLive(2, 1), { peer: P }) // call B, fresh slot

    // Let A's finish() run — with the bug it decremented B's slot to 0.
    await tick()
    await tick()

    // Cap is 1 and B is live, so a second call on incarnation 2 must be
    // refused with RESOURCE_EXHAUSTED (not admitted by an under-counted slot).
    await server.handle(openLive(2, 2), { peer: P })
    await tick()

    const rej = sent.find((f) => isTerminal(f) && f.sid === 2 && f.code === Code.RESOURCE_EXHAUSTED)
    expect(rej, 'the reused peer must still hit MaxLiveCalls=1').toBeDefined()
    await server.stop()
  })
})
