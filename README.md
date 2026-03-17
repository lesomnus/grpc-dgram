# grpc-dgram

*grpc-dgram* is a message-oriented transport layer that reuses code generated for [gRPC](https://grpc.io/) while running over an unreliable datagram-style channel such as UDP or [WebRTC data channels](https://developer.mozilla.org/en-US/docs/Web/API/WebRTC_API/Using_data_channels).

Strictly speaking, this project is not gRPC.

gRPC is defined on top of HTTP/2, and it also carries a set of behavioral conventions around request/response handling, streaming, metadata, deadlines, and status propagation.
This project intentionally does not preserve that wire-level or runtime contract as is.
Instead, it adapts the programming model to a datagram environment where loss, reordering, duplication, and partial delivery need to be considered first.

The name *grpc-dgram* is practical: the project is designed so that code generated from `.proto` files for gRPC can be reused with minimal friction.
In other words, the goal is not "gRPC over datagram" in the protocol-spec sense, but "a datagram transport that can work with gRPC-generated stubs and service definitions":

- you can keep using code generated for gRPC.
- you can keep implementing handlers in the familiar gRPC service shape.
- you can swap the underlying transport model to a datagram-oriented one.

## Features

- [x] Unary Call
- [ ] Server-side Streaming Call
- [ ] Client-side Streaming Call
- [ ] Bidirectional Streaming Call
- [ ] Metadata
- [ ] Trailer
- [ ] Codecs
- [ ] Interceptors
- [ ] Stats Handler
- [ ] Adaptor for [net#PacketConn](https://pkg.go.dev/net#PacketConn)
- [ ] Adaptor for [pion/webrtc](https://github.com/pion/webrtc)
- [ ] Adaptor for [gorilla/websocket](https://github.com/gorilla/websocket)
- [ ] Browser JS port

## Usage

TBD

## What This Project Is

- A transport/runtime for message-based communication.
- A transport/runtime designed for datagram-style channels, where message loss, duplication, and reordering are possible.
- A compatibility layer around gRPC-generated interfaces such as `grpc.ClientConnInterface`, `grpc.ServiceRegistrar`, and generated service/client code (and possibly for other languages).

## What This Project Is Not

- It is not HTTP/2-based gRPC.
- It does not aim to be wire-compatible with standard gRPC implementations.
- It does not fully preserve the service processing rules, transport semantics, or feature set expected by the official gRPC stack.

If you need interoperability with existing gRPC servers, proxies, observability tooling, or the HTTP/2 ecosystem, this project is the wrong transport.

## Non-Goals

- Full gRPC protocol compliance
- HTTP/2 transport compatibility
- Transparent interoperability with standard gRPC implementations
- Exact reproduction of all gRPC metadata, streaming, and lifecycle semantics
