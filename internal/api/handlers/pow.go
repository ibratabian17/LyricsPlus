package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/providers"
	"lyricsplus/backend/internal/providers/lyricsplus"
	"lyricsplus/backend/internal/storage"
)

type Pow struct {
	Issuer           *lyricsplus.Issuer
	Verifier         *lyricsplus.Verifier
	LyricsPlus       *providers.LyricsPlusProvider
	Store            *storage.Store
	GDrive           *storage.GDriveClient
	MaxBodyBytes     int64
	AcceptVandal     bool
	AllowSubmissions bool
	Logger           *logger.Logger

	background sync.WaitGroup
}

func (h *Pow) WaitBackground(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		h.background.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Pow) GetChallenge(w http.ResponseWriter, r *http.Request) {
	if h.Issuer == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Server configuration error"})
		return
	}
	token, _, err := h.Issuer.Issue(time.Now())
	if err != nil {
		h.logf("challenge issue failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Server configuration error"})
		return
	}
	difficulty := 0
	if h.Verifier != nil {
		difficulty = h.Verifier.Difficulty()
	}
	h.noStore(w)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      token,
		"difficulty": difficulty,
	})
}

type submitPayload struct {
	ProofOfWorkToken string          `json:"proofOfWorkToken"`
	Nonce            string          `json:"nonce"`
	SongTitle        string          `json:"songTitle"`
	SongArtist       string          `json:"songArtist"`
	SongAlbum        string          `json:"songAlbum"`
	SongDuration     string          `json:"songDuration"`
	SongISRC         string          `json:"songISRC"`
	SongPlatformID   string          `json:"songPlatformId"`
	LyricsData       json.RawMessage `json:"lyricsData"`
	ForceUpload      bool            `json:"forceUpload"`
	ForceUploadSnake bool            `json:"force_upload"`
}

func (p *submitPayload) isForceUpload() bool {
	return p.ForceUpload || p.ForceUploadSnake
}

func (h *Pow) Submit(w http.ResponseWriter, r *http.Request) {
	if !h.AllowSubmissions {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Submissions are currently disabled"})
		return
	}

	limit := h.MaxBodyBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Unable to read request body"})
		return
	}
	if int64(len(body)) > limit {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Request body too large"})
		return
	}

	var payload submitPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON payload"})
		return
	}
	if payload.ProofOfWorkToken == "" || payload.Nonce == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing proof of work"})
		return
	}
	if h.Verifier == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Server configuration error"})
		return
	}
	if _, err := h.Verifier.Verify(payload.ProofOfWorkToken, payload.Nonce); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid proof of work solution"})
		return
	}

	if strings.TrimSpace(payload.SongTitle) == "" || strings.TrimSpace(payload.SongArtist) == "" ||
		strings.TrimSpace(payload.SongDuration) == "" || len(payload.LyricsData) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing required parameters"})
		return
	}

	var next domain.LyricsResponse
	if err := json.Unmarshal(payload.LyricsData, &next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid lyrics data"})
		return
	}
	norm := parsers.NormalizeV2(&next)

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	durMS := durationToMS(payload.SongDuration)
	query := domain.SearchQuery{
		Title:      strings.TrimSpace(payload.SongTitle),
		Artist:     strings.TrimSpace(payload.SongArtist),
		Album:      strings.TrimSpace(payload.SongAlbum),
		Duration:   durMS,
		ISRC:       strings.TrimSpace(payload.SongISRC),
		PlatformID: strings.TrimSpace(payload.SongPlatformID),
	}
	content, err := json.Marshal(norm)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Serialization failed"})
		return
	}
	fileName := storage.CanonicalFilename(query.Artist, query.Title, query.Album, query.Duration, query.ISRC, query.PlatformID, "json")

	// Check if lyricsplus lyrics already exist for this track
	existingLyrics := h.findExistingLyricsPlus(ctx, query)
	if existingLyrics != nil && !payload.isForceUpload() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Lyrics already exist for this track. Use forceUpload to overwrite.",
		})
		return
	}

	if !h.AcceptVandal && providers.IsVandalismUpdate(existingLyrics, norm) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Suspected vandalism"})
		return
	}

	if h.Store != nil {
		h.Store.SaveUserLyricsAsync(query, content, 15*time.Second)
	}
	if h.GDrive != nil && h.GDrive.IsConfigured() {
		h.background.Add(1)
		go func() {
			defer h.background.Done()
			guctx, c := contextWithTimeoutRaw(30 * time.Second)
			defer c()
			if _, err := h.GDrive.UploadUserLyrics(guctx, fileName, content); err != nil {
				h.logf("submit upload to gdrive failed: %v", err)
			}
		}()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"message":  "Lyrics submitted successfully",
		"filename": fileName,
	})
}

func (h *Pow) findExistingLyricsPlus(ctx context.Context, q domain.SearchQuery) *domain.LyricsResponse {
	if h.LyricsPlus != nil {
		if res, ok := h.LyricsPlus.GetUserLyrics(ctx, q); ok && res != nil {
			return res
		}
		return nil
	}
	if h.Store != nil {
		lp := providers.NewLyricsPlus(nil)
		lp.SetStore(h.Store)
		if res, ok := lp.GetUserLyrics(ctx, q); ok && res != nil {
			return res
		}
	}
	return nil
}

func durationToMS(ds string) int {
	if sec, err := strconv.ParseFloat(strings.TrimSpace(ds), 64); err == nil {
		return int(sec * 1000)
	}
	return 0
}

func contextWithTimeoutRaw(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func (h *Pow) noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func (h *Pow) logf(format string, args ...interface{}) {
	if h.Logger != nil {
		h.Logger.Errorf(format, args...)
	}
}
