package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
)

func TestExternalScraping_BlockedIP(t *testing.T) {
	cfg := config.ExternalScraping{
		Enabled:    true,
		BlockedIPs: []string{"136.54.146.44", "10.0.0.0/8"},
	}
	handler := ExternalScraping(cfg, logger.Nop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test blocked exact IP
	req := httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("CF-Connecting-IP", "136.54.146.44")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for blocked IP, got %d", rec.Code)
	}

	// Test blocked CIDR
	req = httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("CF-Connecting-IP", "10.5.20.1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for CIDR blocked IP, got %d", rec.Code)
	}

	// Test allowed IP
	req = httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("CF-Connecting-IP", "1.1.1.1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for allowed IP, got %d", rec.Code)
	}
}

func TestExternalScraping_BlockedUserAgent(t *testing.T) {
	cfg := config.ExternalScraping{
		Enabled:           true,
		BlockedUserAgents: []string{"blocked-bot", "re:^python-requests/.*"},
	}
	handler := ExternalScraping(cfg, logger.Nop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Test substring match
	req := httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("User-Agent", "blocked-bot/2.3.0")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for blocked UA, got %d", rec.Code)
	}

	// Test regex match
	req = httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("User-Agent", "python-requests/2.28.1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for regex UA, got %d", rec.Code)
	}

	// Test allowed UA
	req = httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for allowed UA, got %d", rec.Code)
	}
}

func TestExternalScraping_CachedOnly(t *testing.T) {
	cfg := config.ExternalScraping{
		Enabled:              true,
		CachedOnlyUserAgents: []string{"CustomCacheClient"},
	}

	var capturedCacheOnly bool
	handler := ExternalScraping(cfg, logger.Nop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCacheOnly = domain.IsCacheOnly(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("User-Agent", "CustomCacheClient/1.0")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}
	if !capturedCacheOnly {
		t.Errorf("expected domain.IsCacheOnly(ctx) to be true")
	}

	// Normal user agent should NOT be cache-only
	capturedCacheOnly = false
	req = httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if capturedCacheOnly {
		t.Errorf("expected domain.IsCacheOnly(ctx) to be false for normal user agent")
	}
}

func TestExternalScraping_ThrottleRule(t *testing.T) {
	cfg := config.ExternalScraping{
		Enabled: true,
		Rules: []config.ExternalScrapingRule{
			{
				Name:       "Throttle test",
				Action:     "throttle",
				UserAgents: []string{"SpamBot"},
				Throttle: &config.ExternalScrapingThrottle{
					Requests: 2,
					Window:   time.Minute,
				},
			},
		},
	}

	handler := ExternalScraping(cfg, logger.Nop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Request 1: allowed
	req := httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("User-Agent", "SpamBot/1.0")
	req.Header.Set("CF-Connecting-IP", "203.0.113.195")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request 1 expected 200, got %d", rec.Code)
	}

	// Request 2: allowed
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request 2 expected 200, got %d", rec.Code)
	}

	// Request 3: throttled (429)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3 expected 429 Too Many Requests, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header to be set")
	}
}

func TestExternalScraping_CountryMatching(t *testing.T) {
	cfg := config.ExternalScraping{
		Enabled: true,
		Rules: []config.ExternalScrapingRule{
			{
				Name:      "India Cache Only",
				Action:    "cached_only",
				Countries: []string{"India"}, // Name lookup test
			},
			{
				Name:      "Block Test Country",
				Action:    "block",
				Countries: []string{"KP"}, // 2-letter ISO test
			},
		},
	}

	var capturedCacheOnly bool
	handler := ExternalScraping(cfg, logger.Nop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCacheOnly = domain.IsCacheOnly(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// Test Country India matching "IN" header
	req := httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("CF-IPCountry", "IN")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}
	if !capturedCacheOnly {
		t.Errorf("expected India request to be cached_only")
	}

	// Test Blocked Country
	req = httptest.NewRequest("GET", "/v2/lyrics/get", nil)
	req.Header.Set("CF-IPCountry", "KP")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for country KP, got %d", rec.Code)
	}
}

func TestExternalScraping_BypassHealth(t *testing.T) {
	cfg := config.ExternalScraping{
		Enabled:             true,
		BlockEmptyUserAgent: true,
		BlockedIPs:          []string{"127.0.0.1"},
	}
	handler := ExternalScraping(cfg, logger.Nop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("CF-Connecting-IP", "127.0.0.1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("health endpoint should bypass external scraping middleware, got %d", rec.Code)
	}
}
