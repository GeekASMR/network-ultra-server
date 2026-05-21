package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GeekASMR/network-ultra-server/internal/config"
	"github.com/GeekASMR/network-ultra-server/internal/httpapi"
	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/room"
	"github.com/GeekASMR/network-ultra-server/internal/ws"
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
