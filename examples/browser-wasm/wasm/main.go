//go:build js && wasm

// Command wasm is the server, compiled for the page: the same todo.Service the
// dev server registers, behind a jsport.Gateway instead of a WebSocket. It is
// built on demand by the dev server (GET /app.wasm) and started by the page,
// so a browser reload restarts the whole server — new instance, new store,
// new everything.
//
// Its whole JS surface is one global, and this file never writes its name:
//
//	globalThis.drpcServe(port) // hand over one end of a MessageChannel
//
// Gateway.Serve publishes it — that publish IS the readiness signal, there is
// no ready callback — and the page waits for the property to appear
// (startWasmServer does both halves; see web/main.js). One port is one peer
// (PROTOCOL.md §6.4), so calling it again with a second port serves another
// client — a Worker, another tab's channel — off the same handlers.
//
//	GOOS=js GOARCH=wasm go build -o web/app.wasm ./wasm
package main

import (
	"context"
	"log"

	drpc "github.com/lesomnus/grpc-dgram"
	_ "github.com/lesomnus/grpc-dgram/examples/browser-wasm/jsoncodec" // the "json" codec the page names on OPEN (§12)
	"github.com/lesomnus/grpc-dgram/examples/browser-wasm/todo"
	"github.com/lesomnus/grpc-dgram/examples/browser-wasm/todopb"
	"github.com/lesomnus/grpc-dgram/transport/jsport"
)

func main() {
	// The same three lines the dev server runs, with jsport.NewGateway in
	// place of gorilla.NewGateway. Both advertise Reliable() == true, so this
	// server runs with every protocol timer off (PROTOCOL.md §10.6) exactly as
	// the other one does.
	gw := jsport.NewGateway()
	srv := drpc.NewServer(gw)
	todopb.RegisterTodoServiceServer(srv, todo.NewService(
		todo.NewMemStore(
			"Reload the page — this server restarts with it",
			"Switch to ?server=ws — the UI code does not change",
		),
		"in-page (wasm)",
	))

	// Last, and blocking: publishing the entry point says "I can serve now", so
	// nothing may be published before the registration above is done (§13) —
	// and blocking is the other half of the job, because a returned main kills
	// the instance and every js.Func with it. The page would see go.run()'s
	// promise resolve and fail its calls, which is exactly what it does when
	// this server really dies.
	log.Fatal(gw.Serve(context.Background(), srv))
}
