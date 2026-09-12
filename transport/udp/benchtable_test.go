package udp_test

// The table issue #5 asks for, derived from the benchmarks next door: for each
// (payload size × message rate × concurrent streams) the CPU share of one core
// the transport costs, the on-wire header share, the k a MaxDelay of 1/5/20 ms
// reaches at that rate, and what Batch/k says that k saves. The latency column
// is just MaxDelay — that is the price, by §10.7.
//
// It reads the numbers instead of carrying them: the run that decides this
// happens on a quiet machine, not here, and a table with numbers baked into it
// would be a second source of truth to keep honest. Feed it the raw output of
// `go test -bench`:
//
//	cd transport/udp
//	go test -run '^$' -bench . -benchmem -count=10 . | tee bench.txt
//	DRPC_BENCH_OUT=bench.txt go test -run TestBenchTable -v .
//
// Repeats (-count) are averaged and the sample count is printed; benchstat is
// not a dependency here and its output is not the input — raw lines are.
//
// The test skips without that variable: it is a report generator that happens
// to live in the test binary, so it stays compiled and cannot rot, but it runs
// only when someone asks for the table.
//
// Decision rule (issue #5, written down before the run — bench_test.go quotes
// it in full): batching is a go only if, at ≤ 1k msg/s and ≤ 100 B, the
// transport share is a majority of per-message cost or of wire bytes, and the
// MaxDelay needed to reach a k that recovers most of it is below the reading's
// useful life. The "Rule check" section below evaluates the two halves that
// are arithmetic and prints the third as the question it is: how long a
// reading stays useful is the operator's number, not a measurement.
//
// A column the run did not measure prints "—", never 0: the numbers here are
// read out of a file someone produced, and a 0 ns syscall would say the
// adapter is the whole cost — the inverse of the result.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/lesomnus/grpc-dgram/transport/udp"
)

// benchOutEnv names the file holding raw `go test -bench` output.
const benchOutEnv = "DRPC_BENCH_OUT"

// The grid is issue #5's, not a measurement: rates are per stream (the example
// runs one stream at 200 Hz, examples/udp-sensor/main.go), streams multiply
// them, delays are the MaxDelay a Coalescer would hold frames for.
var (
	tableRates   = []float64{100, 1e3, 1e4} // msg/s, per stream
	tableStreams = []int{1, 16}
	tableDelays  = []float64{1, 5, 20} // ms
)

// ruleMaxRate and rulePayload are the corner the decision rule is evaluated
// in: "at ≤ 1k msg/s and ≤ 100 B".
const (
	ruleMaxRate = 1e3
	rulePayload = 100
)

// benchRow is one benchmark line, summed over repeats.
type benchRow struct {
	samples int
	sums    map[string]float64
}

func (r *benchRow) mean(unit string) (float64, bool) {
	v, ok := r.sums[unit]
	if !ok || r.samples == 0 {
		return 0, false
	}
	return v / float64(r.samples), true
}

func (r *benchRow) must(t *testing.T, unit string) float64 {
	t.Helper()
	v, ok := r.mean(unit)
	if !ok {
		t.Fatalf("no %q in the benchmark output", unit)
	}
	return v
}

// procSuffix is the -N GOMAXPROCS tag the testing package appends to a name.
var procSuffix = regexp.MustCompile(`-\d+$`)

// parseBench reads raw `go test -bench` lines: a name, an iteration count,
// then (value, unit) pairs. Anything else in the file — build output,
// benchstat tables, PASS lines — is skipped rather than guessed at.
func parseBench(r io.Reader) map[string]*benchRow {
	rows := map[string]*benchRow{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.HasPrefix(f[0], "Benchmark") {
			continue
		}
		if _, err := strconv.Atoi(f[1]); err != nil {
			continue
		}
		name := procSuffix.ReplaceAllString(strings.TrimPrefix(f[0], "Benchmark"), "")
		row := rows[name]
		if row == nil {
			row = &benchRow{sums: map[string]float64{}}
			rows[name] = row
		}
		row.samples++
		for i := 2; i+1 < len(f); i += 2 {
			v, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				continue
			}
			row.sums[f[i+1]] += v
		}
	}
	return rows
}

// batch is one Batch/<size>/k=<n> pair, per frame on both sides. Each side is
// its own row in the input, so each carries whether the run produced it.
type batch struct {
	k          int
	batchedNs  float64
	batchedOK  bool
	unbatchNs  float64
	unbatchOK  bool
	wireBFrame float64
}

// size is everything the table needs about one payload size.
type size struct {
	name string
	// handle is the only row collect insists on: without it there is no
	// table. Everything else carries whether the run measured it.
	payload      float64 // derived: wire − frame header − IP/UDP
	handle       float64 // ns/op
	marshal      float64 // ns/op
	marshalOK    bool
	rawWrite     float64 // ns/op
	rawWriteOK   bool
	core         float64 // ns/op, the core's half of a sent message
	coreOK       bool
	clientSend   float64 // ns/op, one message sent through this adapter
	clientSendOK bool
	allocs       float64
	wireB        float64 // per frame, unbatched
	hdrB         float64 // per frame
	ipudpB       float64 // per frame, unbatched
	samples      int
	batches      []batch
}

// kMax is the largest k the run measured, i.e. the most frames that fit the
// 1200 B budget (udp.DefaultMaxMessageSize, §4.4).
func (s size) kMax() int {
	if len(s.batches) == 0 {
		return 1
	}
	return s.batches[len(s.batches)-1].k
}

// batchAt is the measured batch at the largest k not above want; ok is false
// when the window collects fewer frames than the smallest k measured, or when
// the run has no batched row for any of them.
func (s size) batchAt(want int) (batch, bool) {
	out, ok := batch{}, false
	for _, b := range s.batches {
		if b.k <= want && b.batchedOK {
			out, ok = b, true
		}
	}
	return out, ok
}

var kInName = regexp.MustCompile(`^Batch/([^/]+)/k=(\d+)/(batched|unbatched)$`)

// collect turns parsed rows into one record per payload size. Payload bytes
// are derived from the reported byte metrics (wire = payload + frame header +
// IP/UDP) rather than from the sub-benchmark's name, so the table is built out
// of measured quantities even where a name would have been easier.
func collect(t *testing.T, rows map[string]*benchRow) []size {
	t.Helper()

	byName := map[string]*size{}
	for name, row := range rows {
		rest, ok := strings.CutPrefix(name, "Handle/")
		if !ok {
			continue
		}
		s := &size{
			name:    rest,
			handle:  row.must(t, "ns/op"),
			wireB:   row.must(t, "wireB/frame"),
			hdrB:    row.must(t, "hdrB/frame"),
			ipudpB:  row.must(t, "ipudpB/frame"),
			samples: row.samples,
		}
		s.payload = s.wireB - s.hdrB - s.ipudpB
		if v, ok := row.mean("allocs/op"); ok {
			s.allocs = v
		}
		byName[rest] = s
	}
	if len(byName) == 0 {
		t.Fatalf("no Handle/<size> rows in the benchmark output: was it run with -bench Handle or -bench . ?")
	}

	for name, row := range rows {
		if rest, ok := strings.CutPrefix(name, "Marshal/"); ok {
			if s := byName[rest]; s != nil {
				s.marshal, s.marshalOK = row.must(t, "ns/op"), true
			}
			continue
		}
		if rest, ok := strings.CutPrefix(name, "RawWrite/"); ok {
			if s := byName[rest]; s != nil {
				s.rawWrite, s.rawWriteOK = row.must(t, "ns/op"), true
			}
			continue
		}
		if rest, ok := strings.CutPrefix(name, "CoreSend/"); ok {
			if s := byName[rest]; s != nil {
				s.core, s.coreOK = row.must(t, "ns/op"), true
			}
			continue
		}
		if rest, ok := strings.CutPrefix(name, "ClientSend/"); ok {
			if s := byName[rest]; s != nil {
				s.clientSend, s.clientSendOK = row.must(t, "ns/op"), true
			}
			continue
		}
		m := kInName.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		s := byName[m[1]]
		if s == nil {
			continue
		}
		k, _ := strconv.Atoi(m[2])
		i := sort.Search(len(s.batches), func(i int) bool { return s.batches[i].k >= k })
		if i == len(s.batches) || s.batches[i].k != k {
			s.batches = append(s.batches, batch{})
			copy(s.batches[i+1:], s.batches[i:])
			s.batches[i] = batch{k: k}
		}
		if m[3] == "batched" {
			s.batches[i].batchedNs = row.must(t, "ns/frame")
			s.batches[i].wireBFrame = row.must(t, "wireB/frame")
			s.batches[i].batchedOK = true
		} else {
			s.batches[i].unbatchNs = row.must(t, "ns/frame")
			s.batches[i].unbatchOK = true
		}
	}

	out := make([]size, 0, len(byName))
	for _, s := range byName {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].payload < out[j].payload })
	return out
}

// cpuShare is the fraction of one core a rate of msgs/s costs at perMsg ns per
// message: the whole arithmetic of the CPU column.
func cpuShare(perMsg, rate float64) float64 { return perMsg * rate / 1e9 }

// window is how many frames a MaxDelay of d ms collects at rate msg/s: the one
// that opens the window, plus the rate x d that arrive while it is held. A
// window shorter than the gap between messages therefore collects k = 1, which
// is what "no batching at this rate" looks like.
func window(rate, d float64) int { return 1 + int(rate*d/1e3) }

// maxDelayFor inverts it: the MaxDelay, in ms, a batch of k needs at this rate.
func maxDelayFor(k int, rate float64) float64 { return float64(k-1) / rate * 1e3 }

func pct(v float64) string { return fmt.Sprintf("%.3g%%", v*100) }

// ns formats a nanosecond figure the run measured, or "—" for one it did not.
// Never 0: this file reads numbers out of whatever someone ran, and a 0 ns
// syscall in the raw-write column would make the adapter the whole cost of a
// send — the inverse of the result.
func ns(v float64, ok bool) string {
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.0f", v)
}

func writeTable(w io.Writer, sizes []size) {
	fmt.Fprintf(w, "# Envelop batching: the measurement (issue #5)\n\n")
	fmt.Fprintf(w, "Derived from `%s`. Rates are per stream; `msg/s` is the total across streams.\n", os.Getenv(benchOutEnv))
	fmt.Fprintf(w, "One frame is one datagram today, so the send path is charged once per message.\n\n")

	fmt.Fprintf(w, "## Where one message goes\n\n")
	fmt.Fprintf(w, "| payload | marshal ns | raw write ns | Handle ns | Handle − raw write | allocs/op | wire B/frame | header share of wire | samples |\n")
	fmt.Fprintf(w, "|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, s := range sizes {
		fmt.Fprintf(w, "| %g B | %s | %s | %.0f | %s | %.0f | %.0f | %s | %d |\n",
			s.payload, ns(s.marshal, s.marshalOK), ns(s.rawWrite, s.rawWriteOK), s.handle,
			ns(s.handle-s.rawWrite, s.rawWriteOK), s.allocs,
			s.wireB, pct((s.wireB-s.payload)/s.wireB), s.samples)
	}
	fmt.Fprintf(w, "\nThe syscall is `raw write`; `Handle − raw write` is everything the adapter adds\n")
	fmt.Fprintf(w, "on top of it: the marshal, and the envelop and one-frame slice it allocates\n")
	fmt.Fprintf(w, "around every frame. The frame itself is the core's (`core send`, below).\n")
	fmt.Fprintf(w, "Header share counts both the per-frame Frame header and the per-datagram 28 B\n")
	fmt.Fprintf(w, "IPv4+UDP header; only the second is amortised by batching.\n")
	fmt.Fprintf(w, "A server→client data frame carries peer_epoch too: +5 B per frame (§6.1).\n\n")

	fmt.Fprintf(w, "## What one message costs to send\n\n")
	fmt.Fprintf(w, "| payload | core send ns | transport ns | core + transport | client send ns | transport share |\n")
	fmt.Fprintf(w, "|--:|--:|--:|--:|--:|--:|\n")
	for _, s := range sizes {
		sum, share := "—", "—"
		if s.coreOK {
			sum = fmt.Sprintf("%.0f", s.core+s.handle)
		}
		if s.clientSendOK {
			share = pct(s.handle / s.clientSend)
		}
		fmt.Fprintf(w, "| %g B | %s | %.0f | %s | %s | %s |\n",
			s.payload, ns(s.core, s.coreOK), s.handle, sum,
			ns(s.clientSend, s.clientSendOK), share)
	}
	fmt.Fprintf(w, "\n`core send` is one SendMsg with an adapter that does nothing — the codec\n")
	fmt.Fprintf(w, "marshal, the frame, the seq, the flow-control check; `transport` is the Handle\n")
	fmt.Fprintf(w, "column above; `client send` is that same SendMsg through this adapter. The\n")
	fmt.Fprintf(w, "last two columns are one quantity reached two ways — the sum is there to be\n")
	fmt.Fprintf(w, "checked against the measurement, which on a loopback socket holds only to\n")
	fmt.Fprintf(w, "within the spread of the raw rows. The share divides by the measured one.\n\n")

	fmt.Fprintf(w, "## What the transport costs at rate\n\n")
	fmt.Fprintf(w, "| payload | rate/stream | streams | msg/s | CPU of one core | wire B/s | header share of wire |\n")
	fmt.Fprintf(w, "|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, s := range sizes {
		for _, r := range tableRates {
			for _, n := range tableStreams {
				total := r * float64(n)
				fmt.Fprintf(w, "| %g B | %g | %d | %g | %s | %.0f | %s |\n",
					s.payload, r, n, total, pct(cpuShare(s.handle, total)),
					s.wireB*total, pct((s.wireB-s.payload)/s.wireB))
			}
		}
	}
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "## What a MaxDelay buys\n\n")
	fmt.Fprintf(w, "`k` is what the window collects — the frame that opens it plus the\n")
	fmt.Fprintf(w, "rate × MaxDelay that arrive while it is held — capped by the %d B budget.\n", udp.DefaultMaxMessageSize)
	fmt.Fprintf(w, "`k/stream` batches within one call (the narrow option of TODO §1); `k/all`\n")
	fmt.Fprintf(w, "batches across the streams of one peer (the wide one — it couples their loss,\n")
	fmt.Fprintf(w, "§4.1). A server cannot batch across peers at all: one datagram, one address.\n")
	fmt.Fprintf(w, "The measured column is the largest k the run has a number for, at or below `k/all`.\n\n")
	for _, s := range sizes {
		if len(s.batches) == 0 {
			fmt.Fprintf(w, "### %g B payload\n\n", s.payload)
			if two := 2 * (s.payload + s.hdrB); two > float64(udp.DefaultMaxMessageSize) {
				fmt.Fprintf(w, "No k > 1 fits: two frames are %.0f B of Envelop, over the %d B budget,\n",
					two, udp.DefaultMaxMessageSize)
				fmt.Fprintf(w, "so batching cannot apply at this size.\n\n")
			} else {
				fmt.Fprintf(w, "No `Batch/%s` rows in the input, though two frames would fit the %d B\n",
					s.name, udp.DefaultMaxMessageSize)
				fmt.Fprintf(w, "budget: missing data, not a result.\n\n")
			}
			continue
		}
		fmt.Fprintf(w, "### %g B payload (k ≤ %d fits the %d B budget)\n\n", s.payload, s.kMax(), udp.DefaultMaxMessageSize)
		fmt.Fprintf(w, "| rate/stream | streams | MaxDelay | k/stream | k/all | k measured | ns/frame | vs unbatched | CPU of one core | wire B/frame | vs unbatched |\n")
		fmt.Fprintf(w, "|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
		for _, r := range tableRates {
			for _, n := range tableStreams {
				total := r * float64(n)
				for _, d := range tableDelays {
					// Both are capped: a batch is one Envelop in one
					// datagram either way (§4.1, §4.4), so a window that
					// collects more frames than fit cannot send them as one.
					kStream := window(r, d)
					if kStream > s.kMax() {
						kStream = s.kMax()
					}
					kAll := window(total, d)
					if kAll > s.kMax() {
						kAll = s.kMax()
					}
					b, ok := s.batchAt(kAll)
					if !ok {
						fmt.Fprintf(w, "| %g | %d | %g ms | %d | %d | — | — | — | %s | %.0f | — |\n",
							r, n, d, kStream, kAll, pct(cpuShare(s.handle, total)), s.wireB)
						continue
					}
					vs := "—"
					if b.unbatchOK {
						vs = pct((b.unbatchNs - b.batchedNs) / b.unbatchNs)
					}
					fmt.Fprintf(w, "| %g | %d | %g ms | %d | %d | %d | %.0f | %s | %s | %.0f | %s |\n",
						r, n, d, kStream, kAll, b.k, b.batchedNs, vs,
						pct(cpuShare(b.batchedNs, total)), b.wireBFrame,
						pct((s.wireB-b.wireBFrame)/s.wireB))
				}
			}
		}
		fmt.Fprintf(w, "\n")
	}

	fmt.Fprintf(w, "## Rule check\n\n")
	fmt.Fprintf(w, "Issue #5, before the run: batching is a go only if, at ≤ %g msg/s and ≤ %d B,\n", ruleMaxRate, rulePayload)
	fmt.Fprintf(w, "the transport share is a majority of per-message cost **or** of wire bytes, and\n")
	fmt.Fprintf(w, "the MaxDelay needed to reach a k that recovers most of it is below the reading's\n")
	fmt.Fprintf(w, "useful life. The go/no-go is the conjunction; the last clause is not a\n")
	fmt.Fprintf(w, "measurement — how long a reading stays useful is the operator's number.\n\n")
	fmt.Fprintf(w, "Per-message cost is `ClientSend`: one SendMsg through this adapter, measured\n")
	fmt.Fprintf(w, "whole rather than summed across two paths, so the transport share is `Handle`\n")
	fmt.Fprintf(w, "over it. The core's half (`CoreSend`) is printed beside it because core +\n")
	fmt.Fprintf(w, "transport is that same message reached apart — how closely the two agree is\n")
	fmt.Fprintf(w, "itself a check on the run.\n\n")
	for _, s := range sizes {
		if s.payload > rulePayload {
			continue
		}
		fmt.Fprintf(w, "- **%g B payload**\n", s.payload)
		switch {
		case s.clientSendOK:
			fmt.Fprintf(w, "  - cost: transport %.0f ns of %.0f ns per message = %s — majority: %v\n",
				s.handle, s.clientSend, pct(s.handle/s.clientSend), s.handle/s.clientSend > 0.5)
			if s.coreOK {
				fmt.Fprintf(w, "    (the core's half is %.0f ns, so %.0f + %.0f = %.0f ns is the\n",
					s.core, s.core, s.handle, s.core+s.handle)
				fmt.Fprintf(w, "    same message reached apart)\n")
			}
		case s.coreOK:
			fmt.Fprintf(w, "  - cost: transport %.0f ns against a core half of %.0f ns = %s of their\n",
				s.handle, s.core, pct(s.handle/(s.handle+s.core)))
			fmt.Fprintf(w, "    sum — majority: %v (no `ClientSend/%s` row, so this is the sum, not\n",
				s.handle > s.core, s.name)
			fmt.Fprintf(w, "    a measured message)\n")
		default:
			fmt.Fprintf(w, "  - cost: no `ClientSend/%s` or `CoreSend/%s` row in the input, so the\n", s.name, s.name)
			fmt.Fprintf(w, "    cost half is left open\n")
		}
		wireShare := (s.wireB - s.payload) / s.wireB
		fmt.Fprintf(w, "  - wire: %.0f of %.0f B per frame are header = %s — majority: %v\n",
			s.wireB-s.payload, s.wireB, pct(wireShare), wireShare > 0.5)
		fmt.Fprintf(w, "    (of which %.0f B/frame is recoverable by batching, the IP/UDP header;\n", s.ipudpB)
		fmt.Fprintf(w, "    the %.0f B frame header is not, §4.1)\n", s.hdrB)
		for _, r := range tableRates {
			if r > ruleMaxRate {
				continue
			}
			fmt.Fprintf(w, "  - latency at %g msg/s per stream: ", r)
			parts := []string{}
			for _, b := range s.batches {
				parts = append(parts, fmt.Sprintf("k=%d needs %.0f ms", b.k, maxDelayFor(b.k, r)))
			}
			fmt.Fprintf(w, "%s\n", strings.Join(parts, ", "))
		}
		fmt.Fprintf(w, "  - §10.7 adds that MaxDelay to every bound, in both directions.\n")
	}
	fmt.Fprintf(w, "\n")
}

// TestBenchTable prints the table. It is skipped unless asked for; see the
// file comment for the two commands that produce and consume the numbers.
func TestBenchTable(t *testing.T) {
	path := os.Getenv(benchOutEnv)
	if path == "" {
		t.Skipf("set %s=<file of raw `go test -bench` output> to print the issue #5 table", benchOutEnv)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rows := parseBench(f)
	if len(rows) == 0 {
		t.Fatalf("%s: no benchmark lines", path)
	}
	sizes := collect(t, rows)

	out := &strings.Builder{}
	writeTable(out, sizes)
	fmt.Fprint(os.Stdout, out.String())
}
