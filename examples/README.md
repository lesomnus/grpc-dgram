# examples

Runnable demos of `grpc-dgram`. Each one is its own Go module (so the core
never depends on an example, and an example may pull whatever adapter it
needs), each has a README, and each is small enough to read in one sitting —
they are documentation that compiles.

| | transport | mode | what it shows |
|---|---|---|---|
| [`udp-sensor`](./udp-sensor) | UDP | unreliable | the library's actual purpose: a server-streaming sensor feed, `WithMethodRxBuffer` + `DropOldest`, an explicit deadline, and a report of the gap/drop counters |
| [`websocket-echo`](./websocket-echo) | WebSocket (gorilla) | reliable | timers off, exact sequence, and graceful shutdown draining a live stream |
| [`browser-webrtc`](./browser-webrtc) | WebRTC DataChannel (pion ↔ browser) | reliable | the final goal: a browser page on the TypeScript port calling a Go service over a data channel |
| [`browser-wasm`](./browser-wasm) | JS message port (jsport ↔ browser), and WebSocket | reliable | the server compiled to `js/wasm` and run *in the browser* it serves, in a Worker the package ships, so a reload restarts and rebuilds it; `?server=ws` points the same UI at the server process |

```sh
cd udp-sensor     && go run ./...
cd websocket-echo && go run ./...
cd browser-webrtc && go run .      # after: cd ../../ts && pnpm install && pnpm build
cd browser-wasm   && go run .      # same prerequisite; it builds the wasm server itself
```

The `.proto` files and their generated bindings are committed, so nothing here
needs `buf` or `protoc` to build. Each example's `buf.gen.yaml` documents how
to regenerate them.
