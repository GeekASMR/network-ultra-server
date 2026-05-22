package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/GeekASMR/network-ultra-server/internal/config"
	"github.com/GeekASMR/network-ultra-server/internal/httpapi"
	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/room"
	udpserver "github.com/GeekASMR/network-ultra-server/internal/udp"
	"github.com/GeekASMR/network-ultra-server/internal/ws"

	"golang.org/x/crypto/bcrypt"
)

const (
	buildVersion = "1.0.0"
)

func main() {
	cfgPath := flag.String("config", "/etc/network-ultra/config.toml", "path to config TOML")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("network-ultra-server", buildVersion)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}

	log := setupLogger(cfg.Log)
	log.Info("starting", "version", buildVersion, "config", *cfgPath)

	mreg := metrics.NewRegistry()
	rreg := room.NewRegistry(cfg.Server.MaxRooms, cfg.Server.MaxPeersPerRoom)

	broker := ws.NewDeltaBroker()
	rreg.SetDeltaListener(func(d room.RoomListDelta) {
		broker.Publish(d)
	})
	_ = broker

	wsServer := &ws.Server{
		Reg:            rreg,
		Metrics:        mreg,
		Log:            log,
		MaxConnections: cfg.Server.MaxConnections,
		Subprotocol:    "network-ultra-v1",
	}

	// Server-level password gating (v1.3+). When set in config, hash it once
	// at startup so per-connection bcrypt compares are the only crypto cost.
	// Empty config.Server.Password = open server (legacy).
	if cfg.Server.Password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Server.Password), bcrypt.DefaultCost)
		if err != nil {
			log.Error("hash server password failed", "err", err)
			os.Exit(1)
		}
		wsServer.ServerPasswordHash = hash
		log.Info("server password gating enabled (clients must supply matching password in hello)")
	}

	// UDP data plane (optional). When configured, the WS welcome will
	// advertise the UDP endpoint + token, and clients route audio over
	// UDP to dodge TCP head-of-line blocking on long-RTT links.
	var udpSrv *udpserver.Server
	if cfg.Server.UdpListen != "" {
		udpSrv = udpserver.NewServer(log, mreg)
		if err := udpSrv.Listen(cfg.Server.UdpListen); err != nil {
			log.Error("udp listen failed; continuing without UDP", "err", err)
			udpSrv = nil
		} else {
			defer udpSrv.Close()
			wsServer.Udp = udpSrv
			// Two ways to tell clients where to send UDP:
			//   1. Static UdpEndpoint (admin override; only useful behind
			//      a load-balancer / CDN where the public host differs).
			//   2. Derive from the HTTP Host the client used. This is the
			//      default and handles cloud-NAT correctly without the
			//      operator needing to figure out their public IP.
			udpPort := udpPortFromListen(cfg.Server.UdpListen)
			wsServer.UdpPort = udpPort
			if cfg.Server.UdpAdvertiseHost != "" && udpPort > 0 {
				wsServer.UdpEndpoint = net.JoinHostPort(
					cfg.Server.UdpAdvertiseHost,
					strconv.Itoa(udpPort))
				log.Info("udp data plane enabled", "advertise", wsServer.UdpEndpoint)
			} else {
				log.Info("udp data plane enabled",
					"port", udpPort,
					"advertise", "auto-derive from HTTP Host")
			}
		}
	}

	// HTTP mux for WS upgrade.
	mux := http.NewServeMux()
	mux.HandleFunc("/", wsServer.HandleHTTP)

	wsHTTP := &http.Server{
		Addr:        cfg.Server.Listen,
		Handler:     mux,
		ReadTimeout: 0, // long-lived
		IdleTimeout: 120 * time.Second,
	}

	// Health + metrics on a separate listener (default 127.0.0.1:18901).
	healthMux := http.NewServeMux()
	healthMux.Handle("/healthz", &httpapi.HealthHandler{
		Reg:     rreg,
		Started: time.Now(),
		Version: buildVersion,
	})
	healthMux.Handle("/metrics", &httpapi.MetricsHandler{Reg: mreg})
	healthHTTP := &http.Server{
		Addr:    cfg.Server.HealthListen,
		Handler: healthMux,
	}

	// Run.
	errCh := make(chan error, 2)
	go func() {
		log.Info("ws listening", "addr", cfg.Server.Listen, "tls", cfg.TLS.Enabled)
		var err error
		if cfg.TLS.Enabled && !cfg.TLS.AutoLetsEncrypt {
			err = wsHTTP.ListenAndServeTLS(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			err = wsHTTP.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		log.Info("health listening", "addr", cfg.Server.HealthListen)
		err := healthHTTP.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Wait for SIGTERM/SIGINT.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		log.Info("shutting down", "signal", sig.String())
	case e := <-errCh:
		log.Error("server failed", "err", e)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = wsHTTP.Shutdown(shutdownCtx)
	_ = healthHTTP.Shutdown(shutdownCtx)
	log.Info("bye")
}

// udpPortFromListen extracts the port number from a listen address like
// "0.0.0.0:18902" or ":18902". Returns 0 on parse failure.
func udpPortFromListen(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

// resolveUdpEndpoint is unused now; UDP endpoint advertisement happens
// per-connection in ws.dispatch.go using r.Host (the HTTP Host header)
// so cloud-NAT'd servers don't need to figure out their public IP. Kept
// only as a stub so older callers (none in this binary) compile.
var _ = func() string { return "" }

func setupLogger(c config.LogCfg) *slog.Logger {
	var level slog.Level
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if c.Format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
