# Envelop batching: the measurement (issue #5)

`docs/TODO.md` §1 deferred the `Coalescer` behind one entry condition: "a
benchmark that shows syscall or header overhead dominating at a real message
rate". This is that benchmark's run, and §1 now carries what it decided.

The harness is not in the tree. It was two test files under `transport/udp`
— `bench_test.go`, which measured the send path, and `benchtable_test.go`,
which turned a `go test -bench` output file into the tables below — and both
were retired once the question was answered: they existed to decide one thing,
they decided it, and a benchmark nobody reads is a file that has to keep
compiling. `git show ab77531:transport/udp/bench_test.go` (and the same for
`benchtable_test.go`) brings them back if the numbers ever need re-deriving on
other hardware.

## The run

| | |
|---|---|
| host | AMD Ryzen 7 5825U, 8 cores / 16 threads, 12 GB |
| kernel | Linux 7.0.0-30-generic |
| Go | 1.26.4, linux/amd64 |
| governor | `performance`, EPP `performance` (restored to `powersave` after) |
| channel | loopback UDP, one goroutine draining |
| command | `go test -run '^$' -bench . -benchmem -benchtime 1s -count 10 ./transport/udp` |

The machine was idle: no container, no scheduler agent, nothing else on it.
That matters more than clock speed here, because every number in the first
table is a syscall and the spread of a syscall measurement is what a busy
machine destroys. Loopback is the right channel for this question: the
quantity under test is what the *sender* pays, and a real link would add its
own serialisation on top of it without changing that.

Rates are per stream; `msg/s` is the total across streams.
One frame is one datagram today, so the send path is charged once per message.

## Where one message goes

| payload | marshal ns | raw write ns | Handle ns | Handle − raw write | allocs/op | wire B/frame | header share of wire | samples |
|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 22 B | 198 | 3936 | 4727 | 790 | 4 | 69 | 68.1% | 10 |
| 100 B | 212 | 3923 | 4712 | 789 | 4 | 147 | 32% | 10 |
| 1024 B | 370 | 4040 | 5053 | 1013 | 4 | 1073 | 4.57% | 10 |

The syscall is `raw write`; `Handle − raw write` is everything the adapter adds
on top of it: the marshal, and the envelop and one-frame slice it allocates
around every frame. The frame itself is the core's (`core send`, below).
Header share counts both the per-frame Frame header and the per-datagram 28 B
IPv4+UDP header; only the second is amortised by batching.
A server→client data frame carries peer_epoch too: +5 B per frame (§6.1).

## What one message costs to send

| payload | core send ns | transport ns | core + transport | client send ns | transport share |
|--:|--:|--:|--:|--:|--:|
| 22 B | 376 | 4727 | 5103 | 5442 | 86.9% |
| 100 B | 420 | 4712 | 5131 | 5562 | 84.7% |
| 1024 B | 730 | 5053 | 5783 | 6318 | 80% |

`core send` is one SendMsg with an adapter that does nothing — the codec
marshal, the frame, the seq, the flow-control check; `transport` is the Handle
column above; `client send` is that same SendMsg through this adapter. The
last two columns are one quantity reached two ways — the sum is there to be
checked against the measurement, which on a loopback socket holds only to
within the spread of the raw rows. The share divides by the measured one.

## What the transport costs at rate

| payload | rate/stream | streams | msg/s | CPU of one core | wire B/s | header share of wire |
|--:|--:|--:|--:|--:|--:|--:|
| 22 B | 100 | 1 | 100 | 0.0473% | 6900 | 68.1% |
| 22 B | 100 | 16 | 1600 | 0.756% | 110400 | 68.1% |
| 22 B | 1000 | 1 | 1000 | 0.473% | 69000 | 68.1% |
| 22 B | 1000 | 16 | 16000 | 7.56% | 1104000 | 68.1% |
| 22 B | 10000 | 1 | 10000 | 4.73% | 690000 | 68.1% |
| 22 B | 10000 | 16 | 160000 | 75.6% | 11040000 | 68.1% |
| 100 B | 100 | 1 | 100 | 0.0471% | 14700 | 32% |
| 100 B | 100 | 16 | 1600 | 0.754% | 235200 | 32% |
| 100 B | 1000 | 1 | 1000 | 0.471% | 147000 | 32% |
| 100 B | 1000 | 16 | 16000 | 7.54% | 2352000 | 32% |
| 100 B | 10000 | 1 | 10000 | 4.71% | 1470000 | 32% |
| 100 B | 10000 | 16 | 160000 | 75.4% | 23520000 | 32% |
| 1024 B | 100 | 1 | 100 | 0.0505% | 107300 | 4.57% |
| 1024 B | 100 | 16 | 1600 | 0.809% | 1716800 | 4.57% |
| 1024 B | 1000 | 1 | 1000 | 0.505% | 1073000 | 4.57% |
| 1024 B | 1000 | 16 | 16000 | 8.09% | 17168000 | 4.57% |
| 1024 B | 10000 | 1 | 10000 | 5.05% | 10730000 | 4.57% |
| 1024 B | 10000 | 16 | 160000 | 80.9% | 171680000 | 4.57% |

## What a MaxDelay buys

`k` is what the window collects — the frame that opens it plus the
rate × MaxDelay that arrive while it is held — capped by the 1200 B budget.
`k/stream` batches within one call (the narrow option of TODO §1); `k/all`
batches across the streams of one peer (the wide one — it couples their loss,
§4.1). A server cannot batch across peers at all: one datagram, one address, and
PROTOCOL.md §4.1 makes that a duty of whatever sits at the seam.
The measured column is the largest k the run has a number for, at or below `k/all`.

### 22 B payload (k ≤ 29 fits the 1200 B budget)

| rate/stream | streams | MaxDelay | k/stream | k/all | k measured | ns/frame | vs unbatched | CPU of one core | wire B/frame | vs unbatched |
|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 100 | 1 | 1 ms | 1 | 1 | — | — | — | 0.0473% | 69 | — |
| 100 | 1 | 5 ms | 1 | 1 | — | — | — | 0.0473% | 69 | — |
| 100 | 1 | 20 ms | 3 | 3 | 2 | 2358 | 50.1% | 0.0236% | 55 | 20.3% |
| 100 | 16 | 1 ms | 1 | 2 | 2 | 2358 | 50.1% | 0.377% | 55 | 20.3% |
| 100 | 16 | 5 ms | 1 | 9 | 8 | 734 | 84.3% | 0.117% | 44 | 35.5% |
| 100 | 16 | 20 ms | 3 | 29 | 29 | 306 | 93.5% | 0.0489% | 42 | 39.2% |
| 1000 | 1 | 1 ms | 2 | 2 | 2 | 2358 | 50.1% | 0.236% | 55 | 20.3% |
| 1000 | 1 | 5 ms | 6 | 6 | 4 | 1269 | 72.9% | 0.127% | 48 | 30.4% |
| 1000 | 1 | 20 ms | 21 | 21 | 16 | 447 | 90.5% | 0.0447% | 43 | 38% |
| 1000 | 16 | 1 ms | 2 | 17 | 16 | 447 | 90.5% | 0.715% | 43 | 38% |
| 1000 | 16 | 5 ms | 6 | 29 | 29 | 306 | 93.5% | 0.489% | 42 | 39.2% |
| 1000 | 16 | 20 ms | 21 | 29 | 29 | 306 | 93.5% | 0.489% | 42 | 39.2% |
| 10000 | 1 | 1 ms | 11 | 11 | 8 | 734 | 84.3% | 0.734% | 44 | 35.5% |
| 10000 | 1 | 5 ms | 29 | 29 | 29 | 306 | 93.5% | 0.306% | 42 | 39.2% |
| 10000 | 1 | 20 ms | 29 | 29 | 29 | 306 | 93.5% | 0.306% | 42 | 39.2% |
| 10000 | 16 | 1 ms | 11 | 29 | 29 | 306 | 93.5% | 4.89% | 42 | 39.2% |
| 10000 | 16 | 5 ms | 29 | 29 | 29 | 306 | 93.5% | 4.89% | 42 | 39.2% |
| 10000 | 16 | 20 ms | 29 | 29 | 29 | 306 | 93.5% | 4.89% | 42 | 39.2% |

### 100 B payload (k ≤ 10 fits the 1200 B budget)

| rate/stream | streams | MaxDelay | k/stream | k/all | k measured | ns/frame | vs unbatched | CPU of one core | wire B/frame | vs unbatched |
|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 100 | 1 | 1 ms | 1 | 1 | — | — | — | 0.0471% | 147 | — |
| 100 | 1 | 5 ms | 1 | 1 | — | — | — | 0.0471% | 147 | — |
| 100 | 1 | 20 ms | 3 | 3 | 2 | 2387 | 49.6% | 0.0239% | 133 | 9.52% |
| 100 | 16 | 1 ms | 1 | 2 | 2 | 2387 | 49.6% | 0.382% | 133 | 9.52% |
| 100 | 16 | 5 ms | 1 | 9 | 8 | 757 | 84% | 0.121% | 122 | 16.7% |
| 100 | 16 | 20 ms | 3 | 10 | 10 | 645 | 86.4% | 0.103% | 122 | 17.1% |
| 1000 | 1 | 1 ms | 2 | 2 | 2 | 2387 | 49.6% | 0.239% | 133 | 9.52% |
| 1000 | 1 | 5 ms | 6 | 6 | 4 | 1302 | 72.6% | 0.13% | 126 | 14.3% |
| 1000 | 1 | 20 ms | 10 | 10 | 10 | 645 | 86.4% | 0.0645% | 122 | 17.1% |
| 1000 | 16 | 1 ms | 2 | 10 | 10 | 645 | 86.4% | 1.03% | 122 | 17.1% |
| 1000 | 16 | 5 ms | 6 | 10 | 10 | 645 | 86.4% | 1.03% | 122 | 17.1% |
| 1000 | 16 | 20 ms | 10 | 10 | 10 | 645 | 86.4% | 1.03% | 122 | 17.1% |
| 10000 | 1 | 1 ms | 10 | 10 | 10 | 645 | 86.4% | 0.645% | 122 | 17.1% |
| 10000 | 1 | 5 ms | 10 | 10 | 10 | 645 | 86.4% | 0.645% | 122 | 17.1% |
| 10000 | 1 | 20 ms | 10 | 10 | 10 | 645 | 86.4% | 0.645% | 122 | 17.1% |
| 10000 | 16 | 1 ms | 10 | 10 | 10 | 645 | 86.4% | 10.3% | 122 | 17.1% |
| 10000 | 16 | 5 ms | 10 | 10 | 10 | 645 | 86.4% | 10.3% | 122 | 17.1% |
| 10000 | 16 | 20 ms | 10 | 10 | 10 | 645 | 86.4% | 10.3% | 122 | 17.1% |

### 1024 B payload

No k > 1 fits: two frames are 2090 B of Envelop, over the 1200 B budget,
so batching cannot apply at this size.

## Rule check

Issue #5, before the run: batching is a go only if, at ≤ 1000 msg/s and ≤ 100 B,
the transport share is a majority of per-message cost **or** of wire bytes, and
the MaxDelay needed to reach a k that recovers most of it is below the reading's
useful life. The go/no-go is the conjunction; the last clause is not a
measurement — how long a reading stays useful is the operator's number.

Per-message cost is `ClientSend`: one SendMsg through this adapter, measured
whole rather than summed across two paths, so the transport share is `Handle`
over it. The core's half (`CoreSend`) is printed beside it because core +
transport is that same message reached apart — how closely the two agree is
itself a check on the run.

- **22 B payload**
  - cost: transport 4727 ns of 5442 ns per message = 86.9% — majority: true
    (the core's half is 376 ns, so 376 + 4727 = 5103 ns is the
    same message reached apart)
  - wire: 47 of 69 B per frame are header = 68.1% — majority: true
    (of which 28 B/frame is recoverable by batching, the IP/UDP header;
    the 19 B frame header is not, §4.1)
  - latency at 100 msg/s per stream: k=2 needs 10 ms, k=4 needs 30 ms, k=8 needs 70 ms, k=16 needs 150 ms, k=29 needs 280 ms
  - latency at 1000 msg/s per stream: k=2 needs 1 ms, k=4 needs 3 ms, k=8 needs 7 ms, k=16 needs 15 ms, k=29 needs 28 ms
  - §10.7 adds that MaxDelay to every bound, in both directions.
- **100 B payload**
  - cost: transport 4712 ns of 5562 ns per message = 84.7% — majority: true
    (the core's half is 420 ns, so 420 + 4712 = 5131 ns is the
    same message reached apart)
  - wire: 47 of 147 B per frame are header = 32% — majority: false
    (of which 28 B/frame is recoverable by batching, the IP/UDP header;
    the 19 B frame header is not, §4.1)
  - latency at 100 msg/s per stream: k=2 needs 10 ms, k=4 needs 30 ms, k=8 needs 70 ms, k=10 needs 90 ms
  - latency at 1000 msg/s per stream: k=2 needs 1 ms, k=4 needs 3 ms, k=8 needs 7 ms, k=10 needs 9 ms
  - §10.7 adds that MaxDelay to every bound, in both directions.


## What it says

**The transport is the cost of a message, and the syscall is the transport.**
At 22 B, `Handle` is 4727 ns of the 5442 ns a `SendMsg` takes, and 3936 ns of
that `Handle` is the bare `write`. The core's own half — codec marshal, frame,
seq, flow-control check — is 376 ns. Marshalling the `Envelop` is 198 ns.
Nothing else is worth looking at.

**Batching removes almost all of it.** At 22 B, 29 frames fit one datagram and
cost 306 ns/frame against 4708 ns unbatched, a 93.5% saving. At 100 B, 10
frames fit and save 86.4%.

**And none of that matters at the rate this library is for.** A share is not
an amount. 87% of 5.4 µs is still 5.4 µs, and at the 200 Hz one stream of
`examples/udp-sensor` runs at, the whole transport costs about 0.1% of one
core. Batching it away saves a tenth of a percent of a core, in exchange for
`MaxDelay` added to every bound in both directions (§10.7). At that rate a
5 ms window collects two frames.

The rates where the saving is real are the ones the grid's right-hand edge
shows: 16 streams at 10k msg/s is 160k messages a second, 75.6% of a core
unbatched and 4.9% batched. That is a different workload from the one
`docs/TODO.md` §1 names.

**On the wire the picture is the same shape.** At 22 B a frame is 69 B on the
wire and 47 of them are header, but only the 28 B IPv4+UDP part is
recoverable: the 19 B `Frame` header is per frame and rides inside the
envelop either way (§4.1). Batching 29 of them takes the frame from 69 B to
42 B, 39% off. At 100 B it is 17% off, and at 1 KiB no two frames fit one
1200 B datagram at all, so batching cannot apply.

## The rule

Issue #5 wrote the decision rule down before the run so the result could not
be argued afterwards:

> batching is a go only if, at ≤ 1k msg/s and ≤ 100 B, the transport share is
> a majority of per-message cost or of wire bytes, and the `MaxDelay` needed
> to reach a `k` that recovers most of it is below the reading's useful life.

The first clause holds: 86.9% of per-message cost at 22 B, and 68.1% of wire
bytes. The second is the one to weigh, and it is not a measurement — how long
a reading stays useful is the operator's number. What the run supplies is the
price list: at 200 Hz, `k = 2` costs 5 ms and saves half the transport;
`k = 8`, which saves 84%, costs 35 ms; `k = 29`, which saves 93.5%, costs
140 ms. At 1000 msg/s the same k's cost 1 ms, 7 ms and 28 ms.

So the question the rule leaves is narrow and answerable: **is 35 ms of added
age acceptable on a reading sampled every 5 ms, to recover 0.4% of one core?**
