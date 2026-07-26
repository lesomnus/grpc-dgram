// Command browser-webrtc is the project's final goal, runnable: a browser page
// calling a Go gRPC service over a WebRTC DataChannel. No HTTP/2, no proxy, no
// gRPC-Web — the page speaks the drpc wire protocol directly, using the
// TypeScript port (ts/), and the Go side answers with generated gRPC stubs on
// transport/pion.
//
// The HTTP server here exists only to hand the browser two things: the page
// itself, and one SDP answer. Everything else runs on the DataChannel.
//
//	cd ts && pnpm install && pnpm build      # build the TS port once
//	cd examples/browser-webrtc && go run .   # then open http://127.0.0.1:8080
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	drpc "github.com/lesomnus/grpc-dgram"
	"github.com/lesomnus/grpc-dgram/examples/browser-webrtc/echopb"
	"github.com/lesomnus/grpc-dgram/transport/pion"
)

var (
	addr   = flag.String("addr", "127.0.0.1:8080", "address for the HTTP server (the page and the SDP exchange)")
	webDir = flag.String("web", "web", "directory holding index.html and main.js")
	tsDist = flag.String("ts-dist", "../../ts/dist", "the built TypeScript port; mounted at /ts/dist/")
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "browser-webrtc:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// The page imports the port from /ts/dist/, so it has to exist. Fail with
	// the command that fixes it rather than with a 404 in the console.
	if _, err := os.Stat(filepath.Join(*tsDist, "index.mjs")); err != nil {
		return fmt.Errorf("%s is not built — run `cd ts && pnpm install && pnpm build` first", *tsDist)
	}
	// Modules are refused by the browser unless they are served as JavaScript,
	// and a stale /etc/mime.types can get .mjs wrong.
	_ = mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")

	// One Gateway serves every DataChannel, one drpc peer each; each peer runs
	// in its own channel's mode (PROTOCOL.md §4.3). The page's channel is the
	// default kind — ordered, no caps — so it is reliable and no protocol
	// timer runs on it.
	gw := pion.NewGateway()
	srv := drpc.NewServer(gw)
	echopb.RegisterEchoServiceServer(srv, newEchoService())
	defer srv.Stop()

	sig := &signaler{ctx: ctx, gw: gw, srv: srv}
	defer sig.closeAll()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /offer", sig.offer)
	// The TypeScript port is not published, so the page imports the build
	// output from here; see web/index.html's import map.
	mux.Handle("/ts/dist/", http.StripPrefix("/ts/dist/", http.FileServer(http.Dir(*tsDist))))
	mux.Handle("/", http.FileServer(http.Dir(*webDir)))

	hs := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()

	log.Printf("serving webecho.EchoService over WebRTC — open http://%s", *addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
