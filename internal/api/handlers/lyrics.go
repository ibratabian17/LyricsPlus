package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/service"
)

const (
	cacheControlPublic  = "public, max-age=3600, s-maxage=86400, immutable"
	cacheControlNoStore = "no-store"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var errMissingRequired = errors.New("Missing required parameters: (title and artist) or isrc or platformId")

type lyricsParams struct {
	query       domain.SearchQuery
	sources     []string
	forceReload bool
}

// Lyrics serves the /v1/lyrics/get, /v2/lyrics/get, /v1/ttml/get and /v1/raw/get endpoints.
type Lyrics struct {
	Service  *service.Service
	Logger   *logger.Logger
	KpoeInfo string
}

func (h *Lyrics) parseParams(r *http.Request) (*lyricsParams, error) {
	v := r.URL.Query()
	title := strings.TrimSpace(v.Get("title"))
	artist := strings.TrimSpace(v.Get("artist"))
	album := strings.TrimSpace(v.Get("album"))
	isrc := strings.TrimSpace(v.Get("isrc"))
	platformID := strings.TrimSpace(v.Get("platformId"))

	if (title == "" || artist == "") && isrc == "" && platformID == "" {
		return nil, errMissingRequired
	}

	var durMS int
	if ds := strings.TrimSpace(v.Get("duration")); ds != "" {
		if sec, err := strconv.ParseFloat(ds, 64); err == nil {
			durMS = int(sec * 1000)
		}
	}

	var sources []string
	if s := strings.TrimSpace(v.Get("source")); s != "" {
		for _, p := range strings.Split(s, ",") {
			if t := strings.TrimSpace(p); t != "" {
				sources = append(sources, t)
			}
		}
	}

	return &lyricsParams{
		query: domain.SearchQuery{
			Title:      title,
			Artist:     artist,
			Album:      album,
			Duration:   durMS,
			ISRC:       isrc,
			PlatformID: platformID,
			Sources:    sources,
		},
		sources:     sources,
		forceReload: v.Get("forceReload") == "true",
	}, nil
}

func (h *Lyrics) setProcessing(start time.Time, resp *domain.LyricsResponse) {
	if resp.ProcessingTime == nil {
		resp.ProcessingTime = &domain.ProcessTiming{}
	}
	resp.ProcessingTime.TimeElapsed = time.Since(start).Milliseconds()
	resp.ProcessingTime.LastProcessed = time.Now().UnixMilli()
	if resp.ProcessingTime.TotalElapsedMs == 0 {
		resp.ProcessingTime.TotalElapsedMs = resp.ProcessingTime.TimeElapsed
	}
}

func (h *Lyrics) ensureKpoe(resp *domain.LyricsResponse) {
	if resp.KpoeTools == "" {
		resp.KpoeTools = h.KpoeInfo
	}
}

// GetV2 returns the canonical V2 payload.
func (h *Lyrics) GetV2(w http.ResponseWriter, r *http.Request) {
	p, err := h.parseParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	start := time.Now()
	resp, err := h.Service.FetchLyrics(r.Context(), p.query, p.sources, p.forceReload)
	if h.logFetch(r, "v2", p.query, start, resp, err) {
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Server Error"})
		return
	}
	if resp == nil {
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Lyrics not found"})
		return
	}
	h.ensureKpoe(resp)
	h.setProcessing(start, resp)
	w.Header().Set("Cache-Control", cacheControlPublic)
	writeJSON(w, http.StatusOK, resp)
}

// GetV1 returns the flat V1 (syllable segment) format.
func (h *Lyrics) GetV1(w http.ResponseWriter, r *http.Request) {
	p, err := h.parseParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	start := time.Now()
	resp, err := h.Service.FetchLyrics(r.Context(), p.query, p.sources, p.forceReload)
	if h.logFetch(r, "v1", p.query, start, resp, err) {
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Server Error"})
		return
	}
	if resp == nil {
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Lyrics not found"})
		return
	}
	h.ensureKpoe(resp)
	v1 := parsers.V2ToV1(resp)
	if v1.ProcessingTime == nil {
		v1.ProcessingTime = &domain.ProcessTiming{}
	}
	if resp.ProcessingTime != nil {
		v1.ProcessingTime.WinnerSource = resp.ProcessingTime.WinnerSource
		v1.ProcessingTime.SyncPriority = resp.ProcessingTime.SyncPriority
		v1.ProcessingTime.SourcesStatus = resp.ProcessingTime.SourcesStatus
		v1.ProcessingTime.SelectedSongMetadata = resp.ProcessingTime.SelectedSongMetadata
	}
	v1.ProcessingTime.TimeElapsed = time.Since(start).Milliseconds()
	v1.ProcessingTime.LastProcessed = time.Now().UnixMilli()
	if v1.ProcessingTime.TotalElapsedMs == 0 {
		v1.ProcessingTime.TotalElapsedMs = v1.ProcessingTime.TimeElapsed
	}
	w.Header().Set("Cache-Control", cacheControlPublic)
	writeJSON(w, http.StatusOK, v1)
}

// GetTTML returns the payload serialized as Apple Music TTML inside a JSON envelope.
func (h *Lyrics) GetTTML(w http.ResponseWriter, r *http.Request) {
	p, err := h.parseParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	start := time.Now()
	resp, err := h.Service.FetchLyrics(r.Context(), p.query, p.sources, p.forceReload)
	if h.logFetch(r, "ttml", p.query, start, resp, err) {
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Server Error"})
		return
	}
	if resp == nil {
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Lyrics not found"})
		return
	}
	h.ensureKpoe(resp)
	xmlData, err := parsers.JSONToTTML(resp)
	if err != nil {
		h.logf("ttml conversion failed: %v", err)
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "TTML conversion failed"})
		return
	}
	h.setProcessing(start, resp)
	w.Header().Set("Cache-Control", cacheControlPublic)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ttml":           string(xmlData),
		"processingTime": resp.ProcessingTime,
	})
}

// GetRaw returns the raw source payload verbatim.
func (h *Lyrics) GetRaw(w http.ResponseWriter, r *http.Request) {
	p, err := h.parseParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	start := time.Now()
	raw, err := h.Service.FetchRaw(r.Context(), p.query, p.sources, p.forceReload)
	if err != nil {
		h.logf("raw fetch failed: %v", err)
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal Server Error"})
		return
	}
	if raw == nil || raw.Raw == "" {
		w.Header().Set("Cache-Control", cacheControlNoStore)
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":  "Raw data is not available for this result",
			"source": "unknown",
		})
		return
	}

	contentType := "application/octet-stream"
	switch strings.ToLower(strings.ReplaceAll(raw.Source, "-word", "")) {
	case "apple", "qq":
		contentType = "application/xml"
	case "musixmatch", "spotify":
		contentType = "application/json"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControlPublic)
	w.Header().Set("X-Lyrics-Source", raw.Source)
	w.Header().Set("X-Processing-Time", strconv.FormatInt(time.Since(start).Milliseconds(), 10)+"ms")
	_, _ = w.Write([]byte(raw.Raw))
}

// logFetch reports the outcome and returns true when err is non-nil.
func (h *Lyrics) logFetch(r *http.Request, format string, q domain.SearchQuery, start time.Time, resp *domain.LyricsResponse, err error) bool {
	if err != nil {
		h.logErrf("lyrics fetch error format=%s artist=%q title=%q: %v", format, q.Artist, q.Title, err)
		return true
	}
	if h.Logger == nil {
		return false
	}
	source := "none"
	if resp != nil {
		source = resp.Metadata.Source
	}
	h.Logger.Infof("lyrics format=%s artist=%q title=%q source=%s took=%s",
		format, q.Artist, q.Title, source, time.Since(start).Round(time.Millisecond))
	return false
}

func (h *Lyrics) logf(format string, args ...interface{}) {
	if h.Logger != nil {
		h.Logger.Infof(format, args...)
	}
}

func (h *Lyrics) logErrf(format string, args ...interface{}) {
	if h.Logger != nil {
		h.Logger.Errorf(format, args...)
	}
}
