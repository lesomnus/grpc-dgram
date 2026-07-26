# `@lesomnus/grpc-dgram/transport/protobuf-es`

Derive a dRPC method descriptor straight from a **protobuf-es** service, so RPC
types are never re-declared by hand — the generated `*_pb.ts` (from
`protoc-gen-es`) is the single source of truth for the method path, the
streaming kind, and the payload codec. This is the TS analog of the Go core
plugging grpc-go's generated `ServiceDesc` straight in (G2).

## Peer dependency

`@bufbuild/protobuf` (v2) is an **optional peer dependency** — the core stays
dependency-free; only this entry pulls it in.

```sh
npm i @bufbuild/protobuf
```

## Usage

```ts
import { create } from '@bufbuild/protobuf'
import { fromService } from '@lesomnus/grpc-dgram/transport/protobuf-es'
import { EchoService, EchoRequestSchema, EchoResponseSchema } from './echo_pb' // protoc-gen-es output

const Echo = fromService(EchoService) // { once, many, count, live, ... }, fully typed

// client
await conn.invoke(Echo.once, create(EchoRequestSchema, { message: 'hi' }))

// server — handler req/res types are derived from the descriptor
server.register(Echo.once, (req) => create(EchoResponseSchema, { message: req.message }))
```

`fromMethod(service.method.once)` derives a single descriptor; `fromService`
derives them all, keyed by the generated localName.

## What it derives

| dRPC `MethodDesc` field | from the protobuf-es descriptor |
|---|---|
| `path` | `/<service typeName>/<proto method name>` (the **proto** name, so a TS client and a Go server address the same method, §13) |
| `clientStreams` / `serverStreams` | `methodKind` (`unary` / `server_streaming` / `client_streaming` / `bidi_streaming`) |
| `request` / `response` codec | `toBinary` / `fromBinary` over `method.input` / `method.output` |

The wire codec name stays `''` (proto, §12): protobuf-es marshals the same bytes
the Go proto codec does, so the two implementations interoperate. Verified
against real `protoc-gen-es` output and against a live Go server (see
`test/protobufes-gen.test.ts`, `test/conformance.test.ts`).
