// Package handlers contains the HTTP handlers implementing the LyricsPlus API.
package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/providers"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// Catalog serves /v1/songlist/search and /v1/metadata/get.
type Catalog struct {
	AppleMusic *providers.AppleMusicProvider
	Spotify    *providers.SpotifyProvider
	Musixmatch *providers.MusixmatchProvider
	Logger     *logger.Logger
}

// Search aggregates song catalog results from Apple, Spotify and Musixmatch.
func (h *Catalog) Search(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing required parameter: q (query)"})
		return
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	var results []domain.SongCatalogItem
	agg := func(items []domain.SongCatalogItem, err error) {
		if err != nil {
			h.logf("catalog search %q failed: %v", q, err)
			return
		}
		results = append(results, items...)
	}

	agg(h.AppleMusic.SearchCatalog(ctx, q))
	agg(h.Spotify.SearchCatalog(ctx, q))
	agg(h.Musixmatch.SearchCatalog(ctx, q))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results,
		"processingTime": map[string]int64{
			"timeElapsed":   time.Since(start).Milliseconds(),
			"lastProcessed": time.Now().UnixMilli(),
		},
	})
}

// Metadata returns detailed Apple Music metadata for a track.
func (h *Catalog) Metadata(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	if title == "" || artist == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing required parameters: title and artist"})
		return
	}
	album := strings.TrimSpace(r.URL.Query().Get("album"))
	var durationSec float64
	if ds := strings.TrimSpace(r.URL.Query().Get("duration")); ds != "" {
		durationSec, _ = strconv.ParseFloat(ds, 64)
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	metadata, err := h.AppleMusic.GetMetadata(ctx, title, artist, album, durationSec)
	if err != nil {
		h.logf("metadata fetch failed for %q - %q: %v", artist, title, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Server Error"})
		return
	}
	if metadata == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Could not find metadata"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"metadata": metadata})
}

func (h *Catalog) logf(format string, args ...interface{}) {
	if h.Logger != nil {
		h.Logger.Errorf(format, args...)
	}
}
