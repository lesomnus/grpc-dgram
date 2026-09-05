# Reliable mode

Over a WebSocket or a reliable WebRTC DataChannel there is no loss, no
duplication and no reordering to defend against. `grpc-dgram` notices, turns
the whole datagram machinery off, and gives you plain gRPC semantics: the
exact sequence a handler sent, exactly-once unary execution, and a call that
fails loudly the moment the channel misbehaves.

This page is the practical guide to that path. The normative rules live in
[PROTOCOL.md](./PROTOCOL.md) — mode selection in §4.3, the mode table in
§10.6, flow control in §4.2.1, the adapter's duties in §4.4–§4.5 — and
are cited here rather than restated.

## Picking the mode

There is no option to set and nothing to negotiate on the wire. A transport
that knows it is reliable says so through `TransportInfo`, and
`NewConn`/`NewServer` read it once at construction (§4.3).

| adapter | mode |
|---|---|
| [`transport/gorilla`](../transport/gorilla) (WebSocket) | always reliable |
| [`transport/pion`](../transport/pion) (WebRTC) | ordered + no retransmit/lifetime cap → reliable, else unreliable |
| [`transport/jsport`](../transport/jsport) (JS message port) | always reliable — a port in one process cannot lose, duplicate or reorder |
| [`transport/udp`](../transport/udp) | always unreliable |
| [`ts/src/transport/websocket`](../ts/src/transport/websocket), [`webrtc`](../ts/src/transport/webrtc), [`port`](../ts/src/transport/port) | the same rules, in TypeScript |

Nothing in the wiring mentions the mode:

```go
// server
gw := gorilla.NewGateway()
srv := drpc.NewServer(gw)
pb.RegisterEchoServiceServer(srv, &myHandler{})

http.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
    c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
    if err != nil {
        return
    }
    // Blocks until the socket dies, then calls srv.DisconnectPeer.
    _ = gw.ServePeer(r.Context(), srv, c)
})

// client
c, _, err := websocket.DefaultDialer.DialContext(ctx, "wss://host/rpc", nil)
if err != nil {
    return err
}
conn := drpc.NewConn(gorilla.New(c)) // reliable mode discovered here
defer conn.Close(nil)
```

A custom transport that does not implement `TransportInfo` gets the explicit
override — on **both** endpoints, for the reason in the next section:

```go
_ = drpc.NewConn(tx, drpc.WithReliable(true))
_ = drpc.NewServer(tx, drpc.WithReliable(true))
```

On the server the mode is **per peer**, not per `Server`: reliability is a
property of a channel, and one WebRTC `PeerConnection` may carry a reliable
control channel next to unreliable telemetry channels. A gateway annotates
each channel's receive context (§4.3):

```go
rxCtx := drpc.NewPeerContext(ctx, key)
rxCtx = drpc.NewReliableContext(rxCtx, true)
```

Frames without the annotation fall back to the server's own mode, so a
single-mode gateway can rely on `TransportInfo` alone. `mode_test.go` pins
the mixed case: a reliable peer's idle stream survives past `T_live` on a
server whose default is unreliable, which a single-mode server cannot do.

## Both ends of a channel must agree

§10.6 requires the two endpoints of one channel to be in the same mode.
There is no negotiation — the first frame of a call is already an OPEN — so
the channel itself is the agreement mechanism: both sides derive the mode
from the same channel, and an explicit `WithReliable` is legal only when
applied at both ends. A mismatch does not degrade; it breaks in three
specific ways.

- **Unreliable client, reliable server.** The client retransmits its control
  frames every `RTI` (§10.3). To the strict receiver each retransmission is
  a duplicate, which fails the call with `INTERNAL`. A unary call whose
  response takes longer than ~1 s therefore *cannot* complete: the OPEN is
  resent, and the resend kills the call it was meant to rescue.
- **Reliable client, unreliable server.** A reliable endpoint sends no
  keepalive PING. The unreliable server's liveness window (§10.4) expires
  after `T_live` and cancels handlers that are perfectly healthy — including
  ones it is actively feeding, because a reliable-mode pure consumer sends
  nothing back to refresh the clock.
- **…and then it hangs.** A liveness expiry sends no terminal frame (§10.4:
  the peer is presumed gone). The reliable client runs no probe, no liveness
  and no default `T_call` deadline, and the transport is healthy so §4.5's
  teardown never fires. Without an explicit `ctx` deadline that call waits
  forever.

Mixed reliabilities under one server are expressed per peer, never by letting
one channel's two ends disagree.

## What turns off, and what turns on

| | Unreliable | Reliable |
|---|---|---|
| `T_call` default unary deadline | on (5 s) | **off** — explicit ctx deadlines still travel and are enforced |
| Retransmission, PING, stream probe, liveness | on | **off** — an abort or half-close is sent exactly once |
| Tombstones | on | **off** — "send and tombstone-store `T`" reads as "send `T`" |
| Aged watermark | `hwm_aged` with checkpoints | degenerate: plain `sid > hwm` |
| `T_hold` (delayed RESET) | `RTI` | **0** — nothing is ever merely reordered |
| seq validation | 4096-frame window, fail-loud at `K_loud` | any gap or duplicate fails the call `INTERNAL` |
| rx buffering | drop per `DropNewest`/`DropOldest` | per-stream **and per-peer** flow control |
| Liveness responsibility | the protocol | **the adapter** (§4.5) — non-optional |

The strict sequencing is the visible half. A gap or a duplicate on a channel
that promised neither is a broken transport, not a lost sensor reading, so
the core surfaces it: a gap or duplicate on a live stream fails that call
with `INTERNAL` (`reliable transport lost or reordered a frame`), and so does
a second OPEN at the server — nothing retransmits here, so a duplicate can
only mean the transport made one.
`TestChar_ReliableModeGapIsInternal` drops one server data frame on an
otherwise reliable pipe and asserts that; `websocket-echo` asserts the
positive form, that `Count` delivers `1..N` with no gaps.

## Flow control

This is the newest part of reliable mode (wire v1.1) and the least obvious.
It is HTTP/2's per-stream window, counted in **messages** rather than bytes,
and it exists to remove one specific failure. A second window sits beside it
— [the connection window](#the-connection-window), one per peer — and bounds
what a peer can pin across all of its calls at once.

A reliable adapter delivers every call's frames from **one** read loop
(§4.2), because that is what makes its blocking propagate into TCP/SCTP
backpressure. Before v1.1 the only back-pressure a receiver had was to stall
that loop. So:

- **Before.** A client opens a 200-message server-streaming call and stops
  reading. Its 32-frame buffer fills. The delivery goroutine blocks handing
  over frame 33 — and every *other* call on that channel stops too, because
  they share the loop. One slow consumer froze the whole connection.
- **After.** The buffer still fills, but the *producing handler* parks in
  `Send` instead. The delivery loop stays free and other calls complete
  normally; when the application resumes reading, grants flow back and the
  handler continues — every message arrives, in order.

`TestFlow_StalledConsumerDoesNotBlockOtherCalls` is exactly that scenario: a
200-message stream nobody reads, then a unary `Once` on the same channel that
must still return. Under the pre-v1.1 core that `Once` never returned.

### The advertisement

A receiver advertises its per-call buffer, in messages, in `Frame.window`:
the **client** on its `OPEN`, the **server** on its **creation-ack `H`** —
which is why that ack is mandatory for every streaming call in reliable
mode, server-streaming included (§8).

Each direction is paced independently: the OPEN's window governs what the
*server* sends, the `H`'s window what the *client* sends. On a
server-streaming call the second is inert — the client sends nothing after
its request. On the wire, with default buffers:

```
C→S  seq1   OPEN|CLOSE  method=/wsecho.EchoService/Count  window=32
S→C  seq1   H           window=32          ← creation ack + advertisement
S→C  seq2   data #1
     ...
S→C  seq33  data #32                ← window spent; the next Send parks
C→S  seq0   WINDOW      window=16   ← the app consumed 16
S→C  seq34  data #33
     ...
S→C  seqN   T  code=OK
```

`WINDOW` frames carry `seq = 0` and bypass seq validation entirely (§6.3), so
a grant neither advances nor trips the strict window it travels alongside.
`TestFlow_AdvertisementsAndGrantsOnTheWire` asserts all three shapes on a
real pipe: the OPEN's window, the ack's window, and the grant that consuming
half a window produces.

### `W_init`: what a sender must assume

An advertisement takes a round trip, and a client-streaming sender can empty
a burst onto the wire before the ack it would have been paced by ever lands.
So a sender that has heard nothing yet paces itself by `W_init` = **32
messages** — exactly as an HTTP/2 sender assumes 65535 bytes before
`SETTINGS`. That assumption is only safe if every reliable-mode receiver can
hold it, which is why the rx buffer has a **floor of `W_init`**: a smaller
configured buffer is silently raised to 32.
`TestFlow_ReliableRxBufferFloorAndOverrun` configures a buffer of 2, asserts
the ack advertises 32 anyway, and pushes 32 messages through before anything
drains.

When the advertisement arrives it is **authoritative**: it replaces the
assumption and is counted against what the sender has already sent. A window
smaller than the assumption fails nothing — it parks the sender until the
receiver drains enough to grant. A window of **0** means "this peer does no
flow control": the sender becomes unlimited, the pre-v1.1 behavior.

### Grants

A receiver returns credit as its **application** consumes messages, not as
frames arrive — the point is to track buffer occupancy. Grants are batched at
**half the window**, as HTTP/2 stacks do, so a steady stream costs one small
frame per `window/2` messages rather than one per message.

Only **data frames** consume credit. `OPEN`, `H`, `T`, half-close, abort,
`RESET`, `PING` and `WINDOW` are never credited: they are not buffered, and
crediting them would leave a call at zero credit unable to terminate. The
eager OPEN of a client-streaming call is free, and so is the response riding
a `SendAndClose` terminal.

Credit taken for a frame the adapter then refuses synchronously — an
oversize envelop wrapping `drpc.ErrMessageTooLarge` (§4.4) — is refunded.
That frame never reached the wire, and gRPC lets a handler ignore what `Send`
returns; without the refund such a handler would leak its whole window and
then park on every later message until `T_stall`.

### The connection window

A stream window bounds one call. What a *peer* could pin across all of its
calls was, until now, `MaxLiveCalls × window` — 4096 × 32 messages at the
defaults, a number nobody chose. So beside the stream windows there is one
**connection window** per peer, HTTP/2's connection-level window counted in
messages (§4.2.1): a data frame needs one credit from its stream **and** one
from the peer, and a receiver holds at most `Limits.MaxPeerWindow` messages
from one peer at a time, whatever the calls. Three things make it different
from the stream window.

- **It is never advertised.** Every sender assumes `W_conn` = **1024
  messages** toward each peer and counts from its first data frame. The
  peer's first per-stream advertisement settles that assumption — a window
  above 0 confirms it, 0 turns it off — and only a *streaming* call's
  creation-ack `H` does so on the client: a unary `T` carries no window and
  never settles anything, so a `Conn` that only ever made unary calls stays
  on the assumption. On the server the first OPEN it admits from a client
  incarnation settles it, unary included, since every reliable-mode OPEN
  carries the client's window. Because the assumption is the whole of the
  sender's initial credit, `MaxPeerWindow` has a **floor of 1024**: a
  receiver holding less would be overrun by a conforming sender — the
  `W_init` argument again. A receiver configured *above* it lifts the sender
  once, with a `sid = 0` grant of the difference right behind its first OPEN
  (client) or its first creation ack to that incarnation (server).
  `TestPeerWindow_UnaryTerminalNeverSettles`,
  `TestPeerWindow_StreamingAckWithWindowZeroTurnsItOff` and
  `TestPeerWindow_RaiseBehindFirstOpen` pin the three halves of that rule.
- **Credit rides `WINDOW` with `sid = 0`.** A grant on sid 0 credits the
  peer's connection window; every other sid credits its call, as before.
  The receiver returns one credit for **every** data frame it received, once,
  when the frame stops occupying a buffer: consumed by the application,
  discarded with a call that ended first, or never buffered at all — an
  off-shape frame, a frame for a call that is already gone (the ones
  pipelined behind an abort, which each draw a RESET on a reliable channel),
  a frame the window itself refused. The window is cumulative for the life
  of the peer, so a credit that never came back would be a permanent shrink,
  which is why the ledger counts what is physically buffered and junk cannot
  desync it (`TestPeerWindow_NeverBufferedFramesReturnCredit` and its server
  twin). Grants batch at half the window like stream grants — and fire
  regardless whenever the peer would otherwise be starved, so seventeen stuck
  consumers pinning most of the window cannot cost a healthy eighteenth its
  `T_stall` (`TestPeerWindow_StarvationClause`).
- **Scope.** The receiver's bound is per **transport peer** on the server —
  across client epochs, as `MaxLiveCalls` is counted, so a restarting or
  epoch-spoofing peer holds no more than an honest one
  (`TestLimits_PeerWindowScope`) — and per `Conn` on the client. The sender's
  credit is per incarnation: one per (peer, client-epoch) container on the
  server, one per `Conn` on the client, started over from `W_conn` if the
  `Conn` hears a new server incarnation on a surviving channel
  (`TestPeerWindow_ReassumeOnNewServerEpoch`). The `Conn` locks to a server
  incarnation on the first sequenced frame it hears from it, for a live call
  or for one it has already released — the server's raise rides right behind
  its first creation ack, and a `Conn` that only locked from live calls would
  drop it when that call was cancelled before the ack arrived
  (`TestPeerWindow_RaiseSurvivesACancelledFirstCall`). And because the
  ledger is per peer while grants are per incarnation, what it holds back is
  held back per incarnation too: a restarted client's returned credit never
  rides in a grant addressed to its dead predecessor
  (`TestPeerWindow_CreditIsGrantedToTheIncarnationThatSpentIt`).

A send takes stream credit first and refunds it if the peer's window turns
out to be empty, so a stream parked on its own window never holds a piece of
the shared budget. There is one park and one `T_stall` for both windows.

Nothing changes at the defaults for the flows that exist today: 1024 messages
is 32 full stream windows, so a sender never parks on the connection window
with fewer than 32 saturated streams, and no `sid = 0` frame appears until 512
messages have been consumed in one direction — a stream's first WINDOW is
still its own, the flows the existing tests move never see one, and a
receiver at the floor has nothing to raise by.
`TestPeerWindow_ParksAcrossStreamsAndKeepsTheChannelLive` is the scenario the
window exists for: two 600-message streams nobody reads, on a channel whose
stream buffers are deep enough that no stream window binds, and a unary `Once`
that still returns while both producers park on the peer's window;
`TestPeerWindow_GoToGoPastWConnBothWays` moves 1200 messages each way across
three streams under the defaults, which completes only because both ends
grant on sid 0.

### What a parked sender looks like

Nothing new appears in the API. A parked sender is a `SendMsg` that has not
returned yet:

```go
func (myHandler) Count(req *pb.CountRequest, stream grpc.ServerStreamingServer[pb.EchoResponse]) error {
    for i := uint32(1); i <= req.GetCount(); i++ {
        // Blocks here once the client's window is spent, and returns only
        // when credit arrives, when the call ends, or after T_stall.
        err := stream.Send(&pb.EchoResponse{
            Message:  fmt.Sprintf("tick %d", i),
            Sequence: i,
        })
        if err != nil {
            return err
        }
    }
    return nil
}
```

and the consumer that causes it is an ordinary `Recv` loop:

```go
for {
    res, err := stream.Recv()
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        return err // INTERNAL on a gap, UNAVAILABLE on transport death
    }
    log.Println(res.GetSequence())
    time.Sleep(10 * time.Millisecond) // a slow consumer: the server parks
}
```

A park ends for one of four reasons: credit arrives; the call's own context
is cancelled or its deadline expires; the call ends underneath the sender
(then `Send` reports the call's status, or `io.EOF` on the client); or
`T_stall` elapses.

### `T_stall`

`T_stall` (default **30 s**) is the only bound reliable mode owns itself.
Everything else delegates to the adapter, but a park happens *before* the
adapter's write path, so its write deadline never sees one, and no protocol
timer is running. Past `T_stall` the call fails `UNAVAILABLE` rather than
hanging, and the message names the window that starved it: `the peer granted
no credit for 30s` for the stream's, `the peer granted no connection credit
for 30s` for the peer's. A send that parks on its stream window and later on
the connection window gets one budget, not two — `T_stall` is armed at the
first park and measures the whole wait.

```go
_ = drpc.NewConn(tx,
    drpc.WithReliable(true),
    drpc.WithTiming(drpc.Timing{Stall: 5 * time.Second}),
)
```

`Timing.Stall` is the one field of `Timing` that is live in reliable mode;
the rest configure timers this mode does not run.

### When a peer overruns its window

A conforming sender never exceeds the credit it holds, so a full buffer on a
flow-controlled stream is a contract violation, and the receiver must **not**
block on it. Blocking would be the deadlock flow control exists to remove:
the grant that would unpark the peer has to travel the very read loop the
block is stalling. Instead the receiver fails **that one call** with
`INTERNAL` (`peer exceeded the advertised flow-control window`) and the
channel keeps running for everyone else.

The connection window has the same rule at the peer level. A data frame that
would take a peer past `MaxPeerWindow` is not buffered, and the call it
belongs to fails `INTERNAL` (`peer exceeded the connection flow-control
window`) — that call only, never the peer
(`TestPeerWindow_OverrunFailsOnlyTheOffendingCall`, both roles). HTTP/2
answers the same violation by closing the connection; here the connection is
the adapter's (§4.5), and closing it from inside the read loop would turn one
accounting slip — two `Conn`s sharing a channel, a client restarted at the
same address on a datagram channel — into an outage for every call. The
refused frame's credit goes back like any other never-buffered frame's, so
the bound holds either way.

Three rules keep a grant from becoming a weapon (§4.2.1, §15):

- A `WINDOW` for an unknown, finished or tombstoned sid is dropped **in
  silence**, never answered with a `RESET`. A grant legitimately races the
  end of its own call — the last consumed message produces one — so answering
  would turn every well-behaved stream's tail into a RESET exchange, and hand
  an off-path attacker a free amplifier.
- A grant never *enables* flow control. Only an advertisement does. Otherwise
  one stray, duplicated or injected `WINDOW` could park a sender that was
  never paced at all. Unreliable mode ignores `window` and `WINDOW` outright:
  no advertisement on the OPEN, no grants sent, injected grants inert
  (`TestFlow_UnreliableModeIgnoresWindow`,
  `TestPeerWindow_Sid0GrantIgnoredInUnreliableMode`).
- A `WINDOW` on `sid = 0` credits only a connection window that exists: on
  the client one locked to the server incarnation the frame names, on the
  server the container of the client incarnation it names. Anything else —
  a grant before any OPEN, one naming a foreign epoch, one for a window the
  peer turned off — is dropped in silence, creates nothing, and answers
  nothing (`TestPeerWindow_Sid0GrantGate`, both roles). The bound itself is
  enforced on what is actually buffered, never on what was granted, so a
  forged grant can over-credit a sender but cannot make the receiver hold
  more.

### Watching it

```go
counters := &drpc.Counters{}
_ = drpc.NewServer(tx, drpc.WithProtocolStats(counters))

snap := counters.Snapshot()
log.Printf("stream stalls: %d/%d, peer stalls: %d/%d",
    snap.FlowStall, snap.FlowResume, snap.PeerFlowStall, snap.PeerFlowResume)
```

`EventFlowStall` fires at the moment a send first parks — while it is still
parked, which is what a stall counter has to report — and `EventFlowResume`
when that same send gets credit and continues. Rising `FlowStall` matched by
`FlowResume` is healthy back-pressure; `FlowStall` without `FlowResume` is a
consumer that stopped, heading for `T_stall`.

`EventPeerFlowStall` / `EventPeerFlowResume` are the same pair for the
connection window, and they are separate because the remedies differ. A
stream stall is *this consumer stopped*: find it. A peer stall is *the peer's
whole budget is spent*: the parked call is healthy, and some other consumers
on that channel are holding the window — or the workload genuinely needs more
than 1024 messages in flight to one peer, and `MaxPeerWindow` is the knob.
Both carry the parked call's sid and method. A send short on both windows
reports the connection one: the peer's whole budget is what it waits on, not
one consumer, and the stream pair is for a send whose peer still had credit
to give (`TestPeerWindow_CountersPeerFlow`, `TestPeerWindow_SingleStallBudget`).

## Sizing the buffer

In reliable mode the rx buffer **is** the advertised window, so sizing it is
sizing your peer's send credit — the memory a peer can pin on one call is
exactly the buffer you configured for it.

```go
_ = drpc.NewServer(gw,
    // Every call advertises 128 messages instead of 32.
    drpc.WithRxBuffer(128, drpc.DropNewest),
    // One high-rate method gets a deeper window still.
    drpc.WithMethodRxBuffer("/wsecho.EchoService/Count", 1024, drpc.DropNewest),
)
```

Most-specific wins: per-method override, else the endpoint default, else 32.
The `DropPolicy` argument is inert here — nothing is dropped in reliable
mode — and any size below 32 is raised to the `W_init` floor. Deeper windows
buy throughput on a high-latency link (fewer round trips waiting for credit)
at the cost of a larger burst a stalled consumer can pin in memory.

The connection window is sized with `Limits`, on either role — it is one of
the two `Limits` fields a `Conn` honours:

```go
_ = drpc.NewServer(gw,
    // Each transport peer may have 4096 messages buffered here, all calls
    // and client incarnations together.
    drpc.WithLimits(drpc.Limits{MaxPeerWindow: 4096}),
)
_ = drpc.NewConn(tx,
    // The one peer this Conn talks to.
    drpc.WithLimits(drpc.Limits{MaxPeerWindow: 4096}),
)
```

Values below 1024 are raised to it — a sender assumes that much — so the
memory one peer can pin is min(`MaxLiveCalls` × per-call buffer,
`MaxPeerWindow`) messages, times the adapter's frame size. A deployment that
needs a tighter pin than 1024 messages lowers `MaxLiveCalls` or the per-call
buffer; a deployment with many concurrently saturated streams to one peer
raises `MaxPeerWindow`, and the raise reaches the peer as a single `sid = 0`
grant behind the first OPEN or creation ack.

## The adapter's teardown duty

With every protocol timer off, transport death is the *only* thing that can
fail a live call, and detecting it is the adapter's job (§4.5) — the one
duty here that is not optional. An adapter that skips it leaves calls
hanging forever on a dead socket, because nothing else is running to notice.

```go
// servePeer is the shape every connection-oriented gateway has: pump is the
// adapter's read loop, which delivers frames to srv until the channel dies.
func servePeer(
    ctx context.Context,
    srv *drpc.Server,
    key any,
    pump func(context.Context, drpc.FrameHandler) error,
) error {
    rxCtx := drpc.NewReliableContext(drpc.NewPeerContext(ctx, key), true)
    err := pump(rxCtx, srv)
    // With no protocol timers running, this is the only thing that fails the
    // peer's live calls (PROTOCOL.md §4.5). It must run on every exit path.
    srv.DisconnectPeer(key, err)
    return err
}
```

The client half is `Conn.Close(err)`, which the shipped adapters call from
their pump's exit. Both are idempotent and safe from transport callbacks.

The subtle requirement is *where* the detection lives: death must be
detectable **while the read loop is blocked**, because with synchronous
delivery that loop is what blocks under backpressure, and a blocked loop has
no read pending for a read deadline to fail. At least one signal must fire
from outside it:

- `transport/gorilla` runs a ping/pong keepalive on its own goroutine (20 s
  ping, 30 s timeout). A ping the socket cannot even carry is death seen from
  the sending side, and it cancels the delivery context — unblocking `Handle`
  so the teardown can run. That same timeout is the write deadline on every
  send: a peer that stops draining would otherwise block a write forever.
- `transport/pion` and `ts/src/transport/webrtc` bound each send with
  `sendStallTimeout` (30 s), covering the channel-open wait and the
  buffered-amount mark, and treat a trip as channel death. WebRTC needs one
  thing more from the application: a severed peer connection may never
  surface `close` on the channel, since the SCTP shutdown needs a live
  transport to travel over. Watch `PeerConnectionState` and close the channel
  — or the `Conn`/`Server` — yourself when it fails.
- `transport/jsport` and `ts/src/transport/port` have **no keepalive, by
  design**. Both endpoints of a message port are in one process, so there is
  nothing to partition and a ping would only measure how busy the peer is.
  Nor is there anything for the port to report the death of. So death is
  announced instead: `Close` posts an empty message, the peer's pump reads it
  as EOF and runs the §4.5 teardown. What only the host knows — a wasm
  instance that exited, a terminated worker — it declares itself: TypeScript's
  `close(cause)` carries the cause into the failed calls, and a Go host says it
  through the core's own `conn.Close(err)` / `srv.DisconnectPeer(peer, err)`,
  since `Close` there is an `io.Closer` and takes none.

One browser caveat: an `RTCDataChannel` gives no way to pause delivery, so
inbound messages queue in the adapter while a slow consumer drains. Ordering
and the no-silent-drop contract still hold; adapter rx memory there is not
bounded by the window, only by the sender's own pacing. `postMessage` gives a
message port the same shape, with one difference that matters: a port is
always in reliable mode, so flow control is always running on it and a
conforming peer cannot post what it has no credit for.

## Where to look next

- [PROTOCOL.md](./PROTOCOL.md) §4.2.1 (flow control), §10.6 (the mode table
  and the agreement rule), §4.5 (teardown), Appendix B (defaults).
- [`examples/websocket-echo`](../examples/websocket-echo) — the runnable
  version of this page: mode auto-detected, exact sequence asserted,
  `GracefulStop` draining a live stream.
- [`flow_test.go`](../flow_test.go) and [`flow.go`](../flow.go) — every
  per-stream claim above, pinned end to end, plus the window accounting;
  [`flow_peer_client_test.go`](../flow_peer_client_test.go),
  [`flow_peer_server_test.go`](../flow_peer_server_test.go) and
  [`flow_unit_test.go`](../flow_unit_test.go) — the connection window, from
  the ledger arithmetic up to a Go↔Go channel past `W_conn` both ways.
