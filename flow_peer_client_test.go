package drpc_test

// flow_peer_client_test.go pins the CLIENT half of the connection window
// (PROTOCOL.md §4.2.1, reliable mode only) against a scripted server on the
// far end of the Conn's tx, so every rule can be observed without a Go server
// that grants on sid 0 yet:
//
//   - a sender assumes W_conn per Conn until the server's advertisement and
//     parks — on the connection window, not the stream window — once it has
//     spent what it was advertised, bounded by T_stall;
//   - only a sid-0 WINDOW from the server incarnation the Conn is locked to
//     credits it; a foreign one is dropped in silence, never RESET;
//   - the first H or T from a server incarnation advertises its connection
//     window — a creation ack, a unary T, on a live call or a released one —
//     the rest are ignored, and an H that carries none turns it off;
//   - a new server incarnation starts the sender over (§10.6), and its own
//     first H advertises again;
//   - every OPEN carries the Conn's MaxPeerWindow as conn_window;
//   - an overrun fails the offending call INTERNAL and nothing else;
//   - every data frame received returns exactly one credit on sid 0:
//     consumed, discarded with its call, or never buffered;
//   - Conn.Close unparks a sender waiting on connection credit.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/internal/x"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// wConnTest is W_conn, the connection window a client assumes toward a
// server incarnation until its advertisement arrives (PROTOCOL.md §4.2.1,
// Appendix B); also the MaxPeerWindow floor, and so the default advertised.
const wConnTest = 1024

// peerSrv is a scripted server on the far end of a Conn's tx. It records
// everything the client sends, answers every OPEN synchronously — a
// creation-ack H for a streaming call, a T for a unary one, each carrying
// its advertisements: the per-stream window on the H, the connection window
// on both (§4.2.1) — and lets a test inject sequenced data frames or sid-0
// grants under whichever server incarnation it currently is.
type peerSrv struct {
	conn   *drpc.Conn
	window uint32 // advertised on every H

	mu          sync.Mutex
	connWindow  uint32 // advertised on every H and T; 0 = none
	epoch       uint32 // the incarnation answering now; a restart changes it
	clientEpoch uint32 // learned from the first OPEN
	muted       bool   // answer no OPEN: the test injects the ack itself
	tx          []*drpc.Frame
	seq         map[uint32]uint32 // last server seq per sid
}

// newPeerSrv builds a fake advertising window per stream and W_conn — the
// default, so that the client's assumption and the advertisement agree — as
// its connection window; advertise changes the latter.
func newPeerSrv(epoch, window uint32) *peerSrv {
	return &peerSrv{epoch: epoch, window: window, connWindow: wConnTest, seq: map[uint32]uint32{}}
}

func (p *peerSrv) Handle(ctx context.Context, f *drpc.Frame) error {
	p.mu.Lock()
	p.tx = append(p.tx, proto.CloneOf(f))
	open := f.GetFlags()&drpc.FlagOpen != 0
	if open {
		p.clientEpoch = f.GetEpoch()
	}
	muted, connWindow := p.muted, p.connWindow
	p.mu.Unlock()
	if !open || muted {
		return nil
	}
	if f.GetMethod() == echo.EchoService_Once_FullMethodName {
		// A unary call ends in its T: no H, no per-stream window (§8) — but
		// the connection window, like every T (§4.2.1).
		t := p.frame(f.GetSid(), drpc.FlagClose)
		t.SetCode(uint32(codes.OK))
		t.SetConnWindow(connWindow)
		data, _ := proto.Marshal(&echo.EchoResponse{})
		t.SetPayload(data)
		return p.conn.Handle(ctx, t)
	}
	// The creation ack carries both advertisements (§4.2.1, §8).
	h := p.frame(f.GetSid(), 0)
	h.SetWindow(p.window)
	h.SetConnWindow(connWindow)
	return p.conn.Handle(ctx, h)
}

// advertise sets the connection window the fake carries on every H and T
// from now on; 0 omits it — a server that does no connection flow control.
func (p *peerSrv) advertise(connWindow uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.connWindow = connWindow
}

// frame builds the next sequenced server frame on sid under the current
// incarnation, echoing the client one as every server frame must (§6.1).
func (p *peerSrv) frame(sid, flags uint32) *drpc.Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq[sid]++
	f := &drpc.Frame{}
	f.SetEpoch(p.epoch)
	f.SetPeerEpoch(p.clientEpoch)
	f.SetSid(sid)
	f.SetSeq(p.seq[sid])
	f.SetFlags(flags)
	return f
}

// data injects one data frame on sid.
func (p *peerSrv) data(t *testing.T, sid uint32) {
	t.Helper()
	f := p.frame(sid, 0)
	payload, _ := proto.Marshal(&echo.EchoResponse{})
	f.SetPayload(payload)
	x.NoError(t, p.conn.Handle(context.Background(), f))
}

// grant injects a connection grant under the given server incarnation,
// addressed at the given client one (§4.2.1).
func (p *peerSrv) grant(t *testing.T, epoch, peerEpoch, n uint32) {
	t.Helper()
	f := &drpc.Frame{}
	f.SetEpoch(epoch)
	f.SetPeerEpoch(peerEpoch)
	f.SetFlags(drpc.FlagWindow)
	f.SetWindow(n)
	x.NoError(t, p.conn.Handle(context.Background(), f))
}

func (p *peerSrv) restart(epoch uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.epoch = epoch
}

// mute stops the automatic creation ack, so a test can release the call
// before answering it by hand.
func (p *peerSrv) mute(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.muted = on
}

func (p *peerSrv) client() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clientEpoch
}

func (p *peerSrv) txFrames() []*drpc.Frame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*drpc.Frame(nil), p.tx...)
}

// lastOpen returns the sid of the most recent OPEN the client sent.
func (p *peerSrv) lastOpen() uint32 {
	frames := p.txFrames()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].GetFlags()&drpc.FlagOpen != 0 {
			return frames[i].GetSid()
		}
	}
	return 0
}

// isPeerGrant reports whether f is a connection grant: WINDOW on sid 0, seq
// 0, no payload, epoch = the sender's own (§4.2.1, §7).
func isPeerGrant(f *drpc.Frame) bool { return isWindowFrame(f) && f.GetSid() == 0 }

func peerGrants(frames []*drpc.Frame) (n int, total uint32) {
	for _, f := range frames {
		if isPeerGrant(f) {
			n++
			total += f.GetWindow()
		}
	}
	return n, total
}

func countMatch(frames []*drpc.Frame, match func(*drpc.Frame) bool) int {
	n := 0
	for _, f := range frames {
		if match(f) {
			n++
		}
	}
	return n
}

// clientFixture wires a Conn to a peerSrv. srvWindow is the per-stream window
// the fake advertises on its H — large, so that the STREAM window never binds
// and every park below is the connection window's. Its connection window is
// W_conn unless the test says otherwise (peerSrv.advertise).
func clientFixture(t *testing.T, srvEpoch, srvWindow uint32, events *flowEvents, opts ...drpc.ConnOption) (*peerSrv, *drpc.Conn, echo.EchoServiceClient) {
	t.Helper()
	srv := newPeerSrv(srvEpoch, srvWindow)
	opts = append([]drpc.ConnOption{
		drpc.WithReliable(true),
		drpc.WithTiming(drpc.Timing{Stall: 2 * time.Second}),
	}, opts...)
	if events != nil {
		opts = append(opts, drpc.WithProtocolStats(events))
	}
	conn := drpc.NewConn(srv, opts...)
	srv.conn = conn
	return srv, conn, echo.NewEchoServiceClient(conn)
}

const srvEpochA, srvEpochB = uint32(0xA11CE), uint32(0xB0B)

func sendN(t *testing.T, stream echo.EchoService_BuffClient, n int) {
	t.Helper()
	msg := echo.EchoRequest_builder{Message: "m"}.Build()
	for i := range n {
		if err := stream.Send(msg); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// §4.2.1 initial window and sending: until the server's advertisement the
// client paces itself by W_conn; from it, by what was advertised. Spent
// across the Conn, the sender parks on the CONNECTION window — its stream
// window still has credit — and fails UNAVAILABLE at T_stall naming that
// window. One budget, not two.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Initial window: "Until the advertisement arrives a sender paces
// itself by W_conn = 1024 messages ... Only the client ever needs it — it may
// stream before the first H".
func TestPeerWindow_ClientAssumesWConnUntilAdvertised(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)
		srv.mute(true) // no H ever: nothing advertised, on either window

		// W_conn/W_init calls each fill exactly their own assumed stream
		// window: the whole assumed connection window is spent and no
		// stream window ever was.
		for range wConnTest / int(wInitTest) {
			s, err := client.Buff(t.Context())
			x.NoError(t, err)
			sendN(t, s, int(wInitTest))
		}
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "W_conn messages go out unparked, on the assumption")
		x.Equal(t, 0, events.count(drpc.EventFlowStall))

		// The 1025th, on a fresh call with its own assumed window: it parks
		// on the connection window alone, and fails there at T_stall.
		extra, err := client.Buff(t.Context())
		x.NoError(t, err)
		start := time.Now()
		err = extra.Send(echo.EchoRequest_builder{Message: "m"}.Build())
		x.Equal(t, codes.Unavailable, status.Code(err))
		x.True(t, strings.Contains(status.Convert(err).Message(), "connection credit"),
			"the error must name the window that starved it, got: ", err)
		x.Equal(t, 2*time.Second, time.Since(start), "the park ends exactly at T_stall")
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))
		x.Equal(t, 0, events.count(drpc.EventFlowStall), "the stream window had credit")
	})
}

// Pins §4.2.1 Initial window: "The advertisement is authoritative and
// replaces the assumption" — the client honours the server's advertised
// window exactly, at the floor and above it.
func TestPeerWindow_ClientParksAtTheAdvertisedWindow(t *testing.T) {
	t.Run("W_conn: the park ends at T_stall naming the window", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			_, conn, client := clientFixture(t, srvEpochA, 4096, events) // advertises W_conn
			defer conn.Close(nil)

			stream, err := client.Buff(t.Context())
			x.NoError(t, err)
			sendN(t, stream, wConnTest)
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "W_conn messages go out unparked")

			start := time.Now()
			err = stream.Send(echo.EchoRequest_builder{Message: "m"}.Build())
			x.Equal(t, codes.Unavailable, status.Code(err))
			x.True(t, strings.Contains(status.Convert(err).Message(), "connection credit"),
				"the error must name the window that starved it, got: ", err)
			x.Equal(t, 2*time.Second, time.Since(start), "the park ends exactly at T_stall")
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))
			x.Equal(t, 0, events.count(drpc.EventFlowStall), "the stream window had credit")
		})
	})
	t.Run("2048: exactly that many go out, the 2049th parks until a sid-0 grant", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			srv, conn, client := clientFixture(t, srvEpochA, 4096, events)
			defer conn.Close(nil)
			srv.advertise(2048)

			stream, err := client.Buff(t.Context())
			x.NoError(t, err)
			sendN(t, stream, 2048)
			x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "the advertised 2048 go out unparked")
			x.Equal(t, 0, events.count(drpc.EventFlowStall))

			done := make(chan error, 1)
			go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
			synctest.Wait()
			x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "the 2049th parks: the window is exact")
			x.Equal(t, 2048, countMatch(srv.txFrames(), isDataFrame))

			srv.grant(t, srvEpochA, srv.client(), 1)
			x.NoError(t, <-done)
			x.Equal(t, 1, events.count(drpc.EventPeerFlowResume))
			x.Equal(t, 2049, countMatch(srv.txFrames(), isDataFrame))
		})
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §9.1 grants: a sid-0 WINDOW credits the connection window only
// when it comes from the server incarnation the Conn is locked to and names
// this client incarnation. Anything else is dropped in silence — no RESET,
// no credit.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Grants: a receiver applies a sid-0 WINDOW only "when it holds a
// connection sender for the incarnation the frame names — the client when the
// frame's epoch is the server incarnation the Conn is locked to and its
// peer_epoch the Conn's own epoch".
func TestPeerWindow_Sid0GrantGate(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, wConnTest)

		done := make(chan error, 1)
		go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "the next send parks")

		parked := func(why string) {
			t.Helper()
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("%s: the send must stay parked, returned %v", why, err)
			default:
			}
		}
		me := srv.client()

		// Another client incarnation's grant: not ours, not answered (§9.1).
		srv.grant(t, srvEpochA, me+1, 1)
		parked("foreign peer_epoch")
		x.Equal(t, 0, countMatch(srv.txFrames(), isResetFrame), "a foreign sid-0 grant draws no RESET")

		// Another server incarnation's grant: not the one we count against.
		srv.grant(t, srvEpochB, me, 1)
		parked("foreign server epoch")
		x.Equal(t, 0, countMatch(srv.txFrames(), isResetFrame))

		// The real one.
		srv.grant(t, srvEpochA, me, 1)
		synctest.Wait()
		x.NoError(t, <-done)
		x.Equal(t, 1, events.count(drpc.EventPeerFlowResume))
	})
}

// Pins §4.2.1 Grants: "A receiver applies it only in reliable mode".
func TestPeerWindow_Sid0GrantIgnoredInUnreliableMode(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv := newPeerSrv(srvEpochA, 0)
		conn := drpc.NewConn(srv, drpc.WithReliable(false), drpc.WithTiming(fastTiming), drpc.WithProtocolStats(events))
		srv.conn = conn
		defer conn.Close(nil)

		stream, err := echo.NewEchoServiceClient(conn).Buff(t.Context())
		x.NoError(t, err)
		srv.grant(t, srvEpochA, srv.client(), 1)
		// Well past what a (wrongly) adopted one-message window would allow.
		sendN(t, stream, 40)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall))
		x.Equal(t, 0, countMatch(srv.txFrames(), isResetFrame), "dropped in silence")
		x.Equal(t, 0, countMatch(srv.txFrames(), isPeerGrant), "this side never grants on sid 0 either")
		x.NoError(t, stream.CloseSend())
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 advertisement: the first H or T from a server incarnation carries
// its connection window — a unary T as much as a creation ack — and absent
// turns the window off, whatever the per-stream window said. A grant never
// enables it again.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Advertisement: "whichever arrives first is right and nothing
// needs telling apart — not a creation ack from a SendHeader flush, not a
// streaming call from a unary one ... A peer applies the first advertisement
// it hears from a peer incarnation and ignores the rest".
func TestPeerWindow_UnaryTerminalAdvertises(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)
		srv.advertise(2048)

		// The Conn's first server frame is a unary T: it advertises 2048.
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)

		// A later H says something else: ignored, the first one counts.
		srv.advertise(8192)
		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, 2048)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "2048 go out on the T's advertisement")
		done := make(chan error, 1)
		go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "the 2049th parks: the T's window is exact, the H's ignored")
		conn.Close(nil)
		<-done
	})
}

// Pins §4.2.1 Advertisement: "A conn_window of 0 (absent) ... means 'this
// peer does no connection flow control': the sender's connection window is
// then off toward that incarnation, whatever the per-stream window said" —
// and Grants: "a grant never enables".
func TestPeerWindow_AbsentAdvertisementTurnsTheWindowOff(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		// A per-stream window of W_init and no connection window at all: a
		// server that paces streams but has no connection flow control.
		srv, conn, client := clientFixture(t, srvEpochA, wInitTest, events)
		defer conn.Close(nil)
		srv.advertise(0)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sid, me := srv.lastOpen(), srv.client()

		// The stream window still paces: exactly W_init go out, the 33rd
		// parks on the STREAM window.
		sendN(t, stream, int(wInitTest))
		done := make(chan error, 1)
		go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventFlowStall), "parked on the stream window")
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "the connection window is off")

		// A sid-0 grant never enables it: were the window on at one credit,
		// the sends below would park on it at once. Then per-stream credit
		// for everything below, far past W_conn: no connection park, ever.
		srv.grant(t, srvEpochA, me, 1)
		x.NoError(t, conn.Handle(context.Background(), windowFrame(srvEpochA, me, sid, 4*wConnTest)))
		x.NoError(t, <-done)
		sendN(t, stream, 3*wConnTest)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "off means unlimited, and a grant never enables")
		x.Equal(t, 1, events.count(drpc.EventFlowStall))
		x.Equal(t, 3*wConnTest+int(wInitTest)+1, countMatch(srv.txFrames(), isDataFrame))
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §10.6 restart: a new server incarnation counts from zero, so the
// Conn starts its sender over when a call first accepts a frame from it — a
// cumulative count would park the honest new server's client forever. Grants
// of the dead incarnation are dropped from then on.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Restart: "When it first hears a different server epoch ... the
// Conn MUST start its sender over — assumed at W_conn, unadvertised, nothing
// sent — drop grants naming the old epoch" — and the new incarnation's own
// first H advertises again.
func TestPeerWindow_ReassumeOnNewServerEpoch(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		srv, conn, client := clientFixture(t, srvEpochA, 4096, events)
		defer conn.Close(nil)
		srv.advertise(2048)

		old, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, old, 1500) // past W_conn: A advertised 2048

		srv.restart(srvEpochB)
		fresh, err := client.Buff(t.Context()) // its H comes from the new incarnation
		x.NoError(t, err)
		sendN(t, fresh, 2048)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "a fresh window against the new server, re-advertised at 2048")

		done := make(chan error, 1)
		go func() { done <- fresh.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))

		srv.grant(t, srvEpochA, srv.client(), 1) // the dead incarnation
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("a grant from the old incarnation must be dropped, send returned %v", err)
		default:
		}
		srv.grant(t, srvEpochB, srv.client(), 1)
		x.NoError(t, <-done)
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 advertisement, the client's half: every OPEN — the eager one of a
// streaming call, the piggybacked one of a unary call — carries the Conn's
// MaxPeerWindow as conn_window; nothing rides behind it on sid 0, and a data
// frame carries none.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Advertisement: "the client on every OPEN", and §15: the value
// is MaxPeerWindow, floored at W_conn.
func TestPeerWindow_OpenCarriesTheConnectionWindow(t *testing.T) {
	isOpen := func(f *drpc.Frame) bool { return f.GetFlags()&drpc.FlagOpen != 0 }
	limits := drpc.WithLimits(drpc.Limits{MaxPeerWindow: 2048})

	t.Run("eager OPEN, every one", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, limits)
		defer conn.Close(nil)
		a, err := client.Buff(t.Context())
		x.NoError(t, err)
		_, err = client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, a, 3)

		frames := srv.txFrames()
		x.Equal(t, 2, countMatch(frames, isOpen))
		x.Equal(t, 3, countMatch(frames, isDataFrame))
		x.Equal(t, 5, len(frames), "OPENs and data alone: nothing rides behind an OPEN on sid 0, got ", frames)
		for _, f := range frames {
			if !isOpen(f) {
				x.Equal(t, uint32(0), f.GetConnWindow(), "a data frame carries none")
				continue
			}
			x.Equal(t, uint32(2048), f.GetConnWindow(), "every OPEN advertises MaxPeerWindow")
			x.Equal(t, uint32(32), f.GetWindow(), "beside the per-stream one")
		}
	})
	t.Run("piggybacked OPEN", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, limits)
		defer conn.Close(nil)
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)
		frames := srv.txFrames()
		x.Equal(t, 1, len(frames), "the unary OPEN|CLOSE alone, got ", frames)
		x.True(t, isOpen(frames[0]) && frames[0].GetFlags()&drpc.FlagClose != 0, "got ", frames[0])
		x.Equal(t, uint32(2048), frames[0].GetConnWindow())
	})
	t.Run("the default is W_conn", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil)
		defer conn.Close(nil)
		_, err := client.Buff(t.Context())
		x.NoError(t, err)
		x.Equal(t, uint32(wConnTest), firstMatch(srv.txFrames(), isOpen).GetConnWindow())
	})
	t.Run("at the floor: W_conn, not less", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, drpc.WithLimits(drpc.Limits{MaxPeerWindow: 100}))
		defer conn.Close(nil)
		_, err := client.Buff(t.Context())
		x.NoError(t, err)
		x.Equal(t, uint32(wConnTest), firstMatch(srv.txFrames(), isOpen).GetConnWindow(),
			"MaxPeerWindow is floored at W_conn: a client streams that much on the assumption")
	})
	t.Run("unreliable mode: none", func(t *testing.T) {
		srv := newPeerSrv(srvEpochA, 0)
		conn := drpc.NewConn(srv, drpc.WithReliable(false), drpc.WithTiming(fastTiming), limits)
		srv.conn = conn
		defer conn.Close(nil)
		_, err := echo.NewEchoServiceClient(conn).Buff(t.Context())
		x.NoError(t, err)
		open := firstMatch(srv.txFrames(), isOpen)
		x.True(t, open != nil)
		x.Equal(t, uint32(0), open.GetConnWindow(), "no connection window in unreliable mode (§4.2.1)")
	})
}

// ---------------------------------------------------------------------------
// §4.2 / §4.2.1 / §15 receiving: the server may not have more than
// MaxPeerWindow buffered here across all its calls. The frame that would
// exceed it is never buffered and fails ITS call INTERNAL — the other call is
// untouched, and the failed call's frames come back as credit.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Overrun: "the receiver fails the call it is addressed to with
// INTERNAL ... and returns the frame's credit as never buffered. Never the
// peer".
func TestPeerWindow_OverrunFailsOnlyTheOffendingCall(t *testing.T) {
	// Per-stream windows of 1024, so only the connection window can trip.
	srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, drpc.WithRxBuffer(wConnTest, drpc.DropNewest))
	defer conn.Close(nil)

	a, err := client.Many(t.Context(), &echo.EchoRequest{})
	x.NoError(t, err)
	sidA := srv.lastOpen()
	b, err := client.Many(t.Context(), &echo.EchoRequest{})
	x.NoError(t, err)
	sidB := srv.lastOpen()

	// Nothing is read: exactly the window, split across the two calls.
	for range wConnTest / 2 {
		srv.data(t, sidA)
		srv.data(t, sidB)
	}
	isTerminalFor := func(sid uint32) func(*drpc.Frame) bool {
		return func(f *drpc.Frame) bool { return isTerminal(f) && f.GetSid() == sid }
	}
	x.Equal(t, 0, countMatch(srv.txFrames(), isTerminal), "the window fits")

	// One past it, on b: b fails, a does not.
	srv.data(t, sidB)
	frames := srv.txFrames()
	x.Equal(t, 1, countMatch(frames, isTerminalFor(sidB)), "the offending call aborts")
	x.Equal(t, 0, countMatch(frames, isTerminalFor(sidA)), "the other call is untouched")
	x.Equal(t, 0, countMatch(frames, isResetFrame))

	// b's buffered frames are still delivered, then the overrun surfaces.
	got := 0
	for {
		_, err := b.Recv()
		if err != nil {
			x.Equal(t, codes.Internal, status.Code(err))
			x.True(t, strings.Contains(status.Convert(err).Message(), "connection flow-control window"),
				"the error must name the connection window, got: ", err)
			break
		}
		got++
	}
	x.Equal(t, wConnTest/2, got)

	// Discarding b's frames returned their credit at once: half the window,
	// in one grant — and draining them afterwards returned nothing twice.
	n, total := peerGrants(srv.txFrames())
	x.Equal(t, 1, n)
	x.Equal(t, uint32(wConnTest/2), total)

	// a is live: with b's frames gone there is room for it again.
	srv.data(t, sidA)
	x.Equal(t, 0, countMatch(srv.txFrames(), isTerminalFor(sidA)))
	res, err := a.Recv()
	x.NoError(t, err)
	x.True(t, res != nil)
}

// ---------------------------------------------------------------------------
// §4.2.1 credit return: every reliable-mode data frame received returns one
// credit on sid 0 once it stops occupying a buffer — consumed (batched at half
// the window), or never buffered at all (a frame for a call this side no
// longer has draws a RESET and still returns its credit; an off-shape one is
// dropped and still returns it).
// ---------------------------------------------------------------------------

// Pins §4.2.1 Cadence: "grant once the credit it holds back reaches half its
// window (one small frame per MaxPeerWindow/2 messages)".
func TestPeerWindow_ConsumedFramesGrantOnSid0(t *testing.T) {
	srv, conn, client := clientFixture(t, srvEpochA, 4096, nil, drpc.WithRxBuffer(wConnTest, drpc.DropNewest))
	defer conn.Close(nil)

	stream, err := client.Many(t.Context(), &echo.EchoRequest{})
	x.NoError(t, err)
	sid := srv.lastOpen()
	const injected = 600
	for range injected {
		srv.data(t, sid)
	}
	x.Equal(t, 0, countMatch(srv.txFrames(), isPeerGrant), "nothing consumed, nothing granted")

	for range injected {
		_, err := stream.Recv()
		x.NoError(t, err)
	}
	n, total := peerGrants(srv.txFrames())
	x.Equal(t, 1, n, "batched at half the window: 600 consumed is one grant")
	x.Equal(t, uint32(wConnTest/2), total)
	// The per-stream grant is still there, on the call's own sid (§4.2.1).
	x.Equal(t, 1, countMatch(srv.txFrames(), func(f *drpc.Frame) bool {
		return isWindowFrame(f) && f.GetSid() == sid
	}))
}

// Pins §4.2.1 The receiver's ledger: one credit back "when it was never
// buffered at all — dropped as off-shape (§8) ... or addressed to a call the
// receiver no longer has (RESET-drawn, §9.3)".
func TestPeerWindow_NeverBufferedFramesReturnCredit(t *testing.T) {
	t.Run("unknown sid: RESET and credit", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil)
		defer conn.Close(nil)
		// A finished call, so the Conn is locked to the server incarnation.
		_, err := client.Once(t.Context(), &echo.EchoRequest{})
		x.NoError(t, err)
		sid := srv.lastOpen()

		// Data for the finished call: unknown at a client (no tombstones in
		// reliable mode), each draws a RESET (§9.3, §10.6)...
		for range wConnTest / 2 {
			srv.data(t, sid)
		}
		frames := srv.txFrames()
		x.Equal(t, wConnTest/2, countMatch(frames, isResetFrame))
		// ...and the credit they spent comes back.
		n, total := peerGrants(frames)
		x.Equal(t, 1, n)
		x.Equal(t, uint32(wConnTest/2), total)
	})
	t.Run("off-shape: dropped and credit", func(t *testing.T) {
		srv, conn, client := clientFixture(t, srvEpochA, 4096, nil)
		defer conn.Close(nil)
		stream, err := client.Buff(t.Context()) // client-streaming: no server data frames
		x.NoError(t, err)
		sid := srv.lastOpen()
		for range wConnTest / 2 {
			srv.data(t, sid)
		}
		frames := srv.txFrames()
		x.Equal(t, 0, countMatch(frames, isTerminal), "off-shape frames are dropped, the call lives")
		n, total := peerGrants(frames)
		x.Equal(t, 1, n)
		x.Equal(t, uint32(wConnTest/2), total)
		x.NoError(t, stream.CloseSend())
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 / §4.5: a sender parked on connection credit has no call left to
// wake it through when the Conn closes — Close releases the window itself.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Sending: "Conn.Close and DisconnectPeer (§4.5) release a parked
// connection sender, as a call's end releases a parked stream sender".
func TestPeerWindow_CloseUnparksASenderWaitingOnConnectionCredit(t *testing.T) {
	bubble(t, func(t *testing.T) {
		events := &flowEvents{}
		_, conn, client := clientFixture(t, srvEpochA, 4096, events)

		stream, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, stream, wConnTest)
		done := make(chan error, 1)
		go func() { done <- stream.Send(echo.EchoRequest_builder{Message: "m"}.Build()) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall))

		conn.Close(nil)
		err = <-done
		x.True(t, err == io.EOF || status.Code(err) == codes.Unavailable, "got ", err)
		x.Equal(t, 0, events.count(drpc.EventPeerFlowResume), "no credit ever came")
	})
}

// ---------------------------------------------------------------------------
// §4.2.1 restart / advertisement: the Conn locks to a server incarnation on
// the first sequenced frame it hears from it — on a live call or not — and
// applies the advertisement that frame carries if it is an H or T. When the
// first H lands on a call the client already released it draws a RESET, and a
// Conn that had not applied it would stay at W_conn against a larger window
// for its whole life.
// ---------------------------------------------------------------------------

// Pins §4.2.1 Restart: "a frame that draws a RESET (§9.3) still answers one
// of this Conn's OPENs and names its epoch — and if it is an H or T, carries
// its advertisement, which the Conn applies as it locks".
func TestPeerWindow_AdvertisementLandsOnAReleasedCall(t *testing.T) {
	// Above 2 × W_conn: with the advertisement lost, no cadence of a 4096
	// receiver reaches a sender stuck at 1024 (pending ≥ 2048, or outstanding
	// + pending ≥ 4096) — a forever-park, not a slow one.
	const window = 4 * wConnTest
	msg := echo.EchoRequest_builder{Message: "m"}.Build()

	// lifted sends the advertised window unparked, then parks on the next:
	// the advertisement landed, and it was exactly the advertisement.
	lifted := func(t *testing.T, conn *drpc.Conn, client echo.EchoServiceClient, events *flowEvents) {
		t.Helper()
		s, err := client.Buff(t.Context())
		x.NoError(t, err)
		sendN(t, s, int(window))
		x.Equal(t, 0, events.count(drpc.EventPeerFlowStall), "the advertisement landed")
		done := make(chan error, 1)
		go func() { done <- s.Send(msg) }()
		synctest.Wait()
		x.Equal(t, 1, events.count(drpc.EventPeerFlowStall), "exactly the advertised window")
		conn.Close(nil) // releases the parked send
		<-done
	}

	t.Run("the Conn's first streaming call: its H", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			srv, conn, client := clientFixture(t, srvEpochA, window, events)
			defer conn.Close(nil)
			srv.advertise(window)

			// Released before its ack: the H then lands on no call — a
			// RESET — and carries the advertisement all the same.
			srv.mute(true)
			ctx, cancel := context.WithCancel(t.Context())
			_, err := client.Buff(ctx)
			x.NoError(t, err)
			cancel()
			synctest.Wait()
			h := srv.frame(srv.lastOpen(), 0)
			h.SetWindow(window)
			h.SetConnWindow(window)
			x.NoError(t, conn.Handle(context.Background(), h))
			x.Equal(t, 1, countMatch(srv.txFrames(), isResetFrame), "the H found no call")
			srv.mute(false)

			lifted(t, conn, client, events)
		})
	})
	t.Run("the Conn's first unary call: its T", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			srv, conn, client := clientFixture(t, srvEpochA, window, events)
			defer conn.Close(nil)
			srv.advertise(window)

			// Cancelled before its T; the T then lands on no call — a RESET
			// — and carries the advertisement like any T.
			srv.mute(true)
			ctx, cancel := context.WithCancel(t.Context())
			res := make(chan error, 1)
			go func() {
				_, err := client.Once(ctx, &echo.EchoRequest{})
				res <- err
			}()
			synctest.Wait()
			cancel()
			x.Equal(t, codes.Canceled, status.Code(<-res))
			synctest.Wait()
			term := srv.frame(srv.lastOpen(), drpc.FlagClose)
			term.SetCode(uint32(codes.OK))
			term.SetConnWindow(window)
			x.NoError(t, conn.Handle(context.Background(), term))
			x.Equal(t, 1, countMatch(srv.txFrames(), isResetFrame), "the T found no call")
			srv.mute(false)

			lifted(t, conn, client, events)
		})
	})
	t.Run("the first streaming call to a new incarnation", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			srv, conn, client := clientFixture(t, srvEpochA, window, events)
			defer conn.Close(nil)
			srv.advertise(window)

			// Locked to A by a call it answered and advertised, some sent.
			warm, err := client.Buff(t.Context())
			x.NoError(t, err)
			sendN(t, warm, 10)

			// The server restarts; the Conn's first call to B is released
			// before B's ack, which lands on no call. The Conn re-locks to B
			// from it all the same — sender started over — and applies B's
			// advertisement on it.
			srv.restart(srvEpochB)
			srv.mute(true)
			ctx, cancel := context.WithCancel(t.Context())
			_, err = client.Buff(ctx)
			x.NoError(t, err)
			cancel()
			synctest.Wait()
			h := srv.frame(srv.lastOpen(), 0)
			h.SetWindow(window)
			h.SetConnWindow(window)
			x.NoError(t, conn.Handle(context.Background(), h))
			srv.mute(false)

			lifted(t, conn, client, events)
		})
	})
	t.Run("a released call's data frame locks but advertises nothing", func(t *testing.T) {
		bubble(t, func(t *testing.T) {
			events := &flowEvents{}
			srv, conn, client := clientFixture(t, srvEpochA, window, events)
			defer conn.Close(nil)
			srv.advertise(window)

			// Released before any server frame; a data frame then lands on
			// no call — a RESET — and locks the Conn like any sequenced frame,
			// but only an H or T carries an advertisement: its absent
			// conn_window must neither read as "off" nor spend the latch.
			srv.mute(true)
			ctx, cancel := context.WithCancel(t.Context())
			_, err := client.Buff(ctx)
			x.NoError(t, err)
			cancel()
			synctest.Wait()
			d := srv.frame(srv.lastOpen(), 0)
			d.SetPayload([]byte{0})
			x.NoError(t, conn.Handle(context.Background(), d))
			x.Equal(t, 1, countMatch(srv.txFrames(), isResetFrame), "the data frame found no call")
			srv.mute(false)

			// The next call's H is therefore the first advertisement, and it
			// lands: had the data frame turned the window off, the latch
			// would have ignored this H and nothing would ever park.
			lifted(t, conn, client, events)
		})
	})
}
