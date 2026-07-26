//go:build !js

// Command browser-wasm is a Go server whose UI is developed by running the
// real server inside the browser.
//
// GET /app.wasm builds ./wasm — the same todo.Service this process serves,
// compiled for js/wasm — and the page starts it in a Worker, on a
// MessageChannel. So a reload restarts the server, and with -rebuild it
// recompiles it first: the UI loop is a browser reload, against real handlers,
// real gRPC statuses and a real server stream. The same handlers also answer
// at /rpc over a WebSocket, and the page switches to that with ?server=ws
// without a line of its UI code changing — which is the argument the example
// makes: the server in the browser is not a mock of the server, it is the
// server.
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

	"github.com/gorilla/websocket"
	drpc "github.com/lesomnus/grpc-dgram"
	_ "github.com/lesomnus/grpc-dgram/examples/browser-wasm/jsoncodec" // the "json" codec the page names on OPEN (§12)
	"github.com/lesomnus/grpc-dgram/examples/browser-wasm/todo"
	"github.com/lesomnus/grpc-dgram/examples/browser-wasm/todopb"
	"github.com/lesomnus/grpc-dgram/transport/gorilla"
)

var (
	addr    = flag.String("addr", "127.0.0.1:8080", "address for the HTTP server (the page, /app.wasm and the WebSocket endpoint)")
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

	// One Gateway serves every WebSocket, one drpc peer each. It advertises
	// Reliable() == true, so NewServer turns the timer machinery off and
	// requires the exact sequence on the wire (PROTOCOL.md §10.6) — the same
	// mode the wasm server runs in behind jsport.
	gw := gorilla.NewGateway()
	srv := drpc.NewServer(gw)
	todopb.RegisterTodoServiceServer(srv, todo.NewService(
		todo.NewMemStore(
			"Reload the page — the wasm server restarts with it",
			"This task lives in the server process, not the tab",
		),
		fmt.Sprintf("%s (pid %d)", *addr, os.Getpid()),
	))
	defer srv.Stop()

	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade already wrote the error response
		}
		log.Printf("websocket peer %s connected", c.RemoteAddr())
		go func() {
			// ServePeer blocks until the socket dies — hence the goroutine —
			// and on every exit performs the §4.5 teardown, srv.DisconnectPeer,
			// which fails that peer's live calls. With protocol timers off,
			// nothing else would. The context has to outlive this handler:
			// r.Context() is cancelled the moment it returns, which would tear
			// the fresh peer down at once.
			err := gw.ServePeer(ctx, srv, c)
			log.Printf("websocket peer gone: %v", err)
		}()
	})
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

	log.Printf("serving todo.TodoService in the browser and at ws://%s/rpc — open http://%s", *addr, *addr)
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
