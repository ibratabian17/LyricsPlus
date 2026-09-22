package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lyricsplus/backend/internal/api/handlers"
	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/providers/lyricsplus"
	"lyricsplus/backend/internal/service"
	"lyricsplus/backend/internal/storage"
)

// fakeSource stands in for a real provider so the full HTTP stack can be
// exercised black-box without any upstream credentials.
type fakeSource struct {
	resp *domain.LyricsResponse
	err  error
	name string
}

func (f *fakeSource) Name() string {
	if f.name != "" {
		return f.name
	}
	return "spotify"
}

func (f *fakeSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	return f.resp, f.err
}

func baseTestConfig() config.Config {
	cfg := config.Config{}
	cfg.Server.Addr = "3000"
	cfg.Server.MaxURLBytes = 4096
	cfg.Server.MaxQueryParams = 20
	cfg.Server.MaxQueryValueLen = 500
	cfg.Server.MaxConcurrency = 100
	cfg.RateLimit.Requests = 10000
	cfg.RateLimit.Window = time.Minute
	cfg.Provider.Timeout = 2 * time.Second
	cfg.LyricsPlus.MaxBodyBytes = 1 << 20
	cfg.LyricsPlus.AllowSubmissions = true
	cfg.LyricsPlus.JWTSecret = "test-jwt-secret"
	cfg.LyricsPlus.ChallengeTTL = 10 * time.Minute
	cfg.LyricsPlus.PoWDifficulty = 2
	return cfg
}

func sampleWordLyrics() *domain.LyricsResponse {
	return &domain.LyricsResponse{
		Type: domain.SyncTypeWord,
		Metadata: domain.LyricsMetadata{
			Source:      "Spotify",
			Title:       "Hello",
			Artist:      "Adele",
			SongWriters: []string{"Adele"},
			Language:    "en",
		},
		Lyrics: []domain.Line{
			{
				Time:     0,
				Duration: 5000,
				Text:     "Hello, it's me",
				Element:  domain.LineElement{Key: "L1", Singer: "v1"},
				Syllabus: []domain.Syllable{
					{Time: 0, Duration: 300, Text: "Hello,"},
					{Time: 300, Duration: 200, Text: "it's"},
					{Time: 500, Duration: 250, Text: "me"},
				},
			},
		},
		Cached: domain.CacheNone,
	}
}

func buildTestRouter(t *testing.T, cfg config.Config, f *fakeSource) http.Handler {
	t.Helper()
	racer := orchestrator.NewRacer([]orchestrator.Source{f}, cfg.Provider.Timeout)
	dedup := orchestrator.NewDedup(racer)
	svc := &service.Service{Dedup: dedup, Logger: nil}

	lyricsH := &handlers.Lyrics{Service: svc, Logger: nil, KpoeInfo: "lyricsplus"}
	catalogH := &handlers.Catalog{Logger: nil}
	powH := &handlers.Pow{
		Issuer:           lyricsplus.NewIssuer(cfg.LyricsPlus.JWTSecret, cfg.LyricsPlus.ChallengeTTL),
		Verifier:         lyricsplus.NewVerifier(cfg.LyricsPlus.JWTSecret, cfg.LyricsPlus.PoWDifficulty),
		MaxBodyBytes:     cfg.LyricsPlus.MaxBodyBytes,
		AcceptVandal:     cfg.LyricsPlus.AcceptVandalism,
		AllowSubmissions: cfg.LyricsPlus.AllowSubmissions,
		Logger:           nil,
	}
	healthH := &handlers.Health{Started: time.Now()}

	return newRouter(cfg, nil, lyricsH, catalogH, powH, healthH)
}

func doGet(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("invalid JSON response: %v\nbody=%s", err, rec.Body.String())
	}
}

func TestLyricsGetV2Success(t *testing.T) {
	cfg := baseTestConfig()
	h := buildTestRouter(t, cfg, &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v2/lyrics/get?title=Hello&artist=Adele&duration=232.123&source=spotify")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("expected public cache control, got %q", cc)
	}
	var resp domain.LyricsResponse
	decodeJSON(t, rec, &resp)
	if len(resp.Lyrics) != 1 || resp.Lyrics[0].Text != "Hello, it's me" {
		t.Fatalf("unexpected lyrics payload: %+v", resp.Lyrics)
	}
	if resp.Type != domain.SyncTypeWord {
		t.Fatalf("expected Word sync type, got %q", resp.Type)
	}
	if resp.Metadata.Source != "Spotify" {
		t.Fatalf("expected Spotify source metadata, got %q", resp.Metadata.Source)
	}
	if resp.ProcessingTime == nil {
		t.Fatalf("expected processingTime")
	}
	if resp.ProcessingTime.WinnerSource == nil || *resp.ProcessingTime.WinnerSource != "spotify" {
		t.Fatalf("expected winnerSource inside processingTime, got %+v", resp.ProcessingTime.WinnerSource)
	}
	if resp.ProcessingTime.SourcesStatus == nil || resp.ProcessingTime.SourcesStatus["spotify"].Status != "OK" {
		t.Fatalf("expected sourcesStatus inside processingTime, got %+v", resp.ProcessingTime.SourcesStatus)
	}
	if resp.ProcessingTime.SelectedSongMetadata == nil || resp.ProcessingTime.SelectedSongMetadata.Source != "Spotify" {
		t.Fatalf("expected selectedSongMetadata in processingTime, got %+v", resp.ProcessingTime.SelectedSongMetadata)
	}
}

func TestLyricsGetMissingParams(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v2/lyrics/get")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeJSON(t, rec, &body)
	if !strings.Contains(body["error"], "Missing required parameters") {
		t.Fatalf("unexpected error message: %q", body["error"])
	}
}

func TestLyricsGetNotFound(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{})

	rec := doGet(t, h, "/v2/lyrics/get?title=Nope&artist=Nobody&source=spotify")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("expected no-store on 404, got %q", cc)
	}
}

func TestLyricsGetV1(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v1/lyrics/get?title=Hello&artist=Adele&source=spotify")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp domain.V1Response
	decodeJSON(t, rec, &resp)
	if len(resp.Lyrics) == 0 {
		t.Fatalf("expected flat V1 segments")
	}
	if resp.Lyrics[0].Text == "" {
		t.Fatalf("expected non-empty segment text")
	}
}

func TestLyricsGetTTML(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v1/ttml/get?title=Hello&artist=Adele&source=spotify")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	decodeJSON(t, rec, &body)
	ttml, ok := body["ttml"].(string)
	if !ok || !strings.Contains(ttml, "<tt") {
		t.Fatalf("expected TTML serialization, got %q", body["ttml"])
	}
}

func TestLyricsGetRaw(t *testing.T) {
	raw := `<xml version="1.0">lyrics</xml>`
	resp := sampleWordLyrics()
	resp.RawData = raw
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: resp})

	rec := doGet(t, h, "/v1/raw/get?title=Hello&artist=Adele&source=spotify")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json for spotify raw, got %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("expected Content-Disposition=inline, got %q", cd)
	}
	if sz := rec.Header().Get("X-Lyrics-Source"); sz != "spotify" {
		t.Fatalf("expected X-Lyrics-Source=spotify, got %q", sz)
	}
	if strings.TrimSpace(rec.Body.String()) != raw {
		t.Fatalf("raw body mismatch: %q", rec.Body.String())
	}

	// Apple Music returns application/xml and Content-Disposition: inline
	respApple := sampleWordLyrics()
	respApple.Metadata.Source = "Apple"
	respApple.RawData = raw
	hApple := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: respApple, name: "apple"})
	recApple := doGet(t, hApple, "/v1/raw/get?title=Hello&artist=Adele&source=apple")
	if recApple.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recApple.Code, recApple.Body.String())
	}
	if ct := recApple.Header().Get("Content-Type"); ct != "application/xml" {
		t.Fatalf("expected application/xml for apple raw, got %q", ct)
	}
	if cd := recApple.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("expected Content-Disposition=inline, got %q", cd)
	}

	// Deezer returns text/plain and Content-Disposition: inline
	respDeezer := sampleWordLyrics()
	respDeezer.Metadata.Source = "Deezer"
	respDeezer.RawData = "[00:01.00] Hello"
	hDeezer := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: respDeezer, name: "deezer"})
	recDeezer := doGet(t, hDeezer, "/v1/raw/get?title=Hello&artist=Adele&source=deezer")
	if recDeezer.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recDeezer.Code, recDeezer.Body.String())
	}
	if ct := recDeezer.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("expected text/plain for deezer raw, got %q", ct)
	}
	if cd := recDeezer.Header().Get("Content-Disposition"); cd != "inline" {
		t.Fatalf("expected Content-Disposition=inline, got %q", cd)
	}
}

func TestRawNotFound(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	// RawData empty on otherwise valid lyrics => raw unavailable.
	rec := doGet(t, h, "/v1/raw/get?title=Hello&artist=Adele&source=spotify")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing raw, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHealthAndReady(t *testing.T) {
	cfg := baseTestConfig()
	dbPath := filepath.Join(t.TempDir(), "health_test.db")
	cfg.Storage.DBPath = dbPath
	store, err := storage.NewStore(cfg.Storage)
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	lyricsH := &handlers.Lyrics{}
	catalogH := &handlers.Catalog{}
	powH := &handlers.Pow{}
	healthH := &handlers.Health{Store: store, Started: time.Now()}
	router := newRouter(cfg, nil, lyricsH, catalogH, powH, healthH)

	rec := doGet(t, router, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" || body["database"] != "ok" {
		t.Fatalf("unexpected health body: %v", body)
	}

	rec = doGet(t, router, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 readyz, got %d: %s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected readyz ok, got %v", body)
	}

	// Closing the database makes readiness fail (503) while liveness stays 200.
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	rec = doGet(t, router, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 degraded readyz, got %d", rec.Code)
	}
	rec = doGet(t, router, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected liveness 200 after db close, got %d", rec.Code)
	}
}

func TestRateLimiting(t *testing.T) {
	cfg := baseTestConfig()
	cfg.RateLimit.Requests = 2
	cfg.RateLimit.Window = time.Hour
	h := buildTestRouter(t, cfg, &fakeSource{resp: sampleWordLyrics()})

	target := "/v2/lyrics/get?title=Hello&artist=Adele&source=spotify"
	if rec := doGet(t, h, target); rec.Code != http.StatusOK {
		t.Fatalf("request 1: expected 200, got %d", rec.Code)
	}
	if rec := doGet(t, h, target); rec.Code != http.StatusOK {
		t.Fatalf("request 2: expected 200, got %d", rec.Code)
	}
	rec := doGet(t, h, target)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3: expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 429")
	}

	// Health probes must never be throttled.
	if rec := doGet(t, h, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("health should bypass rate limit, got %d", rec.Code)
	}
	if rec := doGet(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("readyz should bypass rate limit, got %d", rec.Code)
	}
}

func TestQueryLimitMiddlewares(t *testing.T) {
	cfg := baseTestConfig()
	cfg.Server.MaxURLBytes = 32
	h := buildTestRouter(t, cfg, &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v2/lyrics/get?title=Hello&artist=Adele")
	if rec.Code != http.StatusRequestURITooLong {
		t.Fatalf("expected 414 for oversized URL, got %d", rec.Code)
	}

	cfg2 := baseTestConfig()
	cfg2.Server.MaxQueryParams = 2
	h2 := buildTestRouter(t, cfg2, &fakeSource{resp: sampleWordLyrics()})

	rec = doGet(t, h2, "/v2/lyrics/get?title=Hello&artist=Adele&album=A")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for too many params, got %d", rec.Code)
	}
}

func TestCORSAndCompression(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	req := httptest.NewRequest(http.MethodOptions, "/v2/lyrics/get", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for OPTIONS, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected permissive CORS origin")
	}

	req = httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 gzip, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip content-encoding")
	}
}

func TestDocsAndRoot(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/openapi.yaml")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "openapi:") {
		t.Fatalf("expected OpenAPI spec, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doGet(t, h, "/docs")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "swagger-ui") {
		t.Fatalf("expected /docs viewer, got %d", rec.Code)
	}

	rec = doGet(t, h, "/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Seems, you") {
		t.Fatalf("expected root banner, got %d", rec.Code)
	}
}

func solvePow(challenge string, difficulty int) string {
	prefix := strings.Repeat("0", difficulty)
	for i := 0; ; i++ {
		nonce := strconv.Itoa(i)
		sum := sha256.Sum256([]byte(challenge + nonce))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), prefix) {
			return nonce
		}
	}
}

func TestPowChallenge(t *testing.T) {
	cfg := baseTestConfig()
	h := buildTestRouter(t, cfg, &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v1/lyricsplus/challenge")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 challenge, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	decodeJSON(t, rec, &body)
	if body["token"] == "" {
		t.Fatalf("expected challenge token, got %v", body)
	}
	if body["difficulty"] != float64(cfg.LyricsPlus.PoWDifficulty) {
		t.Fatalf("expected difficulty %d, got %v", cfg.LyricsPlus.PoWDifficulty, body["difficulty"])
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("expected no-store on challenge, got %q", cc)
	}
}

// TestPowSubmit exercises the full challenge -> solve -> submit flow over HTTP.
func TestPowSubmit(t *testing.T) {
	cfg := baseTestConfig()
	h := buildTestRouter(t, cfg, &fakeSource{resp: sampleWordLyrics()})

	issuer := lyricsplus.NewIssuer(cfg.LyricsPlus.JWTSecret, cfg.LyricsPlus.ChallengeTTL)
	token, challenge, err := issuer.Issue(time.Now())
	if err != nil {
		t.Fatalf("issue challenge: %v", err)
	}
	nonce := solvePow(challenge, cfg.LyricsPlus.PoWDifficulty)

	payload := map[string]interface{}{
		"proofOfWorkToken": token,
		"nonce":            nonce,
		"songTitle":        "Hello",
		"songArtist":       "Adele",
		"songAlbum":        "25",
		"songDuration":     "232.123",
		"songISRC":         "GBUM71705975",
		"lyricsData":       sampleWordLyrics(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/lyricsplus/submit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 submit, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	decodeJSON(t, rec, &resp)
	if resp["success"] != true {
		t.Fatalf("expected success true, got %v", resp)
	}
	if resp["filename"] == nil {
		t.Fatalf("expected filename in response")
	}
}

func TestPowSubmitInvalidProof(t *testing.T) {
	cfg := baseTestConfig()
	h := buildTestRouter(t, cfg, &fakeSource{resp: sampleWordLyrics()})

	payload := map[string]interface{}{
		"proofOfWorkToken": "not-a-token",
		"nonce":            "1",
		"songTitle":        "Hello",
		"songArtist":       "Adele",
		"songDuration":     "232",
		"lyricsData":       sampleWordLyrics(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/lyricsplus/submit", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad proof of work, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPowSubmitDisabled(t *testing.T) {
	cfg := baseTestConfig()
	cfg.LyricsPlus.AllowSubmissions = false
	h := buildTestRouter(t, cfg, &fakeSource{resp: sampleWordLyrics()})

	req := httptest.NewRequest(http.MethodPost, "/v1/lyricsplus/submit", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when submissions disabled, got %d", rec.Code)
	}
}

func TestCatalogsearchMissingQuery(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v1/songlist/search")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMetadataMissingParams(t *testing.T) {
	h := buildTestRouter(t, baseTestConfig(), &fakeSource{resp: sampleWordLyrics()})

	rec := doGet(t, h, "/v1/metadata/get?title=OnlyTitle")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
