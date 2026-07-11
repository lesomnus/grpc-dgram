package drpc_test

// restart_test.go pins the end-to-end restart walkthroughs of PROTOCOL.md
// §6.5: a mid-call process restart is not a special mechanism, it is what the
// epoch rules compose into. The pipe below models a restart behind a stable
// address — a fresh Server (new epoch, empty dedup state) swapped in behind
// the same delivery path, or a fresh Conn replacing one whose tx went dark.

import (
	"context"
	"sync"
	"sync/atomic"
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

type restartPipe struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	service *echo.EchoServer
	execs   atomic.Int32
	sOpts   []drpc.ServerOption

	srv     atomic.Pointer[drpc.Server]
	target  atomic.Pointer[drpc.Conn] // server frames land on the newest conn
	s2cDead atomic.Bool               // a crashed server's dying gasps vanish
	conns   []*drpc.Conn

	ca chan []byte // server -> client
	cb chan []byte // client -> server
}

func newRestartPipe(t *testing.T) *restartPipe {
	ctx, cancel := context.WithCancel(t.Context())
	p := &restartPipe{
		t:       t,
		ctx:     ctx,
		cancel:  cancel,
		service: &echo.EchoServer{},
		ca:      make(chan []byte, 256),
		cb:      make(chan []byte, 256),
	}
	p.sOpts = []drpc.ServerOption{
		drpc.WithReliable(false),
		drpc.WithTiming(fastTiming),
		countExecs(&p.execs),
	}
	p.srv.Store(p.newServer())

	// Frames round-trip through real Envelop serialization (PROTOCOL.md §4.1);
	// the delivery target is resolved per frame so a swapped-in incarnation
	// takes over the address transparently.
	pump := func(ch chan []byte, deliver func(context.Context, *drpc.Frame)) func() {
		return func() {
			for {
				select {
				case <-ctx.Done():
					return
				case data := <-ch:
					e := &drpc.Envelop{}
					if err := proto.Unmarshal(data, e); err != nil {
						panic(err)
					}
					for _, f := range e.GetFrames() {
						deliver(ctx, f)
					}
				}
			}
		}
	}
	p.wg.Go(pump(p.cb, func(ctx context.Context, f *drpc.Frame) {
		p.srv.Load().Handle(ctx, f)
	}))
	p.wg.Go(pump(p.ca, func(ctx context.Context, f *drpc.Frame) {
		if p.s2cDead.Load() {
			return
		}
		p.target.Load().Handle(ctx, f)
	}))
	return p
}

func (p *restartPipe) newServer() *drpc.Server {
	tx := drpc.Wrap1(drpc.EnvelopHandlerFunc(func(_ context.Context, e *drpc.Envelop) error {
		if p.s2cDead.Load() {
			return nil
		}
		data, err := proto.Marshal(e)
		if err != nil {
			return err
		}
		p.ca <- data
		return nil
	}))
	srv := drpc.NewServer(tx, p.sOpts...)
	echo.RegisterEchoServiceServer(srv, p.service)
	return srv
}

// newConn attaches a client incarnation. The returned kill switch models the
// client process dying: its tx goes dark, but nothing is cleaned up.
func (p *restartPipe) newConn() (client echo.EchoServiceClient, dead *atomic.Bool) {
	dead = &atomic.Bool{}
	tx := drpc.Wrap1(drpc.EnvelopHandlerFunc(func(_ context.Context, e *drpc.Envelop) error {
		if dead.Load() {
			return nil
		}
		data, err := proto.Marshal(e)
		if err != nil {
			return err
		}
		p.cb <- data
		return nil
	}))
	conn := drpc.NewConn(tx, drpc.WithReliable(false), drpc.WithTiming(fastTiming))
	p.conns = append(p.conns, conn)
	p.target.Store(conn)
	return echo.NewEchoServiceClient(conn), dead
}

// restartServer crashes the current server incarnation and brings up a fresh
// one at the same address: in-flight and dying-gasp frames are lost, dedup
// state (tombstones, watermarks) dies with the instance, the epoch is new.
func (p *restartPipe) restartServer() {
	p.s2cDead.Store(true)
	synctest.Wait() // let in-flight deliveries drain into the blackhole
	p.srv.Load().Stop()
	synctest.Wait() // dying gasps (UNAVAILABLE terminals) drop too
	p.srv.Store(p.newServer())
	p.s2cDead.Store(false)
}

func (p *restartPipe) stop() {
	p.srv.Load().Stop()
	for _, c := range p.conns {
		c.Close(nil)
	}
	p.cancel()
	p.wg.Wait()
}

// ---------------------------------------------------------------------------
// §6.5 / §17 L2: a server restart while a unary call is in flight. The
// retransmitted OPEN is a fresh call to the new incarnation — the handler
// runs a SECOND time (the documented cross-incarnation re-execution) — and
// the not-yet-locked client accepts the new-epoch response: the call
// succeeds, the double execution is invisible to the application.
// ---------------------------------------------------------------------------

func TestChar_ServerRestartMidUnary(t *testing.T) {
	bubble(t, func(t *testing.T) {
		p := newRestartPipe(t)
		defer p.stop()
		client, _ := p.newConn()

		// The crash window opens before the first response can escape.
		p.s2cDead.Store(true)

		start := time.Now()
		type result struct {
			res *echo.EchoResponse
			err error
		}
		done := make(chan result, 1)
		go func() {
			res, err := client.Once(p.ctx, echo.EchoRequest_builder{
				Message:       "abc",
				CircularShift: 1,
			}.Build())
			done <- result{res, err}
		}()

		for p.execs.Load() == 0 { // the old incarnation executed; its T is lost
			time.Sleep(5 * time.Millisecond)
		}
		p.restartServer()

		r := <-done
		x.NoError(t, r.err)
		x.Equal(t, "bca", r.res.GetMessage())
		if n := p.execs.Load(); n != 2 {
			t.Fatalf("handler executions across the restart = %d, want exactly 2 (once per incarnation)", n)
		}
		if e := time.Since(start); e >= fastTiming.Call {
			t.Fatalf("recovered in %v, want under T_call %v", e, fastTiming.Call)
		}
	})
}

// ---------------------------------------------------------------------------
// §6.5: a server restart mid-stream. The client stream locked to the dead
// incarnation's epoch, so the new incarnation can never speak to it; the next
// client frame draws a delayed RESET (unknown sid, epoch echo) and the call
// fails UNAVAILABLE — via the RESET fast path, well under the T_live
// backstop. A follow-up call on the same conn succeeds against the new
// incarnation (epoch-flush relearns the method index, §13).
// ---------------------------------------------------------------------------

func TestChar_ServerRestartMidStream(t *testing.T) {
	bubble(t, func(t *testing.T) {
		p := newRestartPipe(t)
		defer p.stop()
		client, _ := p.newConn()

		stream, err := client.Live(p.ctx)
		x.NoError(t, err)
		x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build()))
		_, err = stream.Recv()
		x.NoError(t, err) // first accepted frame: locked to this incarnation

		p.restartServer()

		start := time.Now()
		// The next tx hits the new incarnation as an unknown sid.
		x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "b", Repeat: 1}.Build()))
		_, err = stream.Recv()
		x.Equal(t, codes.Unavailable, status.Code(err))
		if e := time.Since(start); e >= fastTiming.Liveness/2 {
			t.Fatalf("stream failed in %v, want the RESET fast path (well under T_live %v)", e, fastTiming.Liveness)
		}

		// The conn is not poisoned: a new call reaches the new incarnation.
		res, err := client.Once(p.ctx, echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())
	})
}

// ---------------------------------------------------------------------------
// §6.5: a client restart mid-call. The new incarnation (new epoch, same
// address) coexists with the old call — its fresh calls succeed immediately —
// and the old handler is reclaimed without any cooperation from the vanished
// incarnation (stray-frame RESETs or the T_live backstop), so GracefulStop
// cannot wedge.
// ---------------------------------------------------------------------------

func TestChar_ClientRestartMidCall(t *testing.T) {
	bubble(t, func(t *testing.T) {
		p := newRestartPipe(t)
		defer p.stop()

		c1, dead1 := p.newConn()
		stream, err := c1.Live(p.ctx)
		x.NoError(t, err)
		x.NoError(t, stream.Send(echo.EchoRequest_builder{Message: "a", Repeat: 1}.Build()))
		_, err = stream.Recv()
		x.NoError(t, err) // the old incarnation's handler is live

		dead1.Store(true)    // the client process dies...
		c2, _ := p.newConn() // ...and restarts at the same address

		// Coexistence: the new incarnation works while the old call is still
		// registered under the same peer.
		res, err := c2.Once(p.ctx, echo.EchoRequest_builder{
			Message:       "abc",
			CircularShift: 1,
		}.Build())
		x.NoError(t, err)
		x.Equal(t, "bca", res.GetMessage())

		// The old handler is reclaimed within the liveness bound with zero
		// cooperation from the dead incarnation.
		start := time.Now()
		done := make(chan struct{})
		go func() {
			p.srv.Load().GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * fastTiming.Liveness):
			t.Fatal("GracefulStop wedged: the vanished incarnation's handler leaked past T_live")
		}
		if e := time.Since(start); e >= 3*fastTiming.Liveness {
			t.Fatalf("old call reclaimed in %v, want under 3×T_live", e)
		}
	})
}
