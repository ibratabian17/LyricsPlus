package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/providers"
	"lyricsplus/backend/internal/service"
	"lyricsplus/backend/internal/storage"
)

type Admin struct {
	Store       *storage.Store
	Service     *service.Service
	ProviderSet *providers.Set
	Racer       *orchestrator.Racer
	JWTSecret   string
	Logger      *logger.Logger
}

func (h *Admin) Stats(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Storage is uninitialized",
		})
		return
	}

	stats, err := h.Store.AdminStats(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "Internal Server Error",
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

func (h *Admin) ListLyrics(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Storage is uninitialized",
		})
		return
	}

	v := r.URL.Query()
	qStr := strings.TrimSpace(v.Get("query"))
	if qStr == "" {
		qStr = strings.TrimSpace(v.Get("q"))
	}

	page, _ := strconv.Atoi(v.Get("page"))
	limit, _ := strconv.Atoi(v.Get("limit"))

	filter := storage.AdminListFilter{
		Query:      qStr,
		Source:     strings.TrimSpace(v.Get("source")),
		ISRC:       strings.TrimSpace(v.Get("isrc")),
		PlatformID: strings.TrimSpace(v.Get("platform_id")),
		Page:       page,
		Limit:      limit,
		SortBy:     strings.TrimSpace(v.Get("sort_by")),
		SortDir:    strings.TrimSpace(v.Get("sort_dir")),
	}

	res, err := h.Store.AdminList(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "Internal Server Error",
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, res)
}

func (h *Admin) GetLyric(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Storage is uninitialized",
		})
		return
	}

	idStr := chi.URLParam(r, "id")
	if idStr == "" {
		idStr = r.URL.Query().Get("id")
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid or missing ID parameter",
		})
		return
	}

	row, err := h.Store.AdminGetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"error":   "Not Found",
				"message": fmt.Sprintf("Lyric record with ID %d not found", id),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "Internal Server Error",
			"message": err.Error(),
		})
		return
	}

	var parsedContent interface{}
	if len(row.ContentJSON) > 0 {
		if err := json.Unmarshal(row.ContentJSON, &parsedContent); err != nil {
			parsedContent = string(row.ContentJSON)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":          row.ID,
		"filename":    row.Filename,
		"isrc":        row.ISRC,
		"platform_id": row.PlatformID,
		"source":      row.Source,
		"title":       row.Title,
		"artist":      row.Artist,
		"duration_ms": row.DurationMS,
		"created_at":  row.CreatedAt,
		"content":     parsedContent,
		"raw_content": string(row.ContentJSON),
	})
}

type AdminCreateRequest struct {
	Title       string          `json:"title"`
	Artist      string          `json:"artist"`
	Album       string          `json:"album"`
	ISRC        string          `json:"isrc"`
	PlatformID  string          `json:"platform_id"`
	Source      string          `json:"source"`
	DurationMS  int             `json:"duration_ms"`
	Duration    any             `json:"duration"`
	Content     json.RawMessage `json:"content"`
	ContentText string          `json:"content_text"`
}

func (h *Admin) CreateLyrics(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Storage is uninitialized",
		})
		return
	}

	var req AdminCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid JSON body: " + err.Error(),
		})
		return
	}

	title := strings.TrimSpace(req.Title)
	artist := strings.TrimSpace(req.Artist)
	if (title == "" || artist == "") && req.ISRC == "" && req.PlatformID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Either (title and artist) or isrc or platform_id is required",
		})
		return
	}

	durMS := req.DurationMS
	if durMS <= 0 && req.Duration != nil {
		durMS = parseDurationInput(req.Duration)
	}

	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "lyricsplus"
	}

	var contentBytes []byte
	if len(req.Content) > 0 {
		contentBytes = req.Content
	} else if req.ContentText != "" {
		contentBytes = []byte(req.ContentText)
	} else {
		// Default empty payload
		contentBytes = []byte("{}")
	}

	row := &storage.Row{
		Filename:    storage.CanonicalFilename(artist, title, req.Album, durMS, req.ISRC, req.PlatformID, "json"),
		ContentJSON: contentBytes,
		ISRC:        strings.TrimSpace(req.ISRC),
		PlatformID:  strings.TrimSpace(req.PlatformID),
		Source:      source,
		Title:       title,
		Artist:      artist,
		DurationMS:  durMS,
		CreatedAt:   time.Now(),
	}

	if err := h.Store.AdminCreate(r.Context(), row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "Internal Server Error",
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"success": true,
		"message": "Lyrics record created successfully",
		"id":      row.ID,
		"item":    row,
	})
}

type AdminUpdatePayload struct {
	Title       *string          `json:"title"`
	Artist      *string          `json:"artist"`
	ISRC        *string          `json:"isrc"`
	PlatformID  *string          `json:"platform_id"`
	Source      *string          `json:"source"`
	DurationMS  *int             `json:"duration_ms"`
	Duration    any              `json:"duration"`
	Content     *json.RawMessage `json:"content"`
	ContentText *string          `json:"content_text"`
}

func (h *Admin) UpdateLyric(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Storage is uninitialized",
		})
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid or missing ID parameter",
		})
		return
	}

	var req AdminUpdatePayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid JSON body: " + err.Error(),
		})
		return
	}

	var contentBytes []byte
	if req.Content != nil && len(*req.Content) > 0 {
		contentBytes = *req.Content
	} else if req.ContentText != nil {
		contentBytes = []byte(*req.ContentText)
	}

	durMS := req.DurationMS
	if durMS == nil && req.Duration != nil {
		parsed := parseDurationInput(req.Duration)
		durMS = &parsed
	}

	updateReq := storage.AdminUpdateRequest{
		Title:       req.Title,
		Artist:      req.Artist,
		ISRC:        req.ISRC,
		PlatformID:  req.PlatformID,
		Source:      req.Source,
		DurationMS:  durMS,
		ContentJSON: contentBytes,
	}

	updated, err := h.Store.AdminUpdate(r.Context(), id, updateReq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"error":   "Not Found",
				"message": fmt.Sprintf("Lyric record with ID %d not found", id),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "Internal Server Error",
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Lyrics record updated successfully",
		"id":      updated.ID,
		"item":    updated,
	})
}

func (h *Admin) DeleteLyric(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Storage is uninitialized",
		})
		return
	}

	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid or missing ID parameter",
		})
		return
	}

	if err := h.Store.AdminDelete(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"error":   "Not Found",
				"message": fmt.Sprintf("Lyric record with ID %d not found", id),
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "Internal Server Error",
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Lyric record with ID %d deleted successfully", id),
		"id":      id,
	})
}

type BatchDeleteRequest struct {
	IDs []int64 `json:"ids"`
}

func (h *Admin) BatchDeleteLyrics(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Storage is uninitialized",
		})
		return
	}

	var req BatchDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid JSON body: " + err.Error(),
		})
		return
	}

	if len(req.IDs) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Batch delete exceeds maximum allowed limit (maximum 100 items per request)",
		})
		return
	}

	deleted, err := h.Store.AdminDeleteBatch(r.Context(), req.IDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   "Internal Server Error",
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"deleted_count": deleted,
		"requested_ids": len(req.IDs),
	})
}

type ScrapeRequest struct {
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	Album       string   `json:"album"`
	ISRC        string   `json:"isrc"`
	PlatformID  string   `json:"platform_id"`
	Duration    any      `json:"duration"`
	Sources     []string `json:"sources"`
	Provider    string   `json:"provider"`
	ForceReload *bool    `json:"force_reload"`
	Save        *bool    `json:"save"`
}

func (h *Admin) ManualScrape(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req ScrapeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid JSON body: " + err.Error(),
		})
		return
	}

	title := strings.TrimSpace(req.Title)
	artist := strings.TrimSpace(req.Artist)
	album := strings.TrimSpace(req.Album)
	isrc := strings.TrimSpace(req.ISRC)
	platformID := strings.TrimSpace(req.PlatformID)

	if (title == "" || artist == "") && isrc == "" && platformID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Missing required search parameters: (title and artist) or isrc or platform_id",
		})
		return
	}

	if h.Service == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Lyrics Service is uninitialized",
		})
		return
	}

	durMS := parseDurationInput(req.Duration)

	q := domain.SearchQuery{
		Title:      title,
		Artist:     artist,
		Album:      album,
		ISRC:       isrc,
		PlatformID: platformID,
		Duration:   durMS,
	}

	// Sources selection
	var preferredSources []string
	if req.Provider != "" && !strings.EqualFold(req.Provider, "all") {
		preferredSources = []string{strings.TrimSpace(req.Provider)}
	} else if len(req.Sources) > 0 {
		for _, s := range req.Sources {
			if s = strings.TrimSpace(s); s != "" {
				preferredSources = append(preferredSources, s)
			}
		}
	}

	forceReload := true
	if req.ForceReload != nil {
		forceReload = *req.ForceReload
	}

	saveToDB := true
	if req.Save != nil {
		saveToDB = *req.Save
	}

	resp, err := h.Service.FetchLyrics(r.Context(), q, preferredSources, forceReload)
	elapsed := time.Since(start)

	if err != nil {
		var nf *service.NotFoundError
		if errors.As(err, &nf) {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"success":          false,
				"found":            false,
				"message":          nf.Message,
				"searched_sources": nf.Sources,
				"sources_status":   nf.SourceStatuses,
				"elapsed_ms":       elapsed.Milliseconds(),
				"query": map[string]interface{}{
					"title":       title,
					"artist":      artist,
					"album":       album,
					"isrc":        isrc,
					"platform_id": platformID,
					"duration_ms": durMS,
				},
			})
			return
		}

		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success":    false,
			"error":      "Scrape Error",
			"message":    err.Error(),
			"elapsed_ms": elapsed.Milliseconds(),
		})
		return
	}

	winnerSource := ""
	if resp.ProcessingTime != nil && resp.ProcessingTime.WinnerSource != nil {
		winnerSource = *resp.ProcessingTime.WinnerSource
	} else if resp.Metadata.Source != "" {
		winnerSource = resp.Metadata.Source
	}

	var dbRowID int64
	var dbFilename string
	var savedToDB bool

	if saveToDB && h.Store != nil {
		contentJSON, marshalErr := json.Marshal(resp)
		if marshalErr == nil {
			source := winnerSource
			if source == "" {
				source = resp.Metadata.Source
			}
			row := &storage.Row{
				Filename:    storage.CanonicalFilename(artist, title, album, durMS, isrc, platformID, extForSource(source)),
				ContentJSON: contentJSON,
				ISRC:        isrc,
				PlatformID:  platformID,
				Source:      source,
				Title:       title,
				Artist:      artist,
				DurationMS:  durMS,
				CreatedAt:   time.Now(),
			}
			if saveErr := h.Store.SaveLyrics(r.Context(), row); saveErr == nil {
				savedToDB = true
				dbRowID = row.ID
				dbFilename = row.Filename
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":       true,
		"found":         true,
		"winner_source": winnerSource,
		"saved_to_db":   savedToDB,
		"db_id":         dbRowID,
		"filename":      dbFilename,
		"elapsed_ms":    elapsed.Milliseconds(),
		"metadata":      resp.Metadata,
		"lyrics":        resp.Lyrics,
		"sync_type":     resp.Type,
		"lyrics_count":  len(resp.Lyrics),
		"raw_data":      resp.RawData,
		"timing":        resp.ProcessingTime,
	})
}

func (h *Admin) ScrapeSources(w http.ResponseWriter, r *http.Request) {
	if h.Racer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "Service Unavailable",
			"message": "Orchestrator racer is uninitialized",
		})
		return
	}

	var req ScrapeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Invalid JSON body: " + err.Error(),
		})
		return
	}

	title := strings.TrimSpace(req.Title)
	artist := strings.TrimSpace(req.Artist)
	album := strings.TrimSpace(req.Album)
	isrc := strings.TrimSpace(req.ISRC)
	platformID := strings.TrimSpace(req.PlatformID)

	if (title == "" || artist == "") && isrc == "" && platformID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   "Bad Request",
			"message": "Missing required search parameters: (title and artist) or isrc or platform_id",
		})
		return
	}

	durMS := parseDurationInput(req.Duration)

	q := domain.SearchQuery{
		Title:      title,
		Artist:     artist,
		Album:      album,
		ISRC:       isrc,
		PlatformID: platformID,
		Duration:   durMS,
	}

	targetSources := req.Sources
	if len(targetSources) == 0 {
		targetSources = []string{"apple", "musixmatch", "deezer", "spotify", "qq"}
	}

	type SourceResult struct {
		Source      string                 `json:"source"`
		Status      string                 `json:"status"`
		ElapsedMs   int64                  `json:"elapsed_ms"`
		SyncType    string                 `json:"sync_type,omitempty"`
		LyricsCount int                    `json:"lyrics_count,omitempty"`
		Error       string                 `json:"error,omitempty"`
		Lyrics      []domain.Line          `json:"lyrics,omitempty"`
		Metadata    *domain.LyricsMetadata `json:"metadata,omitempty"`
	}

	results := make(map[string]SourceResult, len(targetSources))

	for _, src := range targetSources {
		src = strings.TrimSpace(src)
		if src == "" {
			continue
		}
		t0 := time.Now()
		subCtx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		res := h.Racer.TryFetch(subCtx, src, q)
		cancel()

		sr := SourceResult{
			Source:    src,
			ElapsedMs: time.Since(t0).Milliseconds(),
		}
		if res == nil {
			sr.Status = "NO_RESPONSE"
		} else if res.Err != nil {
			sr.Status = "ERROR"
			sr.Error = res.Err.Error()
		} else if res.Resp != nil && len(res.Resp.Lyrics) > 0 {
			sr.Status = "OK"
			sr.SyncType = string(res.Resp.Type)
			sr.LyricsCount = len(res.Resp.Lyrics)
			sr.Lyrics = res.Resp.Lyrics
			sr.Metadata = &res.Resp.Metadata
		} else {
			sr.Status = res.Status
			if sr.Status == "" {
				sr.Status = "NOT_FOUND"
			}
		}
		results[src] = sr
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"query": map[string]interface{}{
			"title":       title,
			"artist":      artist,
			"album":       album,
			"isrc":        isrc,
			"platform_id": platformID,
			"duration_ms": durMS,
		},
		"results": results,
	})
}

func parseDurationInput(v any) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		if val > 0 && val < 1000 {
			return int(val * 1000)
		}
		return int(val)
	case int:
		if val > 0 && val < 1000 {
			return val * 1000
		}
		return val
	case string:
		s := strings.TrimSpace(val)
		if strings.Contains(s, ":") {
			parts := strings.Split(s, ":")
			if len(parts) == 2 {
				m, _ := strconv.Atoi(parts[0])
				sec, _ := strconv.ParseFloat(parts[1], 64)
				return int((float64(m)*60 + sec) * 1000)
			}
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
			if f < 1000 {
				return int(f * 1000)
			}
			return int(f)
		}
	}
	return 0
}

func extForSource(source string) string {
	s := strings.ToLower(strings.TrimSpace(source))
	if strings.HasPrefix(s, "lyrics+") || strings.HasPrefix(s, "lyricsplus") {
		return "json"
	}
	switch s {
	case "apple", "apple music":
		return "ttml"
	case "qq", "qq music", "qqmusic":
		return "qrc"
	default:
		if strings.Contains(s, "apple") && !strings.Contains(s, "lyrics") {
			return "ttml"
		}
		return "json"
	}
}
