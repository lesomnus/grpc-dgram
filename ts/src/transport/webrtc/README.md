# `@lesomnus/grpc-dgram/transport/webrtc`

dRPC over **WebRTC DataChannels** — the TS twin of the Go `transport/pion`
adapter. One channel message carries one marshaled `Envelop`, and the protocol
mode is **derived from the channel's own configuration**: an ordered channel
with no retransmit or lifetime cap runs reliable (all timers off, §10.6);
anything else is unreliable and the full timer machinery is on. Same adapter,
the mode decided by the channel.

Works in the browser and in any runtime with an `RTCDataChannel`-shaped object
(`DataChannelLike` is structural — node WebRTC implementations and test mocks
fit too). **No npm dependency**; it uses the platform `RTCDataChannel`.

## Client — `DataChannelTransport`

One channel to one server. The `Conn` attaches it and starts the receive pump
itself; `conn.close()` tears everything down, channel included.

```ts
import { Conn } from '@lesomnus/grpc-dgram'
import { DataChannelTransport } from '@lesomnus/grpc-dgram/transport/webrtc'

const dc = pc.createDataChannel('rpc') // ordered, no caps → reliable, no timers
const conn = new Conn(new DataChannelTransport(dc))

await conn.invoke(Once, req)
conn.close()
```

## Server — `DataChannelGateway`

One `Server` serving many peers, one channel each. Channels of differing
reliability mix freely (a reliable control channel + unreliable telemetry
channels on one `RTCPeerConnection`) — each peer runs in its channel's mode.
`bind` **inside `ondatachannel`** so no early message is lost; `servePeer`
performs the §4.5 teardown on every exit.

```ts
import { Server } from '@lesomnus/grpc-dgram'
import { DataChannelGateway } from '@lesomnus/grpc-dgram/transport/webrtc'

const gw = new DataChannelGateway()
const server = new Server(gw)
// server.register(...)

pc.ondatachannel = ({ channel }) => {
  gw.bind(channel)
  void gw.servePeer(server, channel)
}
```

## Options

`{ maxMessageSize, maxBufferedAmount, sendStallTimeoutMs }` — the size limit
(§4.4; default 16 KiB reliable / 1200 B unreliable), the outbound high-water
mark for backpressure, and the stall budget that declares a wedged channel dead
(§4.2). A `RTCPeerConnection` can fail without the channel firing `close`; watch
the peer-connection state and close the channel (or the `Conn`/`Server`)
yourself when it does.

## Note (browser)

A browser `RTCDataChannel` cannot pause delivery, so reliable-mode backpressure
(§4.2) bounds ordering and loss but **not** adapter rx memory — inbound
messages queue while a slow consumer drains. The Node/pion read-loop blocking
has no browser equivalent.
