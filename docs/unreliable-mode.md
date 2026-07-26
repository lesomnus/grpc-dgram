# Unreliable mode

This is the datagram path — the reason `grpc-dgram` exists. A UDP socket or an
unreliable WebRTC data channel loses, duplicates and reorders messages, and
never tells you it did. Unreliable mode keeps the gRPC programming model on
top of that channel and pays for it in exactly one currency: **your stream may
be missing messages**. Nothing else about a call changes — it still ends with
one status, it still carries metadata and deadlines, and it still terminates
within a stated bound no matter which datagram the network ate. The normative
rules live in [PROTOCOL.md](./PROTOCOL.md); every mechanism below is here
because something concrete breaks without it, and each section says what.

**When it is on.** The mode is resolved once, at construction, from the
transport's `TransportInfo` (PROTOCOL.md §4.3): `transport/udp` reports
`Reliable() == false`, so `drpc.NewConn(udp.New(c))` and
`drpc.NewServer(udp.NewGateway(pc))` are already in unreliable mode with no
options, and `WithReliable(false)` forces it for a custom transport. A gateway
serving channels of mixed reliability annotates each peer with
`drpc.NewReliableContext` instead, and the server runs each peer in its
channel's mode. Both ends of one channel must agree; §10.6 says what breaks
otherwise.

## The delivery contract

What the application receives is an **ordered subsequence** of what the peer
sent (PROTOCOL.md §14): never reordered, never duplicated, only thinned.
Duplication is invisible — the per-direction `seq` check drops anything at or
below the highest accepted sequence number. Reordering is invisible *as
reordering*: the window is forward-only, so a one-step swap that puts frame 2
ahead of frame 1 gets frame 2 accepted and frame 1 dropped as too late; the
application sees a gap, not an out-of-order message. `TestFaultInjection` in
`lossy_test.go` pins both — delivering every frame twice still yields
`[0 1 2]`, and holding every data frame one step yields exactly `[1 3 5 7]`
out of eight.

Gaps are silent. There is no NACK and no data retransmission — resending a
20 ms-old reading only delays the current one — so nothing in the receive path
can tell the application "you missed #7". The loop is an ordinary gRPC loop:

```go
stream, err := client.Readings(ctx, &sensorpb.Subscribe{Hz: 200})
if err != nil {
    return err
}
for {
    r, err := stream.Recv()
    if errors.Is(err, io.EOF) {
        break // the handler returned nil: the feed ended cleanly
    }
    if err != nil {
        return err // one gRPC status, and the call is over
    }
    use(r) // r.GetSeq() may have jumped since the last one
}
```

`err` is non-nil exactly once, at the terminal. End-of-stream is a real frame,
never inferred from silence, so `io.EOF` means the handler returned — not that
the network went quiet. To see the shape of what you lost, put a sequence
number in your own message, as `examples/udp-sensor` does with `Reading.seq`;
the protocol's own counter is [below](#seeing-the-loss).

**The one loud exception.** A loss burst longer than the receive window
(`W_fwd` = 4096 frames) can no longer be told apart from a forged or corrupt
sequence number. After `K_loud` = 3 mutually consistent beyond-window frames
arrive with no accepted frame between them, the receiver stops guessing and
fails the call with `DATA_LOSS` (§6.3). That is the only loss that is not
silent, and on a sane link it does not happen.

## The rx buffer: the second source of loss

`Handle` must not block in unreliable mode (PROTOCOL.md §4.2). One adapter
read loop feeds every call on the channel, so a receiver that blocked waiting
for its application to catch up would stall every *other* call too — the
head-of-line blocking that reliable mode solves with per-stream flow control.
That solution is unavailable here: pacing a sender needs the credit grant
itself to arrive, and nothing on this channel is guaranteed to.

So each stream gets a bounded rx buffer, **32 frames by default**, and when it
is full an arriving data frame is discarded exactly as if the network had lost
it. The policy chooses which frame dies:

| Policy | Discards | Keeps | Suits |
|---|---|---|---|
| `DropNewest` (default) | the arriving frame | the buffered prefix | request/response and anything where the earliest pending work matters |
| `DropOldest` | the oldest buffered frame | the freshest | state-sync and sensor feeds, where the latest reading supersedes older ones |

`wire_shape_test.go` pins the difference exactly: four data frames injected
into a 2-slot buffer before the application reads anything leave `[m1 m2]`
under `DropNewest` and `[m3 m4]` under `DropOldest`. Both preserve the ordered
subsequence — the policy chooses *which* messages are missing, never the order
of what survives. Resolution is most-specific-wins: a per-method override on
the server, else the endpoint-wide default, else 32/`DropNewest`.

### A worked sensor example

`examples/udp-sensor` is the whole shape in one runnable module. The server
gives the feed a deep freshest-wins buffer, leaving every other method alone:

```go
srv := drpc.NewServer(gw,
    drpc.WithMethodRxBuffer(sensorpb.SensorService_Readings_FullMethodName, 64, drpc.DropOldest),
    drpc.WithLimits(drpc.Limits{MaxLiveCalls: 64}),
)
sensorpb.RegisterSensorServiceServer(srv, impl)
```

`WithMethodRxBuffer` is a server option only, keyed by the full method name the
generated code exports. A client tunes per endpoint — one `Conn` is one
channel:

```go
counters := &drpc.Counters{}
conn := drpc.NewConn(udp.New(c),
    drpc.WithRxBuffer(4, drpc.DropOldest),
    drpc.WithProtocolStats(counters),
)
```

The example makes the consumer slower than the feed (200 Hz produced, 8 ms per
reading) and drops 5 % of outbound data frames, since loopback UDP loses
nothing. A run:

```
  readings produced   : 399 (seq 1..399)
  readings delivered  : 243 (60.9%)
    lost on the wire  : 12 (the §14 gap counter)
    evicted, DropOldest: 144 (rx buffer full while this consumer lagged)
```

Most of the loss is the consumer's own lag, not the network — which is the
point. Raise `-rx-buffer` or lower `-consume` and the evictions disappear.

## Seeing the loss

The two kinds of loss are counted separately, and the difference matters:

| Counter | Event | What happened |
|---|---|---|
| `Skipped` | `EventSkipped` | the datagram never arrived; the seq window saw the hole and counted the messages it ate |
| `Dropped` | `EventDropped` | the datagram *did* arrive and was discarded by the drop policy — the window accepted it, so it leaves no gap to detect |
| `DataLoss` | `EventDataLoss` | a window overrun failed the call loudly (§6.3) |

An eviction is invisible to every sequencing check on the wire, which is why it
needs its own counter; `TestStats_CountersDroppedRxPolicy` asserts that 10
messages into a 2-slot buffer report `Dropped == 8` and `Skipped == 0`.
`counters.Snapshot()` returns those fields plus the RESET, retransmission,
probe, keepalive, liveness-expiry and tombstone-replay tallies.

To react rather than tally — request a keyframe, log, degrade a display —
install a function instead. Each event names its call, and `Count` on a skip
is the number of *messages* the gap ate, not the number of gaps:

```go
onGap := drpc.ProtocolStatsFunc(func(ev drpc.ProtocolEvent) {
    if ev.Kind == drpc.EventSkipped {
        log.Printf("%s sid=%d: %d readings lost", ev.Method, ev.Sid, ev.Count)
    }
})
```

Observers run on the receive and timer paths and must not block — the
endpoint's whole receive loop waits behind a slow one.

## Why there are timers at all

On a reliable transport a broken connection announces itself. Here it does
not: a crashed peer and a merely quiet one look identical on the wire, and any
datagram can vanish without a trace. Every timer below makes one specific
"wait forever" impossible; together they are goal G1, eventual termination.

**`T_call` (5 s) — the default unary deadline.** If a unary caller's context
carries no deadline, the client injects one; without it a unary call into a
black hole would retransmit its OPEN forever with nothing to stop it. It is a
deliberate divergence from gRPC, which lets such a call run unbounded. A
streaming call gets no default deadline (long-lived streams are a goal) and is
bounded by the mechanisms below instead.

**Control retransmission (`RTI` = 1 s, doubling, capped at `T_probe`).** Data
frames are never retransmitted, but the handful of frames that decide whether
a call *exists* are: the OPEN, the client's half-close, and the client's
abort. They go out byte-identically, same `seq`, so the receiver dedups them
for free; with defaults the retries land at +1 s, +3 s, +7 s, +12 s, +17 s…
Without this, one lost datagram out of thousands could lose a whole call: a
lost OPEN is a call the server never hears about, and a lost half-close leaves
`CloseAndRecv` waiting for a handler that is itself waiting for EOF. A stream
keeps one schedule for everything it owes, restarted at `RTI` whenever a new
obligation is armed, and each obligation stops on its own evidence (§10.3 has
the table): the first server frame for the sid, a matching terminal, a RESET,
or the call ending locally.

The two frames that *answer* a call — the creation ack `H` and the terminal
`T` — are not on a timer; they are replayed on demand, `H` by a duplicate OPEN
and `T` by a straggler or probe hitting a tombstone, rate-limited to one
replay per `RTI` per object plus a per-peer aggregate budget so that a flood
cannot turn the server into an amplifier.

**Tombstones (`TTL_tomb` = 30 s).** When a call ends, each side remembers the
key, the status and — on the server — the terminal frame. This is what makes
retransmission safe: a client whose response was lost retransmits its OPEN,
and the tombstone answers with the *stored* reply instead of running the
handler again. That is the at-most-once story in one sentence: **execution is
deduplicated, the response is delivered at-least-once.** `timeout_test.go`
pins it — with the terminal dropped once, the unary call still succeeds and
the handler execution count is exactly 1. Without tombstones that recovery
would be a second execution of a handler that already charged the credit card.
Memory is bounded (§15): past the byte cap the oldest stored terminals degrade
to key-only, past the entry cap the lowest sid is evicted and a container
floor covers it — both keep dedup and lose only the replay, so cap pressure
costs a timeout, never a re-execution.

**The aged watermark.** Tombstones expire, and a network-duplicated OPEN could
arrive after that. The server therefore keeps, per client incarnation, coarse
checkpoints of the highest sid it ever created, and admits a call only if its
sid is above the value that mark held `TTL_tomb` ago. Such a sid was allocated
more than `TTL_tomb` ago, so if its call ever existed here its tombstone is
gone and the OPEN is necessarily stale: RESET, never re-execute. The age gate
is why this is not a plain sliding window — sid distance measures *call
count*, not time, so a fixed window would reject the legitimate retransmission
of a merely-lost call during any burst of opens. The same test walks it: an
OPEN replayed after `TTL_tomb` draws a RESET, execution count still 1.

**Peer liveness (`T_live` = 15 s, `PING sid = 0`).** Each side keeps one timer
per peer incarnation while any call with that peer is live, refreshed only by
*validated* frames (§9.1) — junk and RESETs do not count, so a flood cannot
keep a ghost alive. A side that has sent nothing for `T_probe` sends a
keepalive, so a healthy-but-silent peer never expires. On expiry the client
fails its calls `UNAVAILABLE` ("peer lost") and the server cancels the handler
contexts, sending no terminal because there is no one to send it to. Without
this, a client unplugged mid-stream pins a handler goroutine forever and
`GracefulStop` never returns, which is what `TestEventualTermination/vanished
client cannot wedge the server` asserts.

**The stream probe (`T_probe` = `T_live`/3 = 5 s, `PING sid ≠ 0`).** Peer
liveness is per peer, so it cannot see one call going out of sync while other
calls keep the peer alive. Two failures live in that blind spot: a terminal
lost on a stream where the client has already stopped sending (nothing left to
retransmit, so it would wait forever), and an orphaned handler whose client
forgot the call (nothing addressed to that sid ever arrives again). When a
call's receive *and* send have both been idle for `T_probe`, its owner sends a
probe every `T_probe`. A live peer treats it as a no-op, a tombstone with a
stored terminal replays it, and an unknown or key-only sid answers an
immediate RESET — so a lost terminal is recovered within about `T_probe + RTI`
and an orphaned handler within about `T_probe`.

Note what the probe is *not*: an idle timeout. A healthy stream silent for an
hour is never killed; both sides just exchange one tiny frame per `T_probe`,
which `TestStreamProbe_HealthyIdleStreamSurvivesProbes` pins in both
directions.

### The bounds an application actually feels

With defaults, for a single loss; `k` independent losses add ~`k` rounds at
the then-current backoff. §10.7 is the full table, state release included.

| What happened | Observed within |
|---|---|
| Unary, any loss pattern | its deadline (default `T_call` = 5 s) |
| Lost terminal on an idle stream | `T_probe + RTI` ≈ 6 s |
| Lost half-close or abort, peer alive | the next retransmission round (≤ 5 s) |
| One side forgot the call | `T_probe + RTI`, or first frame + `T_hold` |
| Peer vanished | `T_live` = 15 s |

## Deadlines on both ends

A deadline is not a timer the client keeps to itself. The remaining budget
travels on the OPEN as `Frame.timeout` and the server bounds the handler
context by it, so both ends reach the same conclusion at the same time and the
server never waits for a frame to learn the call is over
(`TestDeadline_ServerEnforcesWithoutClientFrames`).

For a stream, whose feed has no natural end, the subscription *is* a time
budget:

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()

stream, err := client.Readings(ctx, &sensorpb.Subscribe{Hz: 200})
```

`Recv` then ends with `codes.DeadlineExceeded`, and the handler's context is
cancelled at the same moment by the server's own clock — the same cancellation
an ordinary gRPC handler already knows how to read:

```go
ctx := stream.Context()
for seq := uint64(1); ; seq++ {
    select {
    case <-ctx.Done():
        // The budget expired on this server's own clock, or the peer went
        // silent for T_live. Either way the handler learns it without
        // waiting for a frame.
        return status.FromContextError(ctx.Err()).Err()
    case <-ticker.C:
    }
    if err := stream.Send(&sensorpb.Reading{Seq: seq}); err != nil {
        return err
    }
}
```

Three details that differ from a naive reading:

- A client-asserted budget is trusted by default, as in gRPC;
  `drpc.NewServer(gw, drpc.WithMaxHandlerTimeout(30*time.Second))` clamps it.
- A budget that is present but non-positive — the deadline expired while the
  OPEN was in flight — yields an already-expired handler context, so the
  handler unwinds into `T{DEADLINE_EXCEEDED}` at once, never unbounded.
- When the server's own clock expires the call it *sends and tombstones*
  `T{DEADLINE_EXCEEDED}`, so a still-retransmitting client gets a status
  rather than silence. As in gRPC, side effects that already happened are not
  undone by the client observing a deadline.

## What a restart looks like

A restart is not a special mechanism; it is what the epoch rules compose into
(PROTOCOL.md §6.5, pinned by `restart_test.go`).

**The server restarts while a unary call is in flight.** The client has
accepted nothing yet, so it is still retransmitting its OPEN. The new
incarnation has no tombstone and no watermark for that sid — dedup state died
with the old process — so to it the retransmission is a fresh call and **the
handler runs a second time**. The response comes back under the new server
epoch, the client's stream has not locked to any incarnation yet, and it is
accepted: the call succeeded, within its deadline, with nothing unusual for the
application to see. This is the one residual in at-most-once (§16 L2) — the
guarantee holds *per server incarnation*. Make unary handlers idempotent if a
hidden double execution is unacceptable.

**The server restarts mid-stream.** Here the client stream did lock — to the
first server epoch it accepted — so nothing the new incarnation says can reach
it. The next client frame, or at the latest the stream probe, hits the new
incarnation as an unknown sid and draws a RESET; the call fails `UNAVAILABLE`
("call reset by peer") within one client transmission plus `T_hold`, or
`T_probe + T_hold` on a fully idle stream. Both are far under the `T_live`
backstop. The `Conn` is not poisoned — the very next call on it reaches the
new incarnation and succeeds.

**The client restarts.** The new process is a new epoch at the same address,
and the server keys everything by (peer, client-epoch, sid), so the old call
and the new incarnation's calls coexist: new calls work immediately and the
old handler is undisturbed by them. The old call ends one of two ways. Server
frames for it reach the restarted client naming the *old* incarnation in
`peer_epoch`, so the new client refuses them — even if it has already
re-allocated that same sid to a live call of its own — and answers RESETs that
reclaim exactly the old call at the server. Failing that, peer liveness
expires it within `T_live`. Either way it is reclaimed with zero cooperation
from the process that vanished, so `GracefulStop` cannot be wedged by it.

The rule for an application is short: treat `UNAVAILABLE` as "resubscribe",
and make handlers with side effects idempotent.

## Tuning

Timers are set per endpoint with `WithTiming`; a zero field keeps its default.

| Field | Default | Raise it to | Lower it to |
|---|---|---|---|
| `Call` (`T_call`) | 5 s | tolerate slow handlers on deadline-less unary calls | fail a black-holed unary faster |
| `Liveness` (`T_live`) | 15 s | survive longer outages before failing live calls | detect a vanished peer sooner (also shortens `T_probe`) |
| `Retransmit` (`RTI`) | 1 s | cut retransmission traffic on a high-RTT link | recover a lost control frame sooner |
| `Tombstone` (`TTL_tomb`) | 30 s | widen the window in which a duplicate OPEN is still deduped | reclaim server memory sooner |
| `Hold` (`T_hold`) | = `RTI` | give a reordered OPEN more grace before its frames draw a RESET | RESET an unknown sid faster |

(`Stall`, the sixth field, bounds a sender parked on flow-control credit and
is inert here.) Two couplings are easy to trip over: `T_probe` is always
`T_live`/3 and is not separately settable, so changing liveness moves the probe
cadence and the retransmission backoff cap with it; and `TTL_tomb` is floored
at `2 × T_live`, so a large liveness window lengthens tombstone retention.

```go
drpc.NewConn(tp, drpc.WithTiming(drpc.Timing{
    Call:     2 * time.Second, // T_call
    Liveness: 6 * time.Second, // T_live; T_probe becomes 2s
}))
```

Buffers and caps:

| Knob | Default | Scope |
|---|---|---|
| `WithRxBuffer(size, policy)` | 32 frames / `DropNewest` | endpoint-wide, both roles |
| `WithMethodRxBuffer(method, size, policy)` | — | one full method name, server only; wins over the above |
| `Limits.MaxLiveCalls` | 4096 | live calls per transport peer, across client epochs — the bound on handler goroutines one peer can spawn |
| `Limits.MaxTombstones` / `MaxTombstoneBytes` | 1024 / 1 MiB | per client incarnation; past them replays degrade, dedup does not |
| `Limits.MaxDeadPeers` / `MaxRepliesPerRTI` | 4 / 64 | retained no-live-call incarnations per peer; volunteered replies per peer per `RTI` (anti-amplification) |

`W_fwd` (4096) and `K_loud` (3) are fixed protocol constants, not options: a
knob there would buy nothing but a setting two implementations could disagree
on. And one knob lives outside the core — message size is the adapter's
business (§4.4), so `udp.WithMaxMessageSize(n)` (default 1200 bytes) decides
whether a marshaled envelop fits a datagram; an oversize send is refused
synchronously as `ResourceExhausted` on the owning call, never as silent loss.

## Where to look next

[PROTOCOL.md](./PROTOCOL.md) §4.2, §9, §10 and §14 hold the normative rules
behind everything above; [`examples/udp-sensor`](../examples/udp-sensor) is
the runnable version of this document; and `timeout_test.go`,
`restart_test.go`, `retx_test.go`, `ackrecovery_test.go` and
`characterization_test.go` are the evidence for each bound.
