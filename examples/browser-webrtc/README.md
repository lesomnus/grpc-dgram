# browser-webrtc

The project's final goal, runnable: **a browser page calling a Go gRPC service
over a WebRTC DataChannel.** No HTTP/2, no proxy, no gRPC-Web. The page speaks
the drpc wire protocol directly — using the TypeScript port in
[`ts/`](../../ts) — and the Go side answers with generated gRPC stubs on
[`transport/pion`](../../transport/pion). The only HTTP request in the whole
flow is the SDP exchange that sets the channel up.

## Run it

```sh
# 1. build the TypeScript port (the page imports its dist/ output)
cd ts && pnpm install && pnpm build

# 2. serve the page and the DataChannel gateway
cd ../examples/browser-webrtc && go run .

# 3. open http://127.0.0.1:8080 and type something
```

Type a message, press **Echo**: the page runs `/webecho.EchoService/Echo` over
the data channel and prints the uppercased reply plus which process served it.
Send an empty message to see a gRPC status (`INVALID_ARGUMENT`) come back over
the same channel.

`pnpm build` is required because `@lesomnus/grpc-dgram` is not published: the
page imports the build output, mounted by this server at `/ts/dist/` and named
by the import map in `web/index.html`.

## What it demonstrates

- **One wire, two implementations.** The browser runs the TS port
  (`Conn` + `DataChannelTransport`); the server runs the Go core
  (`drpc.Server` + `pion.Gateway`). Both implement drpc v1.0
  (`PROTOCOL.md`), so the call is just a call.
- **Reliable mode, derived from the channel.** `pc.createDataChannel('rpc')`
  is ordered with no retransmit or lifetime cap, so both adapters report
  *reliable* and both cores run with every protocol timer off (§10.6). Pass
  `{ ordered: false, maxRetransmits: 0 }` instead and the same code runs in
  unreliable mode — the sensor path, in a browser.
- **The gateway wiring that matters** (`sdp.go`): `gw.Bind(dc)` runs
  *synchronously* inside `OnDataChannel` (pion holds the channel's read loop
  until the callback returns, and messages arriving with no handler are lost),
  and `gw.ServePeer` runs on its own goroutine because it blocks until the
  channel dies — then performs the §4.5 teardown, `srv.DisconnectPeer`.
- **Watching the peer connection too.** A severed peer may never surface
  `OnClose` on the channel, so both ends also react to connection-state
  changes; the page closes its `Conn`, which fails any live call instead of
  leaving it hanging.
- **A page with no build step.** `web/main.js` is plain ES modules with
  `// @ts-check` and JSDoc types — no bundler, no framework. Because it has no
  protobuf runtime, it names the `"json"` codec on its OPEN frame (§12) and the
  server marshals with `protojson` (`jsoncodec.go`), so the Go handlers keep
  their generated stubs and never learn about it. With a bundler you would drop
  `jsoncodec.go`, import `@lesomnus/grpc-dgram/transport/protobuf-es` with
  `protoc-gen-es` output, and let the default protobuf codec carry the call.

## Type-checking the page

The page is JavaScript, so there is nothing to compile — but it is checked
against the port's own `.d.mts` types:

```sh
cd ts && pnpm exec tsc -p ../examples/browser-webrtc
```

`tsconfig.json`'s two `paths` entries stand in for the import map, pointing the
package name at `ts/dist`.

## Files

| | |
|---|---|
| `main.go` | flags, the HTTP server (page, `/ts/dist/`, `POST /offer`), the drpc server |
| `sdp.go` | the one-shot SDP exchange and the pion gateway wiring |
| `service.go` | the `EchoService` handler — ordinary gRPC code |
| `jsoncodec.go` | the `"json"` wire codec, so the page needs no protobuf runtime |
| `web/index.html` | the page, the import map, and the styling |
| `web/main.js` | the browser client: descriptor, codec, signaling, calls |
| `proto/webecho.proto`, `echopb/` | the service and its checked-in bindings |

Regenerate the bindings (needs [buf](https://buf.build); the generated files
are committed, so running the example does not):

```sh
buf generate examples/browser-webrtc/proto --template examples/browser-webrtc/buf.gen.yaml   # from the repo root
```

## Flags

| Flag | Default | |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | HTTP address for the page and the SDP exchange |
| `-web` | `web` | directory holding `index.html` and `main.js` |
| `-ts-dist` | `../../ts/dist` | the built TypeScript port, mounted at `/ts/dist/` |

## Notes

- Localhost only, deliberately: no STUN/TURN, no trickle ICE — the answer is
  sent once ICE gathering completes. Across a real network you would add ICE
  servers and a signaling channel of your own; drpc does not care how the
  channel is negotiated.
- WebRTC data channels are DTLS-encrypted, which is the deployment the protocol
  assumes (`PROTOCOL.md` §15). There is still no *authentication* of frames
  beyond the channel — put that in your signaling.
- Serving the port's `dist/` over HTTP is what an unpublished package costs.
  Once it is published the import map disappears and the page imports
  `@lesomnus/grpc-dgram` from a CDN or a bundle like any other dependency.
