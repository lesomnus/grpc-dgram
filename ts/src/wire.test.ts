// The §5 golden vectors are the cross-implementation contract, copied
// byte-for-byte from the Go suite (wire_shape_test.go) or generated from the
// same google.golang.org/protobuf marshaler against the normative Go
// implementation: this port must produce and accept encodings identical to it.

import { describe, expect, it } from 'vitest'
import {
  decodeBase64,
  decodeMetadataValue,
  encodeBase64,
  encodeMetadataValue,
  isBinaryKey,
  validateMetadata,
  validateMetadataPair,
} from './metadata'
import { Code, StatusError } from './status'
import {
  decodeEnvelop,
  decodeFrame,
  encodeEnvelop,
  encodeFrame,
  FlagClose,
  FlagCompressed,
  FlagOpen,
  FlagPing,
  FlagReset,
  FlagWindow,
  frame,
  hasUnknownFlags,
  isCompressed,
  isData,
  isHalfClose,
  isHeaderFrame,
  isTerminal,
  isWindow,
  KNOWN_FLAGS,
  legalShape,
  SHAPE_MASK,
  shapeOf,
} from './wire'

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
    // v1.1 fields at their defaults on a v1.0 vector.
    expect(g.window).toBe(0)
    expect(g.compressor).toBe('')
    expect(g.details).toBeUndefined()
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

// ---------------------------------------------------------------------------
// wire v1.1 — the fields and metadata encoding added on top of v1.0. Every
// vector below was produced by google.golang.org/protobuf marshaling the same
// message against the Go core (deterministic mode, which sorts map keys the
// way this codec does), so the two implementations are pinned byte-for-byte.
// ---------------------------------------------------------------------------

describe('golden bytes — wire v1.1 (§5)', () => {
  it('a "-bin" metadata value carries raw octets (0x00/0xff), base64 in the TS API', () => {
    // Metadata{entries:{"x-bin": {values:[00 01 ff 80 7f]}}} on Frame{epoch:1}.
    const want = '0d0100000062120a100a05782d62696e12070a050001ff807f'
    const f = frame({ epoch: 1 })
    f.header = { 'x-bin': ['AAH/gH8='] }
    expect(hex(encodeFrame(f))).toBe(want)

    const g = decodeFrame(unhex(want))
    expect(g.header).toEqual({ 'x-bin': ['AAH/gH8='] })
    // The octets themselves — what Go's metadata.MD string holds verbatim.
    expect(decodeBase64(g.header!['x-bin']![0]!)).toEqual(new Uint8Array([0x00, 0x01, 0xff, 0x80, 0x7f]))
  })

  it('a text metadata key is UTF-8 bytes on the wire (byte-identical to the old string field)', () => {
    // Metadata{entries:{"x-text": {values:["hello", "wor ld"]}}}.
    const want = '0d01000000621b0a190a06782d74657874120f0a0568656c6c6f0a06776f72206c64'
    const f = frame({ epoch: 1 })
    f.header = { 'x-text': ['hello', 'wor ld'] }
    expect(hex(encodeFrame(f))).toBe(want)
    expect(decodeFrame(unhex(want)).header).toEqual({ 'x-text': ['hello', 'wor ld'] })
  })

  it('a present-but-empty Metadata message is two bytes', () => {
    const want = '0d010000006200'
    const f = frame({ epoch: 1 })
    f.header = {}
    expect(hex(encodeFrame(f))).toBe(want)
    const g = decodeFrame(unhex(want))
    expect(g.header).toEqual({})
    expect(g.trailer).toBeUndefined()
  })

  it('a key with no values keeps the map entry (key + empty Entry message)', () => {
    const want = '0d0100000062090a070a03782d611200'
    const f = frame({ epoch: 1 })
    f.header = { 'x-a': [] }
    expect(hex(encodeFrame(f))).toBe(want)
    expect(decodeFrame(unhex(want)).header).toEqual({ 'x-a': [] })
  })

  it('a key with one empty value is distinct from a key with no values', () => {
    const want = '0d01000000620b0a090a03782d6112020a00'
    const f = frame({ epoch: 1 })
    f.header = { 'x-a': [''] }
    expect(hex(encodeFrame(f))).toBe(want)
    expect(decodeFrame(unhex(want)).header).toEqual({ 'x-a': [''] })
    // ...and the two encode differently: one value of 0 bytes vs no values.
    const none = frame({ epoch: 1 })
    none.header = { 'x-a': [] }
    expect(hex(encodeFrame(f))).not.toBe(hex(encodeFrame(none)))
  })

  it('text and binary keys mix in one Metadata, sorted by key', () => {
    // trailer{"a-text":["v"], "z-bin":[ff 00]} — Go's deterministic marshal
    // sorts map keys, as this codec does.
    const want = '0d010000006a1e0a0d0a06612d7465787412030a01760a0d0a057a2d62696e12040a02ff00'
    const f = frame({ epoch: 1 })
    f.trailer = { 'z-bin': ['/wA='], 'a-text': ['v'] } // insertion order is not encode order
    expect(hex(encodeFrame(f))).toBe(want)
    expect(decodeFrame(unhex(want)).trailer).toEqual({ 'a-text': ['v'], 'z-bin': ['/wA='] })
  })

  it('window is field 15, a varint', () => {
    // OPEN advertising a 32-message receive window (§4.2).
    const want = '0d0100000015020000001d0100000020012a062f612e422f437820'
    const f = frame({ epoch: 1, sid: 2, seq: 1, flags: FlagOpen, method: '/a.B/C', window: 32 })
    expect(hex(encodeFrame(f))).toBe(want)
    expect(decodeFrame(unhex(want)).window).toBe(32)
  })

  it('compressor is field 16, a string', () => {
    const want = '0d0100000015020000001d0100000020012a062f612e422f43820104677a6970'
    const f = frame({ epoch: 1, sid: 2, seq: 1, flags: FlagOpen, method: '/a.B/C', compressor: 'gzip' })
    expect(hex(encodeFrame(f))).toBe(want)
    expect(decodeFrame(unhex(want)).compressor).toBe('gzip')
  })

  it('details is field 17, repeated google.protobuf.Any', () => {
    // T{RESOURCE_EXHAUSTED} carrying two Any details, the second with an
    // empty value (implicit presence: the value field is omitted).
    const want =
      '0d0100000015020000001d03000000200250085a046e6f70658a01300a28747970652e676f6f676c65617069732e636f6d2f676f6f676c652e7270632e5265747279496e666f12040a0208018a01030a0178'
    const f = frame({ epoch: 1, sid: 2, seq: 3, flags: FlagClose, desc: 'nope' })
    f.code = Code.RESOURCE_EXHAUSTED
    f.details = [
      { typeUrl: 'type.googleapis.com/google.rpc.RetryInfo', value: new Uint8Array([0x0a, 0x02, 0x08, 0x01]) },
      { typeUrl: 'x', value: new Uint8Array(0) },
    ]
    expect(hex(encodeFrame(f))).toBe(want)

    const g = decodeFrame(unhex(want))
    expect(g.details).toEqual([
      { typeUrl: 'type.googleapis.com/google.rpc.RetryInfo', value: new Uint8Array([0x0a, 0x02, 0x08, 0x01]) },
      { typeUrl: 'x', value: new Uint8Array(0) },
    ])
    expect(isTerminal(g)).toBe(true)
  })

  it('a COMPRESSED data frame is still a data frame (§7.1)', () => {
    const want = '0d0100000015020000001d0200000020204a04deadbeef'
    const f = frame({ epoch: 1, sid: 2, seq: 2, flags: FlagCompressed })
    f.payload = new Uint8Array([0xde, 0xad, 0xbe, 0xef])
    expect(hex(encodeFrame(f))).toBe(want)

    const g = decodeFrame(unhex(want))
    expect(g.flags).toBe(FlagCompressed)
    expect(shapeOf(g)).toBe(0)
    expect(isCompressed(g)).toBe(true)
    expect(isData(g)).toBe(true) // the whole point of masking (flags !== 0)
    expect(isHeaderFrame(g)).toBe(false)
    expect(hasUnknownFlags(g)).toBe(false)
  })

  it('every v1.1 field at once, in field-number order', () => {
    const want =
      '0d0403020115050000001d060000002010788020820104677a69708a011c0a17747970652e676f6f676c65617069732e636f6d2f612e421201aa'
    const f = frame({
      epoch: 0x01020304,
      sid: 5,
      seq: 6,
      flags: FlagWindow,
      window: 4096,
      compressor: 'gzip',
      details: [{ typeUrl: 'type.googleapis.com/a.B', value: new Uint8Array([0xaa]) }],
    })
    expect(hex(encodeFrame(f))).toBe(want)

    const g = decodeFrame(unhex(want))
    expect(g.flags).toBe(FlagWindow)
    expect(g.window).toBe(4096)
    expect(g.compressor).toBe('gzip')
    expect(g.details).toHaveLength(1)
    expect(isWindow(g)).toBe(true)
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
    // window 0 / compressor "" / no details are defaults too (§5).
    expect(encodeFrame(frame({ window: 0, compressor: '', details: [] }))).toEqual(new Uint8Array(0))
  })

  it('peer_epoch 0 means absent (§6.1) and is omitted', () => {
    const f = frame({ epoch: 1, peerEpoch: 0 })
    expect(hex(encodeFrame(f))).toBe('0d01000000')
  })

  it('an empty details list decodes back to absent', () => {
    const f = frame({ epoch: 1, details: [] })
    expect(decodeFrame(encodeFrame(f)).details).toBeUndefined()
  })
})

describe('flags and shapes (§7, §7.1)', () => {
  it('the masks are the documented bit sets', () => {
    expect(SHAPE_MASK).toBe(0x1f)
    expect(KNOWN_FLAGS).toBe(0x3f)
    expect(FlagWindow).toBe(16)
    expect(FlagCompressed).toBe(32)
  })

  it('shapeOf strips the orthogonal COMPRESSED modifier', () => {
    expect(shapeOf(frame({ flags: FlagCompressed }))).toBe(0)
    expect(shapeOf(frame({ flags: FlagClose | FlagCompressed }))).toBe(FlagClose)
    expect(shapeOf(frame({ flags: FlagOpen | FlagClose | FlagCompressed }))).toBe(FlagOpen | FlagClose)
  })

  it('legalShape accepts exactly the routable shapes', () => {
    for (const s of [0, FlagOpen, FlagClose, FlagOpen | FlagClose, FlagReset, FlagPing, FlagWindow]) {
      expect(legalShape(s)).toBe(true)
    }
    for (const s of [
      FlagOpen | FlagReset,
      FlagOpen | FlagPing,
      FlagClose | FlagReset,
      FlagReset | FlagPing,
      FlagWindow | FlagOpen,
      FlagWindow | FlagClose,
      FlagOpen | FlagClose | FlagReset,
      SHAPE_MASK,
    ]) {
      expect(legalShape(s)).toBe(false)
    }
    // COMPRESSED is not a shape: it must be masked off before the test.
    expect(legalShape(FlagCompressed)).toBe(false)
    expect(legalShape(shapeOf(frame({ flags: FlagClose | FlagCompressed })))).toBe(true)
  })

  it('hasUnknownFlags catches any bit outside 0x3f, including the sign bit', () => {
    expect(hasUnknownFlags(frame({ flags: 0 }))).toBe(false)
    expect(hasUnknownFlags(frame({ flags: KNOWN_FLAGS }))).toBe(false)
    expect(hasUnknownFlags(frame({ flags: 64 }))).toBe(true)
    expect(hasUnknownFlags(frame({ flags: FlagOpen | 0x100 }))).toBe(true)
    expect(hasUnknownFlags(frame({ flags: 0x80000000 }))).toBe(true)
    expect(hasUnknownFlags(frame({ flags: 0xffffffff }))).toBe(true)
    // A flags value from the wire is uint32-ranged.
    expect(hasUnknownFlags(decodeFrame(encodeFrame(frame({ flags: 0x80000000 }))))).toBe(true)
  })

  it('shape tests mask, so a COMPRESSED frame keeps its role', () => {
    const term = frame({ flags: FlagClose | FlagCompressed })
    term.code = Code.OK
    term.payload = new Uint8Array([1])
    expect(isTerminal(term)).toBe(true)
    expect(isHalfClose(term)).toBe(false)

    const half = frame({ flags: FlagClose })
    expect(isHalfClose(half)).toBe(true)
    expect(isTerminal(half)).toBe(false)

    // OPEN|CLOSE is a request shape, never a terminal (Go: shape() == CLOSE).
    const unary = frame({ flags: FlagOpen | FlagClose })
    unary.code = Code.OK
    expect(isTerminal(unary)).toBe(false)

    const h = frame({ flags: FlagCompressed })
    expect(isHeaderFrame(h)).toBe(true)
    expect(isData(h)).toBe(false)

    const w = frame({ flags: FlagWindow, window: 8 })
    expect(isWindow(w)).toBe(true)
    expect(isData(w)).toBe(false)
    expect(isHeaderFrame(w)).toBe(false) // a WINDOW frame is never delivered
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

  it('round-trips every octet through a "-bin" key', () => {
    const all = new Uint8Array(256)
    for (let i = 0; i < 256; i++) all[i] = i
    const f = frame({ epoch: 1 })
    f.header = { 'trace-bin': [encodeBase64(all)] }
    const g = decodeFrame(encodeFrame(f))
    expect(decodeBase64(g.header!['trace-bin']![0]!)).toEqual(all)
  })

  it('the per-key transform runs after the entry closes (key may follow the values)', () => {
    // A hand-built map entry with field 2 (values) BEFORE field 1 (key):
    // legal protobuf, and the only order under which a value-time transform
    // would mis-decode a "-bin" key as text.
    const g = decodeFrame(unhex('0d01000000620f0a0d12040a0200ff0a05782d62696e'))
    expect(g.header).toEqual({ 'x-bin': ['AP8='] })
    expect(decodeBase64(g.header!['x-bin']![0]!)).toEqual(new Uint8Array([0x00, 0xff]))
  })

  it('a repeated Entry field merges its values (proto message merge)', () => {
    // key "x-a" with two `values` submessages: ["a"] then ["b"].
    const g = decodeFrame(unhex('0d0100000062110a0f0a03782d6112030a016112030a0162'))
    expect(g.header).toEqual({ 'x-a': ['a', 'b'] })
  })

  it('a non-UTF-8 text value decodes lossily instead of throwing', () => {
    // Metadata{"x-a": [ff]} — invalid UTF-8 for a text key.
    const g = decodeFrame(unhex('0d01000000620c0a0a0a03782d6112030a01ff'))
    expect(g.header!['x-a']).toEqual(['�'])
  })

  it('an unparseable binary value fails the call INTERNAL, never a codec crash', () => {
    const f = frame({ epoch: 1 })
    f.header = { 'x-bin': ['not base64!'] }
    let err: unknown
    try {
      encodeFrame(f)
    } catch (e) {
      err = e
    }
    expect(err).toBeInstanceOf(StatusError)
    expect((err as StatusError).code).toBe(Code.INTERNAL)
  })
})

describe('metadata base64 boundary (§11)', () => {
  it('isBinaryKey follows the "-bin" convention', () => {
    expect(isBinaryKey('trace-bin')).toBe(true)
    expect(isBinaryKey('-bin')).toBe(true)
    expect(isBinaryKey('trace-binary')).toBe(false)
    expect(isBinaryKey('bin')).toBe(false)
  })

  it('encodeBase64 emits canonical padded standard base64', () => {
    expect(encodeBase64(new Uint8Array(0))).toBe('')
    expect(encodeBase64(new Uint8Array([0x66]))).toBe('Zg==')
    expect(encodeBase64(new Uint8Array([0x66, 0x6f]))).toBe('Zm8=')
    expect(encodeBase64(new Uint8Array([0x66, 0x6f, 0x6f]))).toBe('Zm9v')
    expect(encodeBase64(new Uint8Array([0xff, 0xfe, 0xfd]))).toBe('//79')
  })

  it('decodeBase64 accepts padded and unpadded input', () => {
    expect(decodeBase64('Zg==')).toEqual(new Uint8Array([0x66]))
    expect(decodeBase64('Zg')).toEqual(new Uint8Array([0x66]))
    expect(decodeBase64('Zm8=')).toEqual(new Uint8Array([0x66, 0x6f]))
    expect(decodeBase64('Zm8')).toEqual(new Uint8Array([0x66, 0x6f]))
    expect(decodeBase64('')).toEqual(new Uint8Array(0))
  })

  it('decodeBase64 rejects malformed input (never a silent truncation)', () => {
    expect(() => decodeBase64('Z')).toThrow() // truncated group
    expect(() => decodeBase64('Zm9v!')).toThrow() // illegal character
    expect(() => decodeBase64('Zm 9v')).toThrow() // whitespace is not stripped
    expect(() => decodeBase64('Zg=')).toThrow() // padding to a non-multiple of 4
    expect(() => decodeBase64('Z===')).toThrow() // three pad chars
    expect(() => decodeBase64('Zm9-')).toThrow() // base64url is not standard base64
  })

  it('round-trips 0..1024-byte payloads', () => {
    for (const n of [0, 1, 2, 3, 4, 5, 17, 255, 1024]) {
      const b = new Uint8Array(n)
      for (let i = 0; i < n; i++) b[i] = (i * 37 + 11) & 0xff
      expect(decodeBase64(encodeBase64(b))).toEqual(b)
    }
  })

  it('the value transform is keyed on the "-bin" suffix', () => {
    expect(encodeMetadataValue('x-a', 'hi')).toEqual(new Uint8Array([0x68, 0x69]))
    expect(encodeMetadataValue('x-bin', '//79')).toEqual(new Uint8Array([0xff, 0xfe, 0xfd]))
    expect(decodeMetadataValue('x-a', new Uint8Array([0x68, 0x69]))).toBe('hi')
    expect(decodeMetadataValue('x-bin', new Uint8Array([0xff, 0xfe, 0xfd]))).toBe('//79')
    // Multi-byte UTF-8 text survives (the wire holds its UTF-8 octets).
    expect(decodeMetadataValue('x-a', encodeMetadataValue('x-a', 'héllo'))).toBe('héllo')
  })
})

describe('metadata validation (§11, grpc-go parity)', () => {
  const bad = (fn: () => void): StatusError => {
    let err: unknown
    try {
      fn()
    } catch (e) {
      err = e
    }
    expect(err).toBeInstanceOf(StatusError)
    expect((err as StatusError).code).toBe(Code.INTERNAL)
    return err as StatusError
  }

  it('accepts what grpc-go accepts', () => {
    expect(() => validateMetadata(undefined)).not.toThrow()
    expect(() => validateMetadata({})).not.toThrow()
    expect(() =>
      validateMetadata({
        'x-a': ['1', '2'],
        'a.b_c-d': [''],
        '0': [' ~'], // the printable-ASCII bounds
        'trace-bin': ['AAH/gH8='],
      }),
    ).not.toThrow()
  })

  it('rejects an empty key', () => {
    expect(bad(() => validateMetadata({ '': ['v'] })).desc).toBe('there is an empty key in the header')
  })

  it('rejects keys outside [0-9a-z-_.]', () => {
    for (const k of ['X-a', 'x a', 'x:a', 'x/a', 'héllo']) {
      expect(bad(() => validateMetadata({ [k]: ['v'] })).desc).toContain('illegal characters not in [0-9a-z-_.]')
    }
  })

  it('rejects non-printable-ASCII values on text keys', () => {
    for (const v of ['\n', '\x00', '\x7f', 'é', 'a\tb']) {
      expect(bad(() => validateMetadata({ 'x-a': [v] })).desc).toContain('non-printable ASCII')
    }
  })

  it('does not constrain the octets a "-bin" key carries', () => {
    // Bytes that are illegal as a text value are legal as binary metadata.
    const raw = new Uint8Array([0x00, 0x0a, 0x7f, 0xff])
    expect(() => validateMetadata({ 'x-bin': [encodeBase64(raw)] })).not.toThrow()
    // ...but the TS representation of those octets must be base64.
    expect(bad(() => validateMetadata({ 'x-bin': ['not base64!'] })).desc).toContain('not base64')
  })

  it('validateMetadataPair is the single-key form', () => {
    expect(() => validateMetadataPair('x-a', ['ok'])).not.toThrow()
    bad(() => validateMetadataPair('X-A', ['ok']))
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

  it('a v1.1 field arriving with the wrong wire type is skipped, not fatal', () => {
    // window (15) as a length-delimited value, compressor (16) as a varint.
    const joined = new Uint8Array([...encodeFrame(frame({ epoch: 1 })), 0x7a, 0x01, 0x09, 0x80, 0x01, 0x07])
    const g = decodeFrame(joined)
    expect(g.epoch).toBe(1)
    expect(g.window).toBe(0)
    expect(g.compressor).toBe('')
  })

  it('unknown fields inside an Any are skipped', () => {
    // Any{type_url:"x", 3:varint 1, value:[00]} — field 3 is not ours.
    const g = decodeFrame(unhex('8a01080a01781801120100'))
    expect(g.details).toEqual([{ typeUrl: 'x', value: new Uint8Array([0x00]) }])
  })

  it('truncated input throws', () => {
    const bytes = encodeFrame(goldenFrame())
    expect(() => decodeFrame(bytes.subarray(0, bytes.length - 3))).toThrow()
  })

  it('an empty envelop decodes to no frames', () => {
    expect(decodeEnvelop(new Uint8Array(0))).toEqual([])
  })
})
