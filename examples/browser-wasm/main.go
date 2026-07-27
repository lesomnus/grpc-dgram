//go:build !js

// Command browser-wasm is the dev server for a Go gRPC server that runs in the
// browser: it serves the page, and builds the server the page starts.
//
// GET /app.wasm builds ./wasm — todo.Service, compiled for js/wasm — and the
// page starts it in a Worker, on a MessageChannel. So a reload restarts the
// server, and with -rebuild it recompiles it first: the UI loop is a browser
// reload, against real handlers, real gRPC statuses and a real server stream.
//
// This process serves files; it does not serve the service. That the server in
// the page is a real one is a source-level fact rather than something to
// demonstrate here — todo/ is ordinary gRPC handler code implementing
// todopb.TodoServiceServer — and serving handlers like it from a process, over
// a socket, is examples/websocket-echo.
//
//	cd ts && pnpm install && pnpm build   # build the TS port once
//	cd examples/browser-wasm && go run .  # then open http://127.0.0.1:8080
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	addr    = flag.String("addr", "127.0.0.1:8080", "address for the HTTP server (the page and /app.wasm)")
	rebuild = flag.Bool("rebuild", true, "rebuild ./wasm on every GET /app.wasm; with -rebuild=false the prebuilt web/app.wasm is served instead")
)

const (
	// webDir and tsDist are fixed rather than flags: `go run .` runs in this
	// directory, and the page's import map names /ts/dist/ literally.
	webDir = "web"
	tsDist = "../../ts/dist"
)

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "browser-wasm:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// The page imports the port from /ts/dist/, so it has to exist. Fail with
	// the command that fixes it rather than with a 404 in the console.
	if _, err := os.Stat(filepath.Join(tsDist, "index.mjs")); err != nil {
		return fmt.Errorf("%s is not built — run `cd ts && pnpm install && pnpm build` first", tsDist)
	}
	// Modules are refused by the browser unless they are served as JavaScript,
	// and a stale /etc/mime.types can get .mjs wrong.
	_ = mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")

	wasmExec, err := wasmExecPath()
	if err != nil {
		return err
	}
	app, cleanup, err := newAppWasm()
	if err != nil {
		return err
	}
	defer cleanup()

	mux := http.NewServeMux()
	// The server compiled for the page. This is the point of the example:
	// every reload fetches a freshly built binary and starts a fresh instance.
	mux.HandleFunc("GET /app.wasm", app)
	// The JS half of the Go runtime, from the toolchain that built the module.
	// The worker fetches it, not the page — open() loads it in whichever realm
	// runs the instance — so this route stays even though no <script> names it.
	mux.HandleFunc("GET /wasm_exec.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, wasmExec)
	})
	// The TypeScript port is not published, so the page imports the build
	// output from here; see web/index.html's import map.
	mux.Handle("/ts/dist/", http.StripPrefix("/ts/dist/", http.FileServer(http.Dir(tsDist))))
	mux.Handle("/", http.FileServer(http.Dir(webDir)))

	hs := &http.Server{Addr: *addr, Handler: noStore(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()

	log.Printf("building todo.TodoService for the browser to run — open http://%s", *addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// noStore forbids caching everything this server hands out. It is a dev
// server, and the whole claim is that a reload gets a freshly built, freshly
// started server — a cached app.wasm or a cached ts/dist would quietly break
// exactly that.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// newAppWasm returns the GET /app.wasm handler and the cleanup for whatever it
// allocated.
func newAppWasm() (http.HandlerFunc, func(), error) {
	if !*rebuild {
		// -rebuild=false serves what you built yourself, which is what a
		// deployment does: the wasm binary is a build artifact like any other.
		path := filepath.Join(webDir, "app.wasm")
		return func(w http.ResponseWriter, r *http.Request) {
			if _, err := os.Stat(path); err != nil {
				http.Error(w, "no "+path+": build it with `GOOS=js GOARCH=wasm go build -o web/app.wasm ./wasm`, or drop -rebuild=false", http.StatusInternalServerError)
				return
			}
			serveWasm(w, r, path)
		}, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "browser-wasm")
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "app.wasm")
	// One build at a time: two reloads in flight would otherwise compile into
	// the same file at once, and the loser would be served a half-written
	// binary.
	var mu sync.Mutex

	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		started := time.Now()
		cmd := exec.CommandContext(r.Context(), "go", "build", "-o", path, "./wasm")
		cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
		if out, err := cmd.CombinedOutput(); err != nil {
			// Throw away whatever the build left at path. `go build -o` decides
			// the output is up to date from the build ID it can still read out
			// of the file already sitting there, so a half-written binary — a
			// reload that cancelled r.Context() and killed the build mid-write
			// — would be reported as a successful build from then on and served
			// to every later reload, unchanged.
			_ = os.Remove(path)
			// Answer with the compiler output: a handler that stopped
			// compiling must show up in the browser, not as silence.
			log.Printf("build failed in %s: %v\n%s", took(started), err, out)
			http.Error(w, fmt.Sprintf("go build ./wasm failed: %v\n\n%s", err, out), http.StatusInternalServerError)
			return
		}
		log.Printf("built app.wasm in %s", took(started))
		serveWasm(w, r, path)
	}, func() { _ = os.RemoveAll(dir) }, nil
}

func serveWasm(w http.ResponseWriter, r *http.Request, path string) {
	// WebAssembly.instantiateStreaming refuses anything that is not
	// application/wasm, and refuses it after the fetch has already succeeded —
	// a confusing failure to debug from the page.
	w.Header().Set("Content-Type", "application/wasm")
	http.ServeFile(w, r, path)
}

func took(since time.Time) time.Duration { return time.Since(since).Round(time.Millisecond) }

// wasmExecPath locates wasm_exec.js in the active toolchain. It is the JS half
// of the Go runtime and is version-coupled to the compiler that built the
// module, so it is served from GOROOT rather than committed here — and for the
// same reason the TypeScript package does not vendor it either: a copy in web/
// would keep working right up until the day the toolchain is upgraded.
func wasmExecPath() (string, error) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return "", fmt.Errorf("locate GOROOT (is the go toolchain on PATH?): %w", err)
	}
	path := filepath.Join(strings.TrimSpace(string(out)), "lib", "wasm", "wasm_exec.js")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return path, nil
}
