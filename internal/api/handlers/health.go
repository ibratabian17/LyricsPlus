package handlers

import (
	"context"
	"net/http"
	"time"

	"lyricsplus/backend/internal/version"
)

// StorePinger is the minimal store surface the health handler needs.
type StorePinger interface {
	Ping(ctx context.Context) error
}

// Health serves /health (liveness) and /readyz (readiness).
type Health struct {
	Store   StorePinger
	Started time.Time
	Version string
}

// Handle reports liveness: always 200 while the process is running.
func (h *Health) Handle(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, false)
}

// Ready reports readiness: 200 when the backing cache is reachable, else 503.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, true)
}

func (h *Health) write(w http.ResponseWriter, r *http.Request, ready bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	versionStr := h.Version
	if versionStr == "" {
		versionStr = version.Version
	}

	status := "ok"
	code := http.StatusOK
	dbStatus := "ok"
	if h.Store != nil {
		if err := h.Store.Ping(ctx); err != nil {
			dbStatus = "unavailable"
			if ready {
				status = "degraded"
				code = http.StatusServiceUnavailable
			}
		}
	} else {
		dbStatus = "not_configured"
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, map[string]interface{}{
		"service":       "lyricsplus-backend",
		"status":        status,
		"version":       versionStr,
		"commit":        version.Commit,
		"buildDate":     version.BuildDate,
		"uptimeSeconds": int64(time.Since(h.Started).Seconds()),
		"database":      dbStatus,
	})
}
