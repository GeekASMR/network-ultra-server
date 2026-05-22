// network-ultra-bridge — local WebSocket relay for DAW hosts whose outbound
// connections are firewalled (e.g. cracked Studio One on Windows).
//
//   Studio One ──► VST plugin ──ws://127.0.0.1:18900──► Bridge ──► remote server
//                  (loopback, allowed)                    │
//                                                         └──► ws://server:18900
//                                                              (no firewall on
//                                                               normal Windows app)
//
// The bridge is a transparent byte-for-byte relay. It does no protocol parsing
// beyond the WebSocket frame layer; control + audio frames pass through unmodified.
//
// Usage:
//   network-ultra-bridge -listen 127.0.0.1:18900 -upstream ws://146.56.202.138:18900
//
// On launch the bridge accepts an unbounded number of plugin connections; each
// gets its own dedicated upstream WebSocket. When the plugin disconnects, the
// upstream is also closed; vice versa.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const (
	subprotocol         = "network-ultra-v1"
	relayCloseTimeout   = 3 * time.Second
	upstreamDialTimeout = 15 * time.Second // tolerant of 500 ms RTT + retransmits during peak hours
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18900",
		"Local address to accept plugin connections on (loopback only by default)")
	upstream := flag.String("upstream", "ws://146.56.202.138:18900",
		"Remote Network Ultra Server URL")
	logLevel := flag.String("log-level", "info", "debug | info | warn | error")
	flag.Parse()

	log := setupLogger(*logLevel)
	log.Info("starting", "listen", *listen, "upstream", *upstream)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handlePlugin(r.Context(), w, r, *upstream, log)
	})

	srv := &http.Server{
		Addr:        *listen,
		Handler:     mux,
		ReadTimeout: 0, // long-lived
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	case e := <-errCh:
		if e != nil && e != http.ErrServerClosed {
			log.Error("listen error", "err", e)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Info("bye")
}

func handlePlugin(parent context.Context, w http.ResponseWriter, r *http.Request, upstreamURL string, log *slog.Logger) {
	plugin, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		Subprotocols:       []string{subprotocol},
	})
	if err != nil {
		log.Warn("plugin accept failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	defer plugin.Close(websocket.StatusInternalError, "shutting down")
	plugin.SetReadLimit(8 * 1024 * 1024) // generous: audio frames + control

	log.Info("plugin connected", "remote", r.RemoteAddr)

	dialCtx, dialCancel := context.WithTimeout(parent, upstreamDialTimeout)
	defer dialCancel()

	// Bypass any HTTP_PROXY / HTTPS_PROXY env vars: Clash and other tunnels
	// often intercept arbitrary ports and break custom WebSocket protocols.
	directHTTP := &http.Client{
		Transport: &http.Transport{
			Proxy: nil, // explicitly disable proxy
		},
	}

	// IMPORTANT: explicitly set the HTTP Host header to the upstream's
	// host:port. The plugin connects to us at 127.0.0.1:18900, so without
	// this override the WebSocket handshake to the real server would carry
	// Host: 127.0.0.1:18900 — which the server uses to derive the UDP
	// endpoint to advertise back. Setting it correctly means the server
	// returns the real public IP+port, and the plugin's UDP client can
	// reach it.
	upstreamHost := extractHostFromWsURL(upstreamURL)
	dialHeaders := http.Header{}
	if upstreamHost != "" {
		dialHeaders.Set("Host", upstreamHost)
	}

	upstream, _, err := websocket.Dial(dialCtx, upstreamURL, &websocket.DialOptions{
		Subprotocols: []string{subprotocol},
		HTTPClient:   directHTTP,
		HTTPHeader:   dialHeaders,
		Host:         upstreamHost,
	})
	if err != nil {
		log.Warn("upstream dial failed", "err", err, "url", upstreamURL)
		_ = plugin.Close(websocket.StatusGoingAway, "upstream unavailable")
		return
	}
	defer upstream.Close(websocket.StatusInternalError, "shutting down")
	upstream.SetReadLimit(8 * 1024 * 1024)

	log.Info("upstream connected", "remote", r.RemoteAddr, "upstream", upstreamURL)

	relayCtx, relayCancel := context.WithCancel(parent)
	defer relayCancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relay(relayCtx, "plugin->upstream", plugin, upstream, log)
		relayCancel()
	}()
	go func() {
		defer wg.Done()
		relay(relayCtx, "upstream->plugin", upstream, plugin, log)
		relayCancel()
	}()
	wg.Wait()

	log.Info("session closed", "remote", r.RemoteAddr)
}

// relay copies WebSocket messages from src to dst until either side closes.
// We use Reader/Writer to avoid double-buffering large audio frames.
func relay(ctx context.Context, dir string, src, dst *websocket.Conn, log *slog.Logger) {
	for {
		mt, r, err := src.Reader(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Debug(dir+" reader closed", "err", err)
			}
			return
		}
		w, err := dst.Writer(ctx, mt)
		if err != nil {
			log.Debug(dir+" writer open failed", "err", err)
			return
		}
		if _, err := io.Copy(w, r); err != nil {
			log.Debug(dir+" copy failed", "err", err)
			_ = w.Close()
			return
		}
		if err := w.Close(); err != nil {
			log.Debug(dir+" writer close failed", "err", err)
			return
		}
	}
}

// extractHostFromWsURL pulls "host:port" out of "ws://host:port/path".
// Returns empty string on parse failure (caller falls back to default Host
// behaviour, which is fine when the URL is malformed).
func extractHostFromWsURL(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil || u.Host == "" {
		return ""
	}
	// websocket.Dial accepts ws:// and wss://; the Host on the URL is
	// "host:port" or just "host" (default port). Either is fine for the
	// HTTP Host header.
	return u.Host
}

// suppress unused-import warning for strings if we ever drop the use.
var _ = strings.TrimSpace

func setupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

func init() {
	// Make sure usage line is informative.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"network-ultra-bridge: local relay for DAW hosts whose outbound is firewalled.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
}
