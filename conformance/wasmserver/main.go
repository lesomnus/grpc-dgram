//go:build js && wasm

// Command wasmserver is a cross-language conformance fixture: a real Go
// drpc.Server compiled to GOOS=js GOARCH=wasm, serving internal/echo over a JS
// message port so a TypeScript client can drive it — see ts/test/wasm.test.ts,
// which builds this module, loads it under node and talks to it across a
// MessageChannel.
//
// Why it exists beside conformance/udpserver, the other Go<->TS runtime proof:
// that one runs over loopback UDP, where reliable mode has to be faked frame by
// frame (drpc.NewReliableContext) because the channel is not actually reliable.
// A message port IS reliable — it neither loses, duplicates nor reorders — so
// the half of wire v1.1 that exists in reliable mode only (per-stream flow
// control, PROTOCOL.md §4.2.1) is exercised across implementations here on a
// channel that earns it, with neither side passing a mode option: the gateway
// reports Reliable() and each core discovers it (§4.3).
//
// It also runs in ONE process. There is no socket, no port number to announce
// and no stdin to watch for the parent going away: the client is the same node
// process that instantiated this module, and everything crosses as marshaled
// Envelop bytes over the port (§4.1).
//
// It serves TWO servers, which is the other thing only this fixture can prove.
// One instance may run several — a second drpc.Server with its own registry and
// its own handlers, published under a name of its own (jsport.WithEntryPoint)
// — and the page reaches it with sock.dial({ entryPoint }). Only the FIRST is
// readiness, and the second is published deliberately LATE, after a turn of the
// JS event loop, because that is the case the host has to survive: a real
// program does something asynchronous between its two gateways, and whether the
// page reaches dial() before the second Serve has run is a matter of Go's
// scheduler, not of anything the page can arrange.
//
// The host drives it through exactly four globals:
//
//	drpcServe(port)  Gateway.Serve's own entry point: bind the port and serve
//	                 it as one peer (§6.4). Publishing it is also the readiness
//	                 signal — there is no drpcReady, because the property
//	                 appearing is the event the host awaits (ts/src/wasm's
//	                 open() is that host, and the two teardown globals below are
//	                 set before Serve so they exist by the time it fires)
//	drpcAdmin(port)  the second server's entry point, published one event-loop
//	                 turn after readiness, so a dial to it necessarily WAITS
//	drpcStop()       Gateway.Close: the empty-envelop goodbye on every served
//	                 port, so the TS side can prove that a peer which says
//	                 goodbye tears its peer's calls down (§4.5)
//	drpcExit()       os.Exit: the instance dies saying nothing — no deferred
//	                 code, no goodbye — so the TS side can prove the host's own
//	                 half of §4.5, go.run()'s promise wired to transport.close
package main

import (
	"context"
	"log"
	"os"
	"syscall/js"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/jsport"
	_ "google.golang.org/grpc/encoding/gzip" // registers "gzip", the §12.1 interop baseline
)

// adminEntryPoint is the second server's name; the host dials it by it.
const adminEntryPoint = "drpcAdmin"

// adminDelay is how long the second gateway waits before publishing. A sleep
// stands in for whatever a real program does between its two gateways — a
// fetch, a database opening — and the only thing that matters about it is that
// it YIELDS: Go's scheduler runs on the JS event loop here, so a sleeping
// goroutine hands control back to the host, which is then free to reach dial()
// with this name still absent. Long enough that the host cannot win the race by
// accident, short enough to cost the suite nothing.
const adminDelay = 50 * time.Millisecond

// adminServer is internal/echo with Once marked, which is the whole of how the
// host tells the two servers apart: same service, same wire, different
// registry. Every other method falls through unchanged.
type adminServer struct {
	*echo.EchoServer
}

func (s *adminServer) Once(ctx context.Context, req *echo.EchoRequest) (*echo.EchoResponse, error) {
	res, err := s.EchoServer.Once(ctx, req)
	if err != nil {
		return nil, err
	}
	res.SetMessage("admin:" + res.GetMessage())
	return res, nil
}

// serveAdmin is the second server: its own registry, its own handlers, sharing
// the module, the runtime, the memory and the lifetime of the first. It runs on
// its own goroutine because Serve blocks — and it publishes late on purpose;
// see the delay above.
func serveAdmin(ctx context.Context) {
	gw := jsport.NewGateway(jsport.WithEntryPoint(adminEntryPoint))
	srv := drpc.NewServer(gw)
	// A DIFFERENT implementation behind the same service, so the host can tell
	// which of the two servers answered rather than take the wiring on faith.
	echo.RegisterEchoServiceServer(srv, &adminServer{EchoServer: &echo.EchoServer{}})

	time.Sleep(adminDelay)
	// A refusal must not be silent: it would reach the host only as a dial
	// that waits out its timeout, blaming the name rather than the collision.
	if err := gw.Serve(ctx, srv); err != nil {
		js.Global().Set("drpcAdminErr", err.Error())
	}
}

func main() {
	gw := jsport.NewGateway()
	srv := drpc.NewServer(gw)
	// Registration precedes the first received frame (PROTOCOL.md §13): here,
	// before Serve can publish the entry point, let alone bind a port.
	echo.RegisterEchoServiceServer(srv, &echo.EchoServer{})

	// Neither of these js.Funcs is ever released, which is the one case where
	// that is correct: they live exactly as long as the instance, and the only
	// thing that ends the instance is drpcExit taking both of them with it. A
	// js.Func released while JS can still reach it turns the next call into a
	// console error instead of a teardown.
	js.Global().Set("drpcStop", js.FuncOf(func(js.Value, []js.Value) any {
		gw.Close()
		return nil
	}))
	js.Global().Set("drpcExit", js.FuncOf(func(js.Value, []js.Value) any {
		os.Exit(0)
		return nil
	}))

	ctx := context.Background()
	go serveAdmin(ctx)

	// Serve publishes drpcServe, serves every port the host hands it, and
	// blocks — which is also what keeps main from returning, and a returned
	// main would kill the instance and every js.Func above with it the moment
	// the host saw it ready.
	log.Fatal(gw.Serve(ctx, srv))
}
