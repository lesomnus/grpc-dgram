# gRPC compatibility

`grpc-dgram` exists so that `.proto` files, `protoc-gen-go-grpc` output, and
handler code keep working when the transport underneath is a datagram channel
instead of HTTP/2. `drpc.Conn` implements `grpc.ClientConnInterface` and
`drpc.Server` implements `grpc.ServiceRegistrar`, so generated code plugs in
with no edits at all. That is goal G2 in [PROTOCOL.md](./PROTOCOL.md) §1, and
this document is the honest accounting of how far it goes: every divergence
below says what a ported program will actually observe. The normative rules
live in [PROTOCOL.md](./PROTOCOL.md); the code is `callinfo.go`, `compat.go`
and `metadata.go`, pinned by `compat_test.go`.

## At a glance

| Surface | Status |
|---|---|
| Unary / server- / client- / bidi-streaming, generated stubs | works unchanged |
| Header and trailer metadata, on success and on error | works unchanged |
| Binary `-bin` metadata (arbitrary octets) | works unchanged |
| Status codes, `status.WithDetails` | works; details are shed if the terminal would not fit |
| Client and server interceptors, chained | works; `cc *grpc.ClientConn` is nil |
| Codecs (`ForceCodecV2`, `CallContentSubtype`), compressors | works; both ends must register the name |
| Per-call size caps, `OnFinish`, `Peer`, `PerRPCCredentials` | works |
| `stats.Handler` | works |
| Deadlines, cancellation | works; a deadline-less unary is capped at `T_call` |
| `GetServiceInfo`, `reflection.Register` | compiles and answers; standard tooling still cannot reach it |
| `WaitForReady`, transparent retry, `StaticMethod` | inert |
| Name resolution, load balancing, HTTP/2 interop | absent |

## The four RPC types and generated stubs

All four shapes run, with the generated client and server interfaces
untouched. `RegisterService` and `NewStream`/`Invoke` are the only seams, and
they are the ones code generation already targets:

```go
gw := udp.NewGateway(pc)
srv := drpc.NewServer(gw)
sensorpb.RegisterSensorServiceServer(srv, &sensorImpl{})
go gw.Serve(ctx, srv)

conn := drpc.NewConn(udp.New(c))          // a grpc.ClientConnInterface
client := sensorpb.NewSensorServiceClient(conn)
stream, err := client.Readings(ctx, &sensorpb.Subscribe{Hz: 50})
r, err := stream.Recv()                   // io.EOF, or a gRPC status
```

`examples/udp-sensor` is this program, complete and runnable.

The one semantic that changes is *delivery*, not API. In unreliable mode a
stream hands the application an ordered **subsequence** of what was sent —
never reordered, never duplicated, gaps allowed — and unary is executed at
most once per server incarnation. Over a reliable transport the sequence is
exact and a gap or duplicate fails the call with `INTERNAL`. PROTOCOL.md §14
tabulates this per RPC type; the Guarantees section of
[README.md](../README.md) is the prose version. Nothing about
`Send`/`Recv`/`CloseSend`/`CloseAndRecv` changes.

An OPEN naming a method that does not resolve draws `UNIMPLEMENTED`
(PROTOCOL.md §13), the same code grpc-go returns for an unknown method, and
the answer is tombstoned so a retransmitted OPEN gets the same reply rather
than a second lookup.

## Metadata

Request metadata rides the OPEN frame only; response header metadata rides
the first server frame sent after the handler sets it, and the trailer rides
the terminal (PROTOCOL.md §11). On the API surface nothing moves:

```go
md, _ := metadata.FromIncomingContext(ctx)
_ = md.Get("trace-bin")                                     // raw octets
_ = grpc.SetHeader(ctx, metadata.Pairs("x-served-by", "eu-1"))
_ = grpc.SetTrailer(ctx, metadata.Pairs("x-cost", "3"))
```

`grpc.SendHeader`, `grpc.SetHeader`, `grpc.SetTrailer` and the `ServerStream`
methods of the same name all work, because the handler ctx carries a
`grpc.ServerTransportStream`. `SendHeader` flushes an `H` frame immediately —
on unary calls too — so a client blocked in `Header()` is released before the
response exists, which is what grpc-go's separate HEADERS frame gives for
free. Flushing twice returns `Internal`, grpc-go's `ErrIllegalHeaderWrite`; so
does a `SetHeader` after the header is already on the wire.

`SetTrailer` has no error to return, so a trailer that fails validation is
**dropped** and counted as an off-shape protocol event; letting it through
would fail the terminal frame's marshal, and losing the terminal is what
strands a call. grpc-go logs the violation and sends the trailer anyway, so a
ported handler that set a non-printable trailer value will find that value
missing at the client rather than mangled.

### Binary keys carry arbitrary octets

Wire v1.1 made metadata values `bytes` precisely so gRPC's binary metadata
survives: a proto `string` cannot hold a NUL or an invalid UTF-8 sequence, and
base64-ing everything would have changed what the peer receives. A `-bin` key
therefore travels verbatim.

```go
md := metadata.MD{
    "x-tenant":  []string{"acme"},
    "trace-bin": []string{string([]byte{0x00, 0xff, 0x80})}, // raw octets
}

var header, trailer metadata.MD
_, err := client.Echo(metadata.NewOutgoingContext(ctx, md),
    &echopb.EchoRequest{Message: "hi"},
    grpc.Header(&header), grpc.Trailer(&trailer))
```

**Go vs TypeScript.** grpc-go keeps the octets in the `string` of a
`metadata.MD` value, so `metadata.go`'s conversion is a plain re-typing in
both directions — no base64, no change of semantics. A JS string cannot hold
arbitrary octets, so the TypeScript port draws the boundary the grpc-web way:
a `-bin` value is the **base64** of the octets in TS and the octets themselves
on the wire, and every other value is UTF-8 encoded. The wire bytes are
identical; only the local representation differs. A Go program sending
`"\x00\xff\x80"` under `trace-bin` is read by a TS peer as `"AP+A"`.

### Validation mirrors grpc-go

A key must be non-empty and drawn from `[0-9 a-z _ - .]`; the values of a
non-`-bin` key must be printable ASCII (`%x20-%x7E`); `-bin` values are
unvalidated. A violation fails the call **locally**, before anything reaches
the wire, with `Internal` naming the key. The reason is diagnostic: without
the gate the same bug surfaces as a proto marshal failure deep inside an
adapter — a bare `UNKNOWN` naming no key at all. Credential-produced metadata
goes through the same gate (§11, §15). Upper-case keys are *not* rejected:
grpc-go lower-cases outgoing keys in `FromOutgoingContext` before dRPC ever
sees them, so rejecting the mixed-case form would fail calls grpc-go accepts.

Receiving is deliberately lenient. A hostile peer must not be able to kill a
call by sending metadata the local binding cannot represent, so a receiver
surfaces whatever its language allows (raw octets in Go, a replacement-
charactered string in JS) and never fails the frame for it.

### Header timing

There is no separate HEADERS frame here — the header rides whatever frame goes
out next — and the terminal re-carries it once the handler has set one. That
is insurance: in unreliable mode the frame that first carried it can be lost,
and a client blocked in `Header()` would otherwise wait out the whole call for
nothing. First-wins applies, so a later carrier never rewrites a latched
header. PROTOCOL.md §11 records the divergence this creates: `Header()` can
return metadata where gRPC's trailers-only response would yield `nil`.

`Header()` itself never returns the call's status and never a context error. A
cancelled caller gets `(nil, nil)`, and the status arrives from `RecvMsg` — as
in grpc-go, and deliberately not racy: cancellation ends the call through the
abort path, which releases both of the channels `Header()` waits on.

## Status codes and details

Handler errors become `google.rpc.Status` on the terminal frame: code,
message, and the `Any` details `status.WithDetails` attached. A non-status
error becomes `UNKNOWN` with its `Error()` text, and `context.Canceled` /
`context.DeadlineExceeded` map to `CANCELED` / `DEADLINE_EXCEEDED` — grpc-go's
mapping.

```go
// handler
st := status.New(codes.FailedPrecondition, "sensor is warming up")
st, err := st.WithDetails(&sensorpb.Reading{Seq: 7})
return st.Err()

// caller
for _, d := range status.Convert(err).Details() {
    if r, ok := d.(*sensorpb.Reading); ok {
        _ = r.GetSeq()
    }
}
```

**Details are a passenger.** Every termination bound in PROTOCOL.md §10.7
depends on the terminal frame arriving, and an unreliable adapter refuses a
frame that exceeds its datagram (§4.4). So when the terminal does not fit,
`transmitTerminal` sheds in order of expendability — first the details, then
the response payload, and a still-oversize terminal degrades to a bare
`RESOURCE_EXHAUSTED`. What a ported program observes is a status whose code
and message are intact and whose `Details()` is empty. Keep details small, or
put them on a reliable transport, where any size travels.

## Per-call options

`resolveCallOptions` in `callinfo.go` folds `[]grpc.CallOption` into the
per-call configuration, later options winning — grpc-go's "applied in order"
contract. Endpoint-wide defaults come from `WithDefaultCallOptions`, and a
per-call option overrides them.

| Option | Behavior here |
|---|---|
| `grpc.ForceCodecV2` | honored; the lower-cased `Name()` goes on the OPEN. A nil codec is `Internal` |
| `grpc.CallContentSubtype` | honored; looks the name up in the process `encoding` registry, as grpc-go does |
| `grpc.UseCompressor` | honored; names the call's compressor on the OPEN (§12.1) |
| `grpc.MaxCallRecvMsgSize` | honored; default 4 MiB, gRPC's own. Measured on the **decompressed** message |
| `grpc.MaxCallSendMsgSize` | honored; default `math.MaxInt32`. Measured on the **compressed** bytes |
| `grpc.OnFinish` | honored; fires exactly once with the call's final error |
| `grpc.Peer` | honored; populated before the caller sees the result |
| `grpc.PerRPCCredentials` | honored; accumulates with the dial-level ones, never replaces them |
| `grpc.Header` / `grpc.Trailer` | honored on **unary only**; ignored on streaming calls, where grpc-go populates them at finish |
| `grpc.WaitForReady` / `grpc.FailFast` | **inert** |
| `grpc.MaxRetryRPCBufferSize` / `grpc.StaticMethod` | **inert** |
| `grpc.CallAuthority` | **inert** |
| `grpc.ForceCodec` (v1) / `grpc.CallCustomCodec` | **inert** |

A size limit of `0` rejects everything rather than meaning "unlimited", the
same reading grpc-go takes: turning a deliberate lockdown into an open door is
not a default anyone wants to discover in production.

`OnFinish` and `grpc.Peer(&p)` are both applied by `reportFinish` *before* the
call's `done` channel closes, which is what makes reading `p` safe on return.
The drpc-specific caveat: the callback runs on whichever goroutine ends the
call — usually the adapter's receive loop — so **it must not block**.
Everything that endpoint receives, for every call, waits behind it.

`grpc.Header(&md)` and `grpc.Trailer(&md)` are applied by `Conn.Invoke` on
finish regardless of the status, but `Conn.NewStream` does not apply them at
all, where grpc-go runs the same hooks when a streaming call finishes. A port
that passed them to a streaming stub gets an untouched `metadata.MD`; call
`stream.Header()` and `stream.Trailer()` instead.

### The inert ones, and what a port observes

`WaitForReady` selects behavior that has no referent: there is no connectivity
state machine to wait on, and a datagram `Conn` is always "ready" (PROTOCOL.md
§16). A program that used `WaitForReady(true)` to ride out a backend restart
now sees each call attempted at once and failing on its own deadline —
`DEADLINE_EXCEEDED`, or `UNAVAILABLE` if the transport died. Retrying is the
caller's job, and a `stats.Handler` always sees `FailFast: true` in
`RPCTagInfo` for the same reason. `MaxRetryRPCBufferSize` sizes the buffer for
transparent retry, also a non-goal (§1); `StaticMethod` is a grpc-go-internal
cardinality hint to stats plugins, with no wire or dispatch effect anywhere.

`grpc.CallAuthority` is ignored because there is no `:authority` header to
override, and the endpoint-wide `WithAuthority` is not a substitute — it only
builds the audience string handed to `PerRPCCredentials`. A program that
varied the authority per call to get per-call audiences finds every call using
the endpoint's.

`grpc.ForceCodec` and `grpc.CallCustomCodec` take the v1 `encoding.Codec`
interface, which `CodecV2` supersedes. **This one is silent**: the option is
dropped and the call falls back to proto, so a program whose messages are
proto messages keeps working while quietly ignoring the codec it asked for.
Port these to `grpc.ForceCodecV2` or `grpc.CallContentSubtype`. Nothing else
is ignored: no other exported grpc-go call option changes call semantics.

## Codecs and compression

The codec is named on the OPEN and governs the whole call in both directions;
`""` means proto (PROTOCOL.md §12). An unknown codec at the server draws
`UNIMPLEMENTED`, tombstone-stored, and an unregistered name on the client
fails the call locally before any frame is built. Both ends must therefore
have the codec registered — the usual blank import.

Compression is the same shape: `grpc.UseCompressor("gzip")` names a compressor
on the OPEN, it governs both directions, and an unknown name at the server is
`UNIMPLEMENTED` (§12.1). Three properties differ from HTTP/2 gRPC:

- **Per message, stateless.** A shared stream dictionary is forbidden: in
  unreliable mode one lost message would make every later one undecodable.
- **Per frame, opportunistic.** A payload that would grow — already
  compressed, tiny, or empty — is sent raw, without the `COMPRESSED` flag.
  A 0-byte message is meaningful, so it is never compressed.
- **Bounded on receive.** Decompression reads one byte past the receive cap
  and then fails `RESOURCE_EXHAUSTED`, so a compression bomb costs nothing.

There is no endpoint-wide compressor option; apply one to every call with
`drpc.WithDefaultCallOptions(grpc.UseCompressor("gzip"))`.

Message *size* is a separate axis from the caps above. The per-call caps
measure one message; the adapter measures the whole marshaled `Envelop` and
owns the ceiling (§4.4). `transport/udp` refuses anything over 1200 bytes by
default and the owning call fails `ResourceExhausted`; a reliable transport
carries any size. The core never fragments.

## Interceptors

Unary and stream, client and server, single and chained — all present, with
grpc-go's signatures and ordering:

```go
srv := drpc.NewServer(tx, drpc.ChainUnaryInterceptors(
    func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
        p, _ := peer.FromContext(ctx)
        log.Printf("%s from %s", info.FullMethod, p.Addr)
        return h(ctx, req)
    },
))
```

The client-side names are the `ConnOption` spellings of the dial options:
`WithUnaryInterceptor`, `WithChainUnaryInterceptor`, `WithStreamInterceptor`,
`WithChainStreamInterceptor`.

**The divergence to know:** the `cc *grpc.ClientConn` parameter is always
`nil`. A `drpc.Conn` is a `ClientConnInterface`, not a `*grpc.ClientConn`, and
there is no honest value to pass. A ported interceptor that calls
`cc.Target()`, `cc.GetState()` or similar panics on the first call. Read what
you need off the ctx instead — `peer.FromContext` works on both ends, and
`metadata.FromOutgoingContext` gives the client-side header.

The eager OPEN of a client-streaming call is emitted by the innermost
streamer, after the interceptor chain has run, so metadata an interceptor adds
still reaches the OPEN frame (§8, Appendix C).

## Peer addressing

`grpc.Peer(&p)` on a call and `peer.FromContext` in a handler or client
interceptor both work, because the *transport* names the remote end — never
the frame contents (PROTOCOL.md §6.4). A client transport does it by
implementing `drpc.TransportPeer`; a gateway does it per frame, attaching a
`*peer.Peer` to the receive ctx. All four shipped adapters do one or the
other, so over UDP `p.Addr` and `p.LocalAddr` are real socket addresses on
both sides. A channel with no address of its own says so instead:
`transport/jsport` reports the port's label on network `"js"`.

Two things a port should not assume. **`p.AuthInfo` is always nil**: dRPC
terminates no TLS and inspects no channel, so code that read it to recover a
client certificate must get that from the layer that did the handshake. And
**`p.Addr` need not be an IP** — `transport/pion` names a DataChannel by its
label (`Network()` is `"webrtc"`, `String()` is `datachannel:<label>`,
`LocalAddr` is nil), and an adapter whose peer key is opaque gets a `net.Addr`
whose `Network()` is `"drpc"`. What is guaranteed is that `peer.FromContext`
never hands a handler a nil peer.

## Per-RPC credentials

`PerRPCCredentials` are a metadata producer and nothing more — dRPC
authenticates nothing itself (§15) — and that has two consequences.

```go
conn := drpc.NewConn(tx,
    drpc.WithPerRPCCredentials(creds),
    drpc.WithAuthority("sensors.example.com"),
    drpc.WithAssumeTransportSecurity(), // the channel is DTLS/WSS/WebRTC
)
```

Credentials that report `RequireTransportSecurity()` are **refused** with
`Unauthenticated` before the call exists, because dRPC cannot attest a channel
it does not own; `WithAssumeTransportSecurity()` is the explicit override for
when the channel really is DTLS/WSS/WebRTC. And the audience handed to the
provider is grpc-go's `createAudience` string,
`https://<authority>/<service>`, built from `WithAuthority` — scheme included,
because providers mint the JWT `aud` claim from it and a `drpc://` scheme
would produce tokens no audience-checking server accepts. Without
`WithAuthority` the audience degrades to `https:///pkg.Service`, and unlike
grpc-go dRPC does not strip a trailing `:443`, so pass the bare host when the
audience has to match.

A provider that returns a status error in one of gRFC A54's control-plane-
restricted codes (`InvalidArgument`, `NotFound`, `AlreadyExists`,
`FailedPrecondition`, `Aborted`, `OutOfRange`, `DataLoss`) has it rewritten to
`Internal`, as grpc-go does: a credential provider must not be able to make a
call look like an application-level failure.

## The service registry and reflection

`Server.GetServiceInfo` returns the same map `grpc.Server` exposes — every
registered method under its service, with the streaming flags — so
`reflection.Register` accepts a `*drpc.Server` unchanged:

```go
reflection.Register(srv) // *drpc.Server satisfies reflection.GRPCServer
```

Be clear about what that buys. The reflection service is an ordinary bidi RPC,
so it answers a peer that speaks the **dRPC wire**; `grpcurl`, `grpc_cli` and
every other tool in the HTTP/2 ecosystem cannot reach it, because there is no
HTTP/2 to reach it over. And its responses carry `FileDescriptorProto` blobs
that routinely exceed a 1200-byte datagram, so over `transport/udp` such a
call fails `ResourceExhausted` at send. Reflection is practical over a
reliable adapter, not over the sensor path.

`RegisterService` after the server has started serving panics: the registry is
immutable once `Handle` runs (§13), because a method table that can change
under a dispatch is a correctness hazard no protocol bound can cover.

## Deadlines and cancellation

The client's remaining budget travels on the OPEN as `Frame.timeout`, and both
ends enforce it independently — the server never waits for a frame to notice
expiry (PROTOCOL.md §10.2). Handler code is ordinary gRPC code: watch
`stream.Context().Done()` and return
`status.FromContextError(ctx.Err()).Err()`.

Cancelling the caller's ctx ends the call locally at once and sends a best-
effort terminal CLOSE, so the handler's ctx is cancelled too. As in gRPC,
handler side effects may complete even after the client observed
`DEADLINE_EXCEEDED`: expiry does not undo server work.

Two divergences. **A deadline-less unary call is capped**: in unreliable mode
the client injects `now + T_call` (5 s by default) when the caller's ctx has
no deadline, because otherwise a lost terminal would hang the call forever and
G1 promises bounded termination. gRPC would wait indefinitely. Set an explicit
deadline or raise `Timing.Call`; reliable mode injects nothing, since there
the transport's own death detection fails a live call. **Streaming calls get
no default deadline** — long-lived streams are the point — and are bounded
instead by completion, cancellation, RESET, and peer liveness (`T_live`, 15 s
by default).

A server that does not trust client-asserted timeouts clamps them with
`drpc.WithMaxHandlerTimeout(d)`, off by default for gRPC equivalence.

## Observability

`drpc.WithStatsHandler` takes a `google.golang.org/grpc/stats.Handler` on
either role, so existing gRPC instrumentation observes dRPC calls unchanged:
`Begin`, `OutHeader`, `OutPayload`, `InPayload`, `InTrailer`, `End` on the
client and `Begin`, `InHeader`, `InPayload`, `OutPayload`, `OutTrailer`, `End`
on the server, with `Client` and `End.Error` set as grpc-go sets them and
`End` last. `Length` is the message and `WireLength` the bytes that went out,
so they differ exactly when the call is compressed. The datagram-specific
events gRPC has no concept of — gaps, rx drops, RESETs, retransmissions,
probes, liveness expiry, tombstone replays, stream and peer flow stalls — live
on a second surface, `WithProtocolStats` (§14), with `drpc.Counters` as a
ready-made sink.

## Not here, and why

- **No transparent retry, no hedging, no `WaitForReady`.** All three need a
  connectivity model — channel states, backoff, subchannel readiness — that a
  datagram channel does not have, and retrying a 20 ms-old sensor reading only
  delays the current one (§1). What replaces them is bounded termination: a
  call fails with a status inside a stated ceiling, and the caller decides.
- **No name resolution, load balancing, service config, or `*grpc.ClientConn`
  API.** There is no `grpc.Dial` and no target string: `drpc.NewConn` takes a
  transport you already opened, and `GetState`/`WaitForStateChange`/`Connect`
  belong to that same absent model. Picking a backend and watching DNS live
  above this library.
- **No HTTP/2 wire compatibility.** The framing is dRPC's own (§5). No
  existing proxy, sidecar, gateway or CLI speaks it. If interop with gRPC
  infrastructure matters, this is the wrong transport — that is the trade the
  whole project is built on.
- **No authentication on the wire** (§15). `epoch` is a correctness device,
  not a security token; deploy over DTLS, WSS or WebRTC.

One capacity difference is easy to miss. gRPC's `MaxConcurrentStreams` makes a
client *queue* new streams; dRPC's `Limits.MaxLiveCalls` (4096 per transport
peer, §15) makes the server **reject** the OPEN with `RESOURCE_EXHAUSTED`,
tombstoned so a retransmit gets the same answer. A port that relied on
queueing to absorb a burst sees failed calls instead.

The other capacity knob has HTTP/2's shape and one deliberate difference.
`Limits.MaxPeerWindow` (§4.2.1, reliable mode) is a connection-level
flow-control window beside the per-stream ones — counted in messages, 1024 by
default, one per transport peer — and a sender assumes that much toward every
peer, as an HTTP/2 sender assumes 65535 bytes. Where HTTP/2 answers an
overrun with `FLOW_CONTROL_ERROR` and `GOAWAY` for the whole connection, dRPC
fails **only the overrunning call** with `INTERNAL`: the transport belongs to
the adapter, and tearing it down from inside the read loop would turn one
accounting slip into an outage for every call on it.

## Porting checklist

1. Replace `grpc.NewClient`/`grpc.Dial` with a transport plus `drpc.NewConn`,
   and `grpc.NewServer` with `drpc.NewServer` plus the adapter's `Serve`.
2. Grep for `WaitForReady`, `ForceCodec(`, `CallCustomCodec`, `CallAuthority`
   and service-config retry policies: inert here.
3. Grep interceptors for uses of the `cc *grpc.ClientConn` parameter: nil here.
4. Give every unary call an explicit deadline, and check that streaming
   handlers exit on `ctx.Done()`.
5. Decide per stream whether an ordered subsequence is acceptable (§14). If
   not, run it over a reliable adapter — the mode is auto-detected, no code
   change.
6. Keep messages inside the adapter's ceiling, and `status.WithDetails`
   payloads small.
