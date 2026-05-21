package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/GeekASMR/network-ultra-server/internal/metrics"
	"github.com/GeekASMR/network-ultra-server/internal/room"
)

type HealthHandler struct {
	Reg     *room.Registry
	Started time.Time
	Version string
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"status":  "ok",
		"uptime":  int(time.Since(h.Started).Seconds()),
		"rooms":   h.Reg.CountRooms(),
		"peers":   h.Reg.CountPeers(),
		"version": h.Version,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

type MetricsHandler struct {
	Reg *metrics.Registry
}

func (h *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	h.Reg.WriteText(w)
}
