// Metadata is the gRPC-style multi-value string map (PROTOCOL.md §11).
// Plain object form so it is trivially constructable and structurally typed;
// keys are used as-is (no case normalization — normalize at the application
// boundary if needed, as with grpc-go's metadata.MD).

export type Metadata = Record<string, string[]>

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
