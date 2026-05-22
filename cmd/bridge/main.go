// network-ultra-bridge — local relay for DAW hosts whose outbound is firewalled.
//
//   Studio One ──► VST plugin ──ws://127.0.0.1:18900──► Bridge ──► remote server
//                                                       │
//                              ──udp://127.0.0.1:18902──┤
//                                                       └──udp://server:18902
//
// Why we proxy BOTH WS and UDP:
//   * The plugin runs inside the DAW, which is often blocked outbound by
//     Windows Firewall (cracked Studio One on China retail builds is the
//     canonical case). The plugin can only reach 127.0.0.1.
//   * The bridge is a normal Windows app — not blocked. It does the heavy
//     lifting on both control plane (WebSocket) and data plane (UDP audio).
//
// Wire-level behaviour:
//   * Control: byte-for-byte WS relay, EXCEPT we sniff the JSON `welcome`
//     frame coming back from the server and rewrite `udpEndpoint` to point
//     at the bridge's own UDP listener (127.0.0.1:18902). The plugin then
//     handshakes UDP with us, not with the remote server.
//   * Audio out: every UDP packet on 127.0.0.1:18902 from the plugin is
//     forwarded to the upstream UDP endpoint (host of the WS upstream URL,
//     port 18902).
//   * Audio in: every UDP packet from upstream is forwarded back to the
//     plugin's most recent local UDP address. Because there's only one
//     plugin per bridge instance, a single source-address binding is
//     sufficient.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

const (
	subprotocol         = "network-ultra-v1"
	upstreamDialTimeout = 15 * time.Second
	udpListenPort       = 18902 // loopback UDP we expose to plugin
	udpUpstreamPort     = 18902 // remote server UDP port (matches server config)
	udpDatagramMax      = 9000
)

// udpProxy bridges plugin UDP traffic to the remote server.
//
// We use TWO sockets because a single socket bound to 127.0.0.1 can never
// receive packets from the public internet (kernel routes them to a
// 0.0.0.0-bound socket, not to 127.0.0.1). The two-socket design:
//
//   loopbackConn  : bound to 127.0.0.1:18902
//                   receives from plugin, sends back to plugin
//
//   upstreamConn  : bound to 0.0.0.0:0  (ephemeral)
//                   sends to remote server, receives replies, forwards back
//                   to plugin via loopbackConn
//
// The plugin's source address is captured the first time we see a packet
// from it; replies from upstream are sent to that address.
type udpProxy struct {
	log *slog.Logger

	loopbackConn *net.UDPConn // 127.0.0.1:18902
	upstreamConn *net.UDPConn // 0.0.0.0:ephemeral
	upstream     *net.UDPAddr // server's UDP host:port

	pluginAddrMu sync.RWMutex
	pluginAddr   *net.UDPAddr
}

func newUDPProxy(log *slog.Logger, listenAddr, upstreamAddr string) (*udpProxy, error) {
	la, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen %s: %w", listenAddr, err)
	}
	ua, err := net.ResolveUDPAddr("udp", upstreamAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream %s: %w", upstreamAddr, err)
	}
	loopback, err := net.ListenUDP("udp", la)
	if err != nil {
		return nil, fmt.Errorf("listen loopback %s: %w", listenAddr, err)
	}
	upstreamConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		_ = loopback.Close()
		return nil, fmt.Errorf("listen upstream socket: %w", err)
	}
	for _, c := range []*net.UDPConn{loopback, upstreamConn} {
		_ = c.SetReadBuffer(4 * 1024 * 1024)
		_ = c.SetWriteBuffer(4 * 1024 * 1024)
	}

	p := &udpProxy{
		log:          log,
		loopbackConn: loopback,
		upstreamConn: upstreamConn,
		upstream:     ua,
	}
	go p.loopFromPlugin()
	go p.loopFromUpstream()
	return p, nil
}

// loopFromPlugin reads loopback UDP from the plugin, captures its source
// address, and forwards to the upstream server via upstreamConn.
func (p *udpProxy) loopFromPlugin() {
	buf := make([]byte, udpDatagramMax)
	for {
		n, src, err := p.loopbackConn.ReadFromUDP(buf)
		if err != nil {
			if !errIsUseOfClosed(err) {
				p.log.Debug("udp loopback read error", "err", err)
			}
			return
		}
		p.pluginAddrMu.Lock()
		p.pluginAddr = src
		p.pluginAddrMu.Unlock()

		if _, err := p.upstreamConn.WriteToUDP(buf[:n], p.upstream); err != nil {
			p.log.Debug("udp upstream write error", "err", err)
		}
	}
}

// loopFromUpstream reads the upstream socket; any packet that arrives is
// assumed to be a reply from the server (the only thing we ever wrote to
// is the upstream UDP endpoint). Forward back to the plugin's last seen
// address.
func (p *udpProxy) loopFromUpstream() {
	buf := make([]byte, udpDatagramMax)
	for {
		n, _, err := p.upstreamConn.ReadFromUDP(buf)
		if err != nil {
			if !errIsUseOfClosed(err) {
				p.log.Debug("udp upstream read error", "err", err)
			}
			return
		}
		p.pluginAddrMu.RLock()
		dst := p.pluginAddr
		p.pluginAddrMu.RUnlock()
		if dst == nil {
			continue // server reply before plugin ever sent anything; drop
		}
		if _, err := p.loopbackConn.WriteToUDP(buf[:n], dst); err != nil {
			p.log.Debug("udp loopback write error", "err", err)
		}
	}
}

func (p *udpProxy) close() {
	if p.loopbackConn != nil {
		_ = p.loopbackConn.Close()
	}
	if p.upstreamConn != nil {
		_ = p.upstreamConn.Close()
	}
}

// errIsUseOfClosed checks for the "use of closed network connection" error.
func errIsUseOfClosed(err error) bool {
	if err == nil {
		return false
	}
	// Best-effort string match; net package does not export the sentinel.
	return err.Error() == "use of closed network connection" ||
		(net.ErrClosed != nil && err == net.ErrClosed)
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18900",
		"Local address to accept plugin connections on")
	upstream := flag.String("upstream", "ws://146.56.202.138:18900",
		"Remote Network Ultra Server URL")
	logLevel := flag.String("log-level", "info", "debug | info | warn | error")
	flag.Parse()

	log := setupLogger(*logLevel)
	log.Info("starting", "ws-listen", *listen, "upstream", *upstream)

	// Each plugin connection gets its own UDP proxy (ephemeral loopback +
	// upstream sockets), so multiple plugin instances on the same DAW —
	// e.g. one Send, one Recv — can each have an independent UDP binding
	// on the server. A single shared proxy would alias them: server would
	// bind only the latest hello's source addr and the others would
	// silently drop to WS fallback.

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handlePlugin(r.Context(), w, r, *upstream, log)
	})

	srv := &http.Server{
		Addr:        *listen,
		Handler:     mux,
		ReadTimeout: 0,
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

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

func handlePlugin(parent context.Context, w http.ResponseWriter, r *http.Request,
	upstreamURL string, log *slog.Logger) {
	plugin, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		Subprotocols:       []string{subprotocol},
	})
	if err != nil {
		log.Warn("plugin accept failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	defer plugin.Close(websocket.StatusInternalError, "shutting down")
	plugin.SetReadLimit(8 * 1024 * 1024)

	log.Info("plugin connected", "remote", r.RemoteAddr)

	// Per-connection UDP proxy: ephemeral loopback port (we'll tell the
	// plugin which one via the welcome rewrite) + ephemeral upstream
	// socket so the server treats each plugin as a distinct source.
	upstreamHost := extractHostFromWsURL(upstreamURL)
	upstreamUDP := net.JoinHostPort(extractBareHost(upstreamHost),
		strconv.Itoa(udpUpstreamPort))
	uproxy, err := newUDPProxy(log, "127.0.0.1:0", upstreamUDP)
	if err != nil {
		log.Warn("udp proxy init failed", "err", err)
		_ = plugin.Close(websocket.StatusInternalError, "udp proxy init")
		return
	}
	defer uproxy.close()
	udpAdvertise := uproxy.loopbackConn.LocalAddr().String()
	log.Info("udp proxy live for plugin",
		"plugin", r.RemoteAddr,
		"loopback", udpAdvertise, "upstream", upstreamUDP)

	dialCtx, dialCancel := context.WithTimeout(parent, upstreamDialTimeout)
	defer dialCancel()

	directHTTP := &http.Client{
		Transport: &http.Transport{Proxy: nil},
	}

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

	var sniffOnce atomic.Bool

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relayPassthrough(relayCtx, "plugin->upstream", plugin, upstream, log)
		relayCancel()
	}()
	go func() {
		defer wg.Done()
		relayWithRewrite(relayCtx, "upstream->plugin", upstream, plugin, udpAdvertise, &sniffOnce, log)
		relayCancel()
	}()
	wg.Wait()

	log.Info("session closed", "remote", r.RemoteAddr)
}

// relayPassthrough copies WS messages src→dst with no rewriting.
func relayPassthrough(ctx context.Context, dir string, src, dst *websocket.Conn, log *slog.Logger) {
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

// relayWithRewrite copies WS messages src→dst, but for the first text frame
// containing a `welcome` envelope it rewrites the `udpEndpoint` field to
// `udpAdvertise` so the plugin opens its UDP socket against us, not against
// the real server. After the first welcome we degrade to passthrough.
func relayWithRewrite(ctx context.Context, dir string, src, dst *websocket.Conn,
	udpAdvertise string, sniffOnce *atomic.Bool, log *slog.Logger) {
	for {
		mt, payload, err := src.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Debug(dir+" read closed", "err", err)
			}
			return
		}

		if mt == websocket.MessageText && !sniffOnce.Load() {
			if rewritten, ok := maybeRewriteWelcome(payload, udpAdvertise); ok {
				payload = rewritten
				sniffOnce.Store(true)
				log.Info("welcome udpEndpoint rewritten", "advertise", udpAdvertise)
			}
		}

		if err := dst.Write(ctx, mt, payload); err != nil {
			log.Debug(dir+" write failed", "err", err)
			return
		}
	}
}

// maybeRewriteWelcome decodes the JSON envelope; if it's a welcome with a
// non-empty udpEndpoint, replaces it with udpAdvertise. Returns the
// (possibly modified) payload and a bool telling the caller if rewriting
// happened.
func maybeRewriteWelcome(payload []byte, udpAdvertise string) ([]byte, bool) {
	// Use map[string]any to keep all unknown fields verbatim.
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		return payload, false
	}
	if t, _ := env["type"].(string); t != "welcome" {
		return payload, false
	}
	data, ok := env["data"].(map[string]any)
	if !ok {
		return payload, false
	}
	ep, _ := data["udpEndpoint"].(string)
	if ep == "" {
		return payload, false
	}
	data["udpEndpoint"] = udpAdvertise
	out, err := json.Marshal(env)
	if err != nil {
		return payload, false
	}
	return out, true
}

func extractHostFromWsURL(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// extractBareHost strips the ":port" suffix from "host:port" — used to
// build the upstream UDP address (which uses a fixed port, not the WS port).
func extractBareHost(hostPort string) string {
	if hostPort == "" {
		return ""
	}
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return h
}

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
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"network-ultra-bridge: local WS+UDP relay for firewalled DAW hosts.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n  %s [flags]\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
}

// Suppress unused-port-constant warning if only one of them ends up referenced.
var _ = udpListenPort
