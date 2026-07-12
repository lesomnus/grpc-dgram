// The §5 golden vectors are the cross-implementation contract, copied
// byte-for-byte from the Go suite (wire_shape_test.go): this port must
// produce and accept encodings identical to the Go implementation.

import { describe, expect, it } from 'vitest'
import { decodeEnvelop, decodeFrame, encodeEnvelop, encodeFrame, FlagClose, FlagOpen, frame } from '../src/wire'

const hex = (b: Uint8Array) => [...b].map((x) => x.toString(16).padStart(2, '0')).join('')
const unhex = (s: string) => new Uint8Array([...(s.match(/../g) ?? [])].map((x) => parseInt(x, 16)))

// Frame{epoch:0x01020304 sid:5 seq:6 flags:OPEN|CLOSE method:"/a.B/C"
// codec:"json" timeout:1.5s payload:[0xAA] code:0(present) desc:"d"
// peer_epoch:0x0A0B0C0D} — header/trailer absent to keep the vector stable.
const goldenFrameHex =
  '0d0403020115050000001d0600000020032a062f612e422f433a046a736f6e420808011080cab5ee014a01aa50005a0164750d0c0b0a'

// Envelop{frames:[OPEN{epoch:1 sid:2 seq:1 flags:1 method:"/a.B/C"},
// data{epoch:1 sid:2 seq:2 payload:[0xAA]}]} — frames is field 1.
const goldenEnvelopHex = '0a190d0100000015020000001d0100000020012a062f612e422f430a120d0100000015020000001d020000004a01aa'

function goldenFrame() {
  const f = frame({
    epoch: 0x01020304,
    sid: 5,
    seq: 6,
    flags: FlagOpen | FlagClose,
    method: '/a.B/C',
    codec: 'json',
    timeoutMs: 1500,
    desc: 'd',
    peerEpoch: 0x0a0b0c0d,
  })
  f.payload = new Uint8Array([0xaa])
  f.code = 0 // presence is load-bearing: terminal CLOSE vs half-close (§5)
  return f
}

describe('golden bytes (§5)', () => {
  it('Frame encodes byte-identically to the Go implementation', () => {
    const f = goldenFrame()
    expect(hex(encodeFrame(f))).toBe(goldenFrameHex)
    expect(hex(encodeFrame(f))).toBe(hex(encodeFrame(f))) // deterministic
  })

  it('the golden Frame bytes round-trip every field', () => {
    const g = decodeFrame(unhex(goldenFrameHex))
    expect(g.epoch).toBe(0x01020304)
    expect(g.sid).toBe(5)
    expect(g.seq).toBe(6)
    expect(g.flags).toBe(FlagOpen | FlagClose)
    expect(g.method).toBe('/a.B/C')
    expect(g.codec).toBe('json')
    expect(g.timeoutMs).toBe(1500)
    expect(g.payload).toEqual(new Uint8Array([0xaa]))
    expect(g.code).toBe(0) // explicit presence: 0 must survive as present
    expect(g.desc).toBe('d')
    expect(g.header).toBeUndefined()
    expect(g.trailer).toBeUndefined()
    expect(g.peerEpoch).toBe(0x0a0b0c0d)
  })

  it('Envelop encodes byte-identically to the Go implementation', () => {
    const open = frame({ epoch: 1, sid: 2, seq: 1, flags: FlagOpen, method: '/a.B/C' })
    const data = frame({ epoch: 1, sid: 2, seq: 2 })
    data.payload = new Uint8Array([0xaa])
    expect(hex(encodeEnvelop([open, data]))).toBe(goldenEnvelopHex)
  })

  it('the golden Envelop bytes round-trip', () => {
    const fs = decodeEnvelop(unhex(goldenEnvelopHex))
    expect(fs).toHaveLength(2)
    expect(fs[0]!.epoch).toBe(1)
    expect(fs[0]!.sid).toBe(2)
    expect(fs[0]!.seq).toBe(1)
    expect(fs[0]!.flags).toBe(FlagOpen)
    expect(fs[0]!.method).toBe('/a.B/C')
    expect(fs[0]!.payload).toBeUndefined()
    expect(fs[1]!.seq).toBe(2)
    expect(fs[1]!.flags).toBe(0)
    expect(fs[1]!.payload).toEqual(new Uint8Array([0xaa]))
  })
})

describe('presence semantics (§5, §7)', () => {
  it('a zero-byte payload is present on the wire (data frame at 0 bytes)', () => {
    const f = frame({ epoch: 1, sid: 1, seq: 2 })
    f.payload = new Uint8Array(0)
    const g = decodeFrame(encodeFrame(f))
    expect(g.payload).toEqual(new Uint8Array(0))
    const h = frame({ epoch: 1, sid: 1, seq: 2 }) // header frame: absent payload
    expect(decodeFrame(encodeFrame(h)).payload).toBeUndefined()
  })

  it('code 0 (OK) is distinguishable from an absent code', () => {
    const half = frame({ epoch: 1, sid: 1, seq: 3, flags: FlagClose })
    expect(decodeFrame(encodeFrame(half)).code).toBeUndefined()
    const term = frame({ epoch: 1, sid: 1, seq: 3, flags: FlagClose })
    term.code = 0
    expect(decodeFrame(encodeFrame(term)).code).toBe(0)
  })

  it('implicit-presence fields at their default are omitted', () => {
    expect(encodeFrame(frame())).toEqual(new Uint8Array(0))
  })

  it('peer_epoch 0 means absent (§6.1) and is omitted', () => {
    const f = frame({ epoch: 1, peerEpoch: 0 })
    expect(hex(encodeFrame(f))).toBe('0d01000000')
  })
})

describe('metadata', () => {
  it('round-trips multi-value entries', () => {
    const f = frame({ epoch: 1, sid: 1, seq: 1 })
    f.header = { 'x-a': ['1', '2'], 'x-b': [''] }
    f.trailer = { done: ['yes'] }
    const g = decodeFrame(encodeFrame(f))
    expect(g.header).toEqual({ 'x-a': ['1', '2'], 'x-b': [''] })
    expect(g.trailer).toEqual({ done: ['yes'] })
  })

  it('an empty metadata message is present but empty', () => {
    const f = frame({ epoch: 1 })
    f.header = {}
    const g = decodeFrame(encodeFrame(f))
    expect(g.header).toEqual({})
    expect(g.trailer).toBeUndefined()
  })
})

describe('robustness', () => {
  it('unknown fields are skipped (field 6 reserved; future additions)', () => {
    // field 6 varint 7, field 99 length-delimited "xx" appended to a valid frame.
    const base = encodeFrame(frame({ epoch: 1, sid: 2, seq: 3 }))
    const extra = new Uint8Array([0x30, 0x07, 0x9a, 0x06, 0x02, 0x78, 0x78])
    const joined = new Uint8Array([...base, ...extra])
    const g = decodeFrame(joined)
    expect(g.epoch).toBe(1)
    expect(g.sid).toBe(2)
    expect(g.seq).toBe(3)
  })

  it('truncated input throws', () => {
    const bytes = encodeFrame(goldenFrame())
    expect(() => decodeFrame(bytes.subarray(0, bytes.length - 3))).toThrow()
  })

  it('an empty envelop decodes to no frames', () => {
    expect(decodeEnvelop(new Uint8Array(0))).toEqual([])
  })
})
