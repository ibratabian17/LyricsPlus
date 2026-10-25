package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAdminAuthSecurity(t *testing.T) {
	secret := "secure-custom-admin-key-987"
	middleware := AdminAuth(secret)

	nextCalled := false
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	// Security Test 1: Insecure default secret must be rejected
	{
		defaultHandler := AdminAuth("lyricsplus-submit-opensource-yes-yes-yes")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		req.Header.Set("Authorization", "Bearer lyricsplus-submit-opensource-yes-yes-yes")
		rr := httptest.NewRecorder()
		defaultHandler.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable on default secret, got code=%d", rr.Code)
		}
	}

	// Security Test 2: Missing token -> 401
	{
		nextCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || nextCalled {
			t.Fatalf("expected 401 on missing token, got code=%d", rr.Code)
		}
	}

	// Security Test 3: Query parameter token is prohibited -> 400
	{
		nextCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats?token="+secret, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest || nextCalled {
			t.Fatalf("expected 400 Bad Request on query param token, got code=%d", rr.Code)
		}
	}

	// Security Test 4: Public PoW challenge token MUST be rejected (privilege escalation check)
	{
		challengeClaims := jwt.MapClaims{
			"challenge": "random-uuid-challenge-from-public-api",
			"iat":       time.Now().Unix(),
			"exp":       time.Now().Add(1 * time.Hour).Unix(),
		}
		tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, challengeClaims).SignedString([]byte(secret))
		if err != nil {
			t.Fatalf("failed to sign challenge token: %v", err)
		}

		nextCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || nextCalled {
			t.Fatalf("CRITICAL SECURITY FAILURE: Public challenge token was accepted as admin! got code=%d", rr.Code)
		}
	}

	// Security Test 5: JWT without admin role MUST be rejected
	{
		normalUserClaims := jwt.MapClaims{
			"sub": "regular_user",
			"exp": time.Now().Add(1 * time.Hour).Unix(),
		}
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, normalUserClaims).SignedString([]byte(secret))

		nextCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized || nextCalled {
			t.Fatalf("expected 401 on non-admin JWT, got code=%d", rr.Code)
		}
	}

	// Security Test 6: Valid raw secret via Authorization header -> 200
	{
		nextCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !nextCalled {
			t.Fatalf("expected 200 on valid bearer secret, got code=%d", rr.Code)
		}
	}

	// Security Test 7: Valid raw secret via X-Admin-Token header -> 200
	{
		nextCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		req.Header.Set("X-Admin-Token", secret)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !nextCalled {
			t.Fatalf("expected 200 on valid X-Admin-Token, got code=%d", rr.Code)
		}
	}

	// Security Test 8: Valid JWT with role: "admin" -> 200
	{
		adminClaims := jwt.MapClaims{
			"sub":  "operator",
			"role": "admin",
			"exp":  time.Now().Add(1 * time.Hour).Unix(),
		}
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, adminClaims).SignedString([]byte(secret))

		nextCalled = false
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !nextCalled {
			t.Fatalf("expected 200 on admin JWT, got code=%d", rr.Code)
		}
	}

	// Security Test 9: Brute-force lockout after 5 failed attempts from same IP
	{
		for i := 0; i < 5; i++ {
			req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
			req.RemoteAddr = "198.51.100.42:12345"
			req.Header.Set("Authorization", "Bearer bad-guess")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
		}

		// 6th attempt must be locked out with 429
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		req.RemoteAddr = "198.51.100.42:12345"
		req.Header.Set("Authorization", "Bearer "+secret) // Even with correct password, blocked during lockout
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 Too Many Requests during lockout, got code=%d", rr.Code)
		}
	}
}
