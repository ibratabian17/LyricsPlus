package handlers

import (
	"context"
	"net/http"
	"time"

	"lyricsplus/backend/internal/metrics"
	"lyricsplus/backend/internal/version"
)

type StorePinger interface {
	Ping(ctx context.Context) error
}

type Health struct {
	Store     StorePinger
	Started   time.Time
	Version   string
	BuildDate string
}

func (h *Health) Handle(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, false)
}

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
	buildDateStr := h.BuildDate
	if buildDateStr == "" {
		buildDateStr = version.BuildDate
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

	report := metrics.Default.Snapshot()

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, map[string]interface{}{
		"service":       "lyricsplus-backend",
		"status":        status,
		"version":       versionStr,
		"commit":        version.Commit,
		"buildDate":     buildDateStr,
		"uptimeSeconds": int64(time.Since(h.Started).Seconds()),
		"database":      dbStatus,
		"requests":      report.Requests,
		"lyrics":        report.Lyrics,
		"platforms":     report.Platforms,
	})
}
