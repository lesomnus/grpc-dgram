# Documentation

gRPC-dgram runs the gRPC programming model over datagram channels. These pages
are the readable path in; [PROTOCOL.md](./PROTOCOL.md) is the normative wire
specification and the final authority when they disagree.

## Start here

- **[Getting started](./getting-started.md)** — install, a working client and
  server over UDP, and what changes when the channel is reliable (nothing in
  your code).
- **[Transports](./transports.md)** — the shipped adapters, how the mode is
  decided, the two message-size ceilings, and how to write your own adapter.

## The two modes

The protocol has one behavioral fork, taken from the channel itself. Read the
side you are on.

- **[Unreliable mode](./unreliable-mode.md)** — the datagram path: what loss
  looks like to your application, the drop policies, and the timer system
  (deadlines, retransmission, tombstones, liveness, probes) that guarantees no
  call hangs.
- **[Reliable mode](./reliable-mode.md)** — WebSocket and reliable
  DataChannel: strict sequencing, and the per-stream flow control that keeps
  one slow consumer from stalling every call on the channel.

## Reference

- **[gRPC compatibility](./grpc-compatibility.md)** — what works unchanged,
  what behaves differently, and what is deliberately absent.
- **[Observability](./observability.md)** — the `stats.Handler` bridge and
  dRPC's own protocol counters, including the skipped-message counter that is
  the only way loss is visible at all.
- **[TypeScript port](./typescript.md)** — the browser/Node implementation of
  the same wire, and how interoperability is proven.
- **[PROTOCOL.md](./PROTOCOL.md)** — the wire format, the state machines, and
  the reasoning behind them. Cited as "§4.2" throughout the other pages.
- **[TODO.md](./TODO.md)** — what is left, and what has to be decided first.

Runnable code lives in [`examples/`](../examples): a UDP sensor stream with the
loss counters printed, a reliable WebSocket echo with graceful shutdown, a
browser↔Go WebRTC demo driving the TypeScript port, and a Go server compiled to
`js/wasm` that the page starts on a message port — reload the browser and the
server restarts.
