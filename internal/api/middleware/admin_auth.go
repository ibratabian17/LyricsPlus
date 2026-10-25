package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Brute-force tracking: max 5 failed attempts per IP within 1 minute window
type authFailureTracker struct {
	mu       sync.Mutex
	failures map[string]*ipFailEntry
}

type ipFailEntry struct {
	count       int
	firstFail   time.Time
	lockedUntil time.Time
}

var globalAuthTracker = &authFailureTracker{
	failures: make(map[string]*ipFailEntry),
}

func (t *authFailureTracker) isLocked(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.failures[ip]
	if !ok {
		return false
	}
	now := time.Now()
	if now.Before(entry.lockedUntil) {
		return true
	}
	// Expired lockout or window
	if now.Sub(entry.firstFail) > 1*time.Minute {
		delete(t.failures, ip)
		return false
	}
	return false
}

func (t *authFailureTracker) recordFailure(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	entry, ok := t.failures[ip]
	if !ok || now.Sub(entry.firstFail) > 1*time.Minute {
		t.failures[ip] = &ipFailEntry{
			count:     1,
			firstFail: now,
		}
		return
	}

	entry.count++
	if entry.count >= 5 {
		// Lock out for 1 minute
		entry.lockedUntil = now.Add(1 * time.Minute)
	}
}

func (t *authFailureTracker) recordSuccess(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failures, ip)
}

// AdminAuth validates that the incoming request is authorized for admin database operations.
// Security features:
// - Prevents timing attacks using subtle.ConstantTimeCompare
// - Rejects default/unconfigured secrets to prevent out-of-the-box exploitation
// - Disallows public challenge tokens from escalating privileges
// - Rejects tokens passed in query strings (?token=) to prevent credential logging
// - Protects against brute-force attacks via temporary IP lockouts
func AdminAuth(validTokens ...string) func(http.Handler) http.Handler {
	var validSecrets []string
	for _, t := range validTokens {
		t = strings.TrimSpace(t)
		if t != "" && t != "lyricsplus-submit-opensource-yes-yes-yes" {
			validSecrets = append(validSecrets, t)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientIP := ClientIP(r)

			// 1. Guard against unconfigured or insecure default secret
			if len(validSecrets) == 0 {
				writeAdminError(w, http.StatusServiceUnavailable, "Admin database access is disabled for security: Please configure a non-default admin_token or jwt_secret in config.json or environment")
				return
			}

			// 2. Guard against brute force attacks
			if globalAuthTracker.isLocked(clientIP) {
				writeAdminError(w, http.StatusTooManyRequests, "Too many failed admin authentication attempts. Please wait 60 seconds before retrying.")
				return
			}

			// 3. Extract token exclusively from HTTP headers (never URL query strings)
			token := extractAdminTokenHeader(r)
			if token == "" {
				// Check if user attempted to pass token in query string and warn them
				if r.URL.Query().Get("token") != "" {
					writeAdminError(w, http.StatusBadRequest, "Passing admin tokens in URL query parameters is prohibited for security. Please use Authorization: Bearer <token> or X-Admin-Token header.")
					return
				}
				writeAdminError(w, http.StatusUnauthorized, "Missing admin credentials. Please provide token via Authorization: Bearer <token> or X-Admin-Token header.")
				return
			}

			// 4. Validate token against any configured valid secret
			matched := false
			for _, secret := range validSecrets {
				if isValidAdminCredential(token, secret) {
					matched = true
					break
				}
			}

			if !matched {
				globalAuthTracker.recordFailure(clientIP)
				writeAdminError(w, http.StatusUnauthorized, "Invalid admin token")
				return
			}

			globalAuthTracker.recordSuccess(clientIP)
			next.ServeHTTP(w, r)
		})
	}
}

func extractAdminTokenHeader(r *http.Request) string {
	// 1. Authorization header: Bearer <token> or <token>
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); auth != "" {
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return strings.TrimSpace(auth[7:])
		}
		return auth
	}

	// 2. X-Admin-Token header
	if tok := strings.TrimSpace(r.Header.Get("X-Admin-Token")); tok != "" {
		return tok
	}

	return ""
}

func isValidAdminCredential(token, secret string) bool {
	token = strings.TrimSpace(token)
	secret = strings.TrimSpace(secret)
	if token == "" || secret == "" {
		return false
	}

	// 1. Constant-time raw token comparison (prevents timing attacks)
	if len(token) == len(secret) && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
		return true
	}

	// 2. Parse as JWT
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || parsed == nil || !parsed.Valid {
		return false
	}

	// SECURITY: Ensure this is NOT a public submission PoW challenge token!
	// Public PoW challenge tokens carry a "challenge" claim and are issued freely.
	if _, hasChallenge := claims["challenge"]; hasChallenge {
		return false
	}

	// Check token expiration
	if exp, ok := claims["exp"].(float64); ok && exp > 0 {
		if time.Now().Unix() > int64(exp) {
			return false
		}
	}

	// Require explicit admin authorization claim
	if role, ok := claims["role"].(string); ok && strings.EqualFold(role, "admin") {
		return true
	}
	if sub, ok := claims["sub"].(string); ok && strings.EqualFold(sub, "admin") {
		return true
	}
	if isAdmin, ok := claims["admin"].(bool); ok && isAdmin {
		return true
	}

	return false
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   http.StatusText(status),
		"message": message,
		"status":  status,
	})
}
