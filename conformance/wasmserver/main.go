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
// The host drives it through exactly three globals:
//
//	drpcServe(port)  Gateway.Serve's own entry point: bind the port and serve
//	                 it as one peer (§6.4). Publishing it is also the readiness
//	                 signal — there is no drpcReady, because the property
//	                 appearing is the event the host awaits (ts/src/wasm's
//	                 open() is that host, and the two teardown globals below are
//	                 set before Serve so they exist by the time it fires)
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

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/internal/echo"
	"github.com/lesomnus/grpc-dgram/transport/jsport"
	_ "google.golang.org/grpc/encoding/gzip" // registers "gzip", the §12.1 interop baseline
)

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

	// Serve publishes drpcServe, serves every port the host hands it, and
	// blocks — which is also what keeps main from returning, and a returned
	// main would kill the instance and every js.Func above with it the moment
	// the host saw it ready.
	log.Fatal(gw.Serve(context.Background(), srv))
}
