// Metadata is the gRPC-style multi-value string map (PROTOCOL.md §11).
// Plain object form so it is trivially constructable and structurally typed;
// keys are used as-is (no case normalization — normalize at the application
// boundary if needed, as with grpc-go's metadata.MD).
//
// Values travel as raw **bytes** on the wire (`Metadata.Entry.values` is
// `repeated bytes`, wire v1.1): gRPC's binary metadata carries arbitrary
// octets, which a proto `string` cannot hold. Go keeps those octets in the
// string of a metadata.MD value, so its conversion is a plain re-typing; a JS
// string cannot hold arbitrary octets, so this port draws the boundary the
// grpc-web way instead:
//
//   - a key ending in "-bin" is BINARY: its TS value is the **base64** of the
//     octets, and the octets themselves go on the wire;
//   - every other key is TEXT: its TS value is UTF-8 encoded onto the wire.
//
// Both sides therefore put identical bytes on the wire for identical
// metadata; only the local representation differs.

import { Code, StatusError } from './status'

export type Metadata = Record<string, string[]>

// isBinaryKey reports whether key uses gRPC's binary ("-bin") convention.
export const isBinaryKey = (key: string): boolean => key.endsWith('-bin')

// metadataJoin merges b into a copy of a, appending values per key — the
// equivalent of grpc-go's metadata.Join. Either side may be undefined.
export function metadataJoin(a: Metadata | undefined, b: Metadata | undefined): Metadata | undefined {
  if (a === undefined) return b === undefined ? undefined : cloneMetadata(b)
  const out = cloneMetadata(a)
  if (b !== undefined) {
    for (const [k, vs] of Object.entries(b)) {
      const cur = out[k]
      out[k] = cur === undefined ? [...vs] : [...cur, ...vs]
    }
  }
  return out
}

export function cloneMetadata(md: Metadata): Metadata {
  const out: Metadata = {}
  for (const [k, vs] of Object.entries(md)) out[k] = [...vs]
  return out
}

// ---------------------------------------------------------------------------
// base64 — hand-rolled
// ---------------------------------------------------------------------------
//
// No Buffer (Node-only) and no atob/btoa (they round-trip through Latin-1
// strings, are absent in some worker scopes, and throw DOMExceptions): the
// core must run unchanged in a browser, in Node, and in a worker.

const B64_CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'

// B64_VALUES maps a char code to its 6-bit value; -1 = not in the alphabet.
const B64_VALUES = (() => {
  const t = new Int8Array(128).fill(-1)
  for (let i = 0; i < B64_CHARS.length; i++) t[B64_CHARS.charCodeAt(i)] = i
  return t
})()

// encodeBase64 renders bytes as canonical, padded, standard-alphabet base64.
export function encodeBase64(bytes: Uint8Array): string {
  const out: string[] = []
  let i = 0
  for (; i + 2 < bytes.length; i += 3) {
    const n = (bytes[i]! << 16) | (bytes[i + 1]! << 8) | bytes[i + 2]!
    out.push(B64_CHARS[(n >>> 18) & 63]! + B64_CHARS[(n >>> 12) & 63]! + B64_CHARS[(n >>> 6) & 63]! + B64_CHARS[n & 63]!)
  }
  const rem = bytes.length - i
  if (rem === 1) {
    const n = bytes[i]! << 16
    out.push(`${B64_CHARS[(n >>> 18) & 63]!}${B64_CHARS[(n >>> 12) & 63]!}==`)
  } else if (rem === 2) {
    const n = (bytes[i]! << 16) | (bytes[i + 1]! << 8)
    out.push(`${B64_CHARS[(n >>> 18) & 63]!}${B64_CHARS[(n >>> 12) & 63]!}${B64_CHARS[(n >>> 6) & 63]!}=`)
  }
  return out.join('')
}

// decodeBase64 parses standard-alphabet base64, with or without padding.
// It throws on anything else — including base64url and embedded whitespace —
// so a mistyped binary value is reported instead of silently truncated.
export function decodeBase64(s: string): Uint8Array {
  let end = s.length
  while (end > 0 && s.charCodeAt(end - 1) === 0x3d /* '=' */) end--
  const pad = s.length - end
  if (pad > 2 || (pad > 0 && s.length % 4 !== 0)) throw new Error('drpc: invalid base64: bad padding')
  const rem = end & 3
  if (rem === 1) throw new Error('drpc: invalid base64: truncated group')

  const full = end >>> 2
  const out = new Uint8Array(full * 3 + (rem === 0 ? 0 : rem - 1))
  let o = 0
  let i = 0
  const six = (p: number): number => {
    const c = s.charCodeAt(p)
    const v = c < 128 ? B64_VALUES[c]! : -1
    if (v < 0) throw new Error(`drpc: invalid base64: illegal character ${JSON.stringify(s[p] ?? '')}`)
    return v
  }
  for (let g = 0; g < full; g++, i += 4) {
    const n = (six(i) << 18) | (six(i + 1) << 12) | (six(i + 2) << 6) | six(i + 3)
    out[o++] = (n >>> 16) & 0xff
    out[o++] = (n >>> 8) & 0xff
    out[o++] = n & 0xff
  }
  if (rem === 2) {
    out[o++] = ((six(i) << 2) | (six(i + 1) >>> 4)) & 0xff
  } else if (rem === 3) {
    const n = (six(i) << 12) | (six(i + 1) << 6) | six(i + 2)
    out[o++] = (n >>> 10) & 0xff
    out[o++] = (n >>> 2) & 0xff
  }
  return out
}

// ---------------------------------------------------------------------------
// the TS API ↔ wire value boundary
// ---------------------------------------------------------------------------

const textEncoder = new TextEncoder()
const textDecoder = new TextDecoder()

// encodeMetadataValue renders one value as the octets that go on the wire:
// base64-decoded for a "-bin" key, UTF-8 for any other. An unparseable binary
// value fails the call locally (INTERNAL) rather than crashing the codec —
// the same outcome validateMetadata reports up front.
export function encodeMetadataValue(key: string, value: string): Uint8Array {
  if (!isBinaryKey(key)) return textEncoder.encode(value)
  try {
    return decodeBase64(value)
  } catch (err) {
    throw new StatusError(Code.INTERNAL, binaryValueMsg(key, err))
  }
}

// decodeMetadataValue is the inverse: wire octets to the TS representation.
// Text values are decoded lossily (a non-UTF-8 octet becomes U+FFFD) — the
// wire never fails a call over metadata a peer chose to send.
export function decodeMetadataValue(key: string, bytes: Uint8Array): string {
  return isBinaryKey(key) ? encodeBase64(bytes) : textDecoder.decode(bytes)
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

// validateMetadata mirrors grpc-go's internal/metadata.Validate (which drpc's
// Go core calls from every metadata entry point), so the same metadata is
// legal on both stacks (PROTOCOL.md §11):
//
//   - a key is non-empty and drawn from [0-9 a-z _ - .];
//   - the values of a "-bin" key are arbitrary octets, unvalidated — except
//     that this port needs them to be base64, since that is how a JS string
//     holds octets at all;
//   - every other value is printable ASCII (%x20-%x7E).
//
// A violation throws StatusError{INTERNAL}, matching grpc-go's mdStatusErr:
// the call fails locally, at the API boundary, instead of surfacing as an
// opaque encode failure deep inside an adapter.
export function validateMetadata(md: Metadata | undefined): void {
  if (md === undefined) return
  for (const [k, vs] of Object.entries(md)) validateMetadataPair(k, vs)
}

export function validateMetadataPair(key: string, values: readonly string[]): void {
  if (key === '') throw new StatusError(Code.INTERNAL, 'there is an empty key in the header')
  for (let i = 0; i < key.length; i++) {
    const c = key.charCodeAt(i)
    const ok = (c >= 0x61 && c <= 0x7a) /* a-z */ || (c >= 0x30 && c <= 0x39) /* 0-9 */ || c === 0x2e /* . */ || c === 0x2d /* - */ || c === 0x5f /* _ */
    if (!ok) throw new StatusError(Code.INTERNAL, `header key ${JSON.stringify(key)} contains illegal characters not in [0-9a-z-_.]`)
  }
  if (isBinaryKey(key)) {
    // Binary metadata: any octets, carried verbatim by the bytes field. Only
    // the local encoding is checked.
    for (const v of values) {
      try {
        decodeBase64(v)
      } catch (err) {
        throw new StatusError(Code.INTERNAL, binaryValueMsg(key, err))
      }
    }
    return
  }
  for (const v of values) {
    for (let i = 0; i < v.length; i++) {
      const c = v.charCodeAt(i)
      if (c < 0x20 || c > 0x7e) {
        throw new StatusError(Code.INTERNAL, `header key ${JSON.stringify(key)} contains value with non-printable ASCII characters`)
      }
    }
  }
}

function binaryValueMsg(key: string, err: unknown): string {
  const why = err instanceof Error ? err.message : String(err)
  return `header key ${JSON.stringify(key)} carries a binary value that is not base64 (${why})`
}
