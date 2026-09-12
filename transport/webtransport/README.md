# transport/webtransport

dRPC over WebTransport datagrams
([quic-go/webtransport-go](https://github.com/quic-go/webtransport-go)):
**one datagram carries one marshaled `Envelope`**, the channel is unreliable
(dRPC's default mode, timers on), and nothing is ever fragmented. This is the
browser's sensor-stream path: a page reaches a server with a URL and nothing
else — no signaling, no second server — and gets the mode the protocol is
designed around, which WebSocket cannot give it.

Only the session's datagram side is used. A reliable channel over one of the
session's streams is a possible second step, on the pion precedent; the
adapter takes an already-established `*wt.Session` and leaves dialing, the
HTTP/3 server, TLS and the CONNECT upgrade to the application.

Its own Go module: importing the core never pulls quic-go. The library's
package is also named `webtransport`, so the snippets alias it `wt`, as the
adapter's own source does:

```go
import (
    "github.com/lesomnus/grpc-dgram/transport/webtransport"
    "github.com/quic-go/quic-go/http3"
    wt "github.com/quic-go/webtransport-go"
)
```

## Server

One `Gateway` serves many sessions, one peer each. The session is created
by the HTTP/3 handler that upgrades the CONNECT request; it outlives that
handler, so `ServePeer` takes a server-lifetime context, not the request's.

```go
gw := webtransport.NewGateway()
srv := drpc.NewServer(gw)
pb.RegisterSensorServiceServer(srv, &myHandler{})

mux := http.NewServeMux()
s := &wt.Server{
    H3: &http3.Server{
        TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
        Handler:   mux,
    },
    // The default admits only same-origin requests; a page served from
    // elsewhere needs an explicit answer.
    CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "https://app.example" },
}
mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
    sess, err := s.Upgrade(w, r)
    if err != nil {
        w.WriteHeader(http.StatusInternalServerError)
        return
    }
    // Blocks until the session dies, then deregisters the peer and calls
    // srv.DisconnectPeer — failing that peer's live calls. ctx is the
    // server's, so returning from this handler is fine.
    go gw.ServePeer(ctx, srv, sess)
})

pc, err := net.ListenUDP("udp", &net.UDPAddr{Port: 4433})
if err != nil { ... }
go s.Serve(pc) // one UDP socket; returns when s.Close is called

// shutdown:
srv.GracefulStop() // or srv.Stop()
s.Close()          // closes every session while the socket is still open
pc.Close()
```

`Serve` enables QUIC datagrams on the HTTP/3 server itself. If you accept
QUIC connections yourself and hand them over with `ServeQUICConn`, set both
`EnableDatagrams` and `EnableStreamResetPartialDelivery` on that
connection's `quic.Config` — the library refuses a session without them.

`ServePeer` does not close the session on exit; it is the caller's, as with
every gateway. `Server.Close` closes them all, and a peer whose context you
cancelled is yours to close.

## Client

`wt.Transport.Dial` opens one QUIC connection per session. `drpc.NewConn`
attaches the transport (`drpc.ConnAttacher`): the receive
pump starts by itself — no goroutine to manage — and the transport owns the
session from then on.

```go
d := &wt.Transport{
    TLSClientConfig: &tls.Config{RootCAs: pool}, // Dial sets the h3 ALPN
}
_, sess, err := d.Dial(ctx, "https://host:4433/rpc", nil)
if err != nil { ... }

conn := drpc.NewConn(webtransport.New(sess)) // unreliable mode auto-detected via TransportInfo
client := pb.NewSensorServiceClient(conn)

// shutdown — one call closes the conn, the transport, the session, and
// (for a dialed session) the QUIC connection:
conn.Close(nil)
```

The browser twin is `ts/src/transport/webtransport` (`dialWebTransport`),
wire-compatible with this adapter.

## Options

| Option | Default | Meaning |
|---|---|---|
| `WithMaxMessageSize(n)` | `DefaultMaxMessageSize` (1200 B) | largest marshaled `Envelope` this endpoint will **send**; receives accept any datagram |

The default is a constant, not "what the session reports": webtransport-go
exposes no max-datagram-size getter. quic-go's ceiling right after the
handshake is 1243 B of QUIC payload (from its 1280-byte initial packet
size), one byte of which is the HTTP/3 quarter-stream-id prefix, and
path-MTU discovery only raises it from there — so 1200 B, transport/udp's
number, fits from the first packet. The browser side reports
`datagrams.maxDatagramSize` and defaults to it.

## TLS, and the browser

- **There is no plaintext path.** WebTransport is HTTP/3 over QUIC, which is
  always TLS: the server needs a certificate, and the URL is `https://`.
  ALPN is `h3` (`http3.ConfigureTLSConfig` sets it on the server;
  `Transport.Dial` on the client).
- **Development certificates.** A browser accepts a self-signed certificate
  through `serverCertificateHashes` (`new WebTransport(url,
  {serverCertificateHashes: [{algorithm: "sha-256", value}]})`), with
  limits: an ECDSA key (P-256; RSA is refused), a validity period under two
  weeks, and a dedicated connection (no pooling). `value` is the SHA-256 of
  the DER certificate:

  ```go
  sum := sha256.Sum256(cert.Certificate[0])
  // base64.StdEncoding.EncodeToString(sum[:]) is what the page decodes
  ```

  The suite's `testCert` builds exactly this shape (13-day ECDSA P-256), and
  webtransport-go's `example/server` prints the hash for a page. Anything
  longer-lived needs a CA the browser trusts.
- **Origin.** `Server.CheckOrigin` defaults to same-origin (an `Origin`
  header must match `Host`); Go clients send none and pass. A page served
  from another origin needs an explicit `CheckOrigin`.
- **Liveness at two levels.** A vanished peer is noticed by dRPC's own
  timers (`T_live`) and, underneath, by QUIC's idle timeout
  (`quic.Config.MaxIdleTimeout`, default 30 s, with `KeepAlivePeriod` to
  hold NATs open) — the latter closes the session, which is the adapter's
  death signal.

## Caveats

- **One message = one datagram, never fragmented.** A marshaled envelope over
  the limit is refused at send and the owning call fails with
  `ResourceExhausted`; the session stays up. The QUIC stack keeps its own,
  path-dependent ceiling underneath: raising the option past it does not
  fragment either — the stack refuses before queueing and the adapter
  reports it the same way. Keep messages small — natural for sensor
  readings.
- **Teardown comes from the session, not the read loop.** The session's
  closure — a `WT_CLOSE_SESSION` from either side, a QUIC error, the idle
  timeout — ends the pump from outside it and the attached pump /
  `ServePeer` call `Conn.Close` / `DisconnectPeer` with the cause
  (`PROTOCOL.md` §4.5). A goodbye (code 0, or the peer closing its
  connection with `H3_NO_ERROR`) is a clean end and reports `nil`; a coded
  close carries its code and message into the failed calls' status.
- **Sends park on a dead path; ctx and the idle timeout bound them.** The
  connection's datagram queue holds 32 frames and drains only as QUIC sends
  them, under congestion control: once the peer stops acknowledging (a
  laptop lid closed) it drains one frame per PTO probe, at intervals that
  double, until the idle timeout closes the connection — nothing like a
  socket buffer, which drains at line rate whatever the peer does. The
  adapter waits on the stack only as long as the ctx it was handed and the
  session live: the core gives `Handle` the call's ctx, so a handler whose
  peer the core has torn down (`T_live`, §4.5) returns with its calls,
  `GracefulStop` included, rather than parking until the connection dies.
  An abandoned send keeps a goroutine — and its datagram, which may still go
  out late — until a slot frees or the connection dies. Both are bounded by
  `quic.Config.MaxIdleTimeout` (30 s by default; `KeepAlivePeriod` keeps a
  live path from hitting it), set on the client's `Transport.QUICConfig` and
  the H3 server's `QUICConfig` alike. Received datagrams past the stack's
  per-session queue (32) are dropped — datagram semantics, and the pump
  keeps up.
- **Received garbage is ignored.** Unparseable envelopes are dropped; they
  never tear the session down.
- The wire is encrypted by QUIC, but the protocol itself authenticates
  nothing beyond the session — see `PROTOCOL.md` §15.
