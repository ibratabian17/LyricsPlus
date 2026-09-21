// Package middleware provides request processing middleware for the API server.
package middleware

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lyricsplus/backend/internal/logger"
)

type ctxKey int

const (
	ctxReqID ctxKey = iota
	ctxStartTime
)

// IsHealthPath reports whether the request targets a health probe.
// Health paths are exempt from request throttling so deployment probes are
// never starved by client traffic.
func IsHealthPath(r *http.Request) bool {
	switch r.URL.Path {
	case "/health", "/readyz":
		return true
	}
	return false
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// RequestID reads the 8-character request id from the context.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxReqID).(string); ok {
		return v
	}
	return ""
}

// Tracing generates an 8-character hex request ID, attaches it to the context,
// and logs a structured completion line with correlation ID.
func Tracing(l *logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := shortHexID()
			ctx := context.WithValue(r.Context(), ctxReqID, id)
			ctx = context.WithValue(ctx, ctxStartTime, time.Now())
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r.WithContext(ctx))
			l.Info("request",
				slog.String("id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Duration("took", time.Since(start).Round(time.Millisecond)),
			)
		})
	}
}

func shortHexID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// CORS allows any origin with default methods and headers.
func CORS() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, x-proxy-token")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Compression serves gzip when the client accepts it.
func Compression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		next.ServeHTTP(gzipResponseWriter{ResponseWriter: w, gw: gz}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gw *gzip.Writer
}

func (g gzipResponseWriter) Write(b []byte) (int, error) {
	return g.gw.Write(b)
}

// QueryLimits rejects oversized URLs and too many/long query parameters.
func QueryLimits(maxURLBytes, maxParams, maxValueLen int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.RequestURI) > maxURLBytes {
				writeJSON(w, http.StatusRequestURITooLong, map[string]string{"error": "Request URL is too long"})
				return
			}
			values := r.URL.Query()
			if len(values) > maxParams {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Query parameters exceed allowed limits"})
				return
			}
			for k, vv := range values {
				if len(k) > maxValueLen {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Query parameters exceed allowed limits"})
					return
				}
				for _, v := range vv {
					if len(v) > maxValueLen {
						writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Query parameters exceed allowed limits"})
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimit enforces a maximum request body size (413 on exceed).
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if maxBytes > 0 {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ConcurrencyLimiter limits maximum inflight requests.
func ConcurrencyLimiter(max int64) func(http.Handler) http.Handler {
	var inflight atomic.Int64
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			if max > 0 && cur > max {
				w.Header().Set("Retry-After", "1")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":   "Server Busy",
					"message": "Too many concurrent requests. Please retry shortly. Currently we have " + itoaMax(cur) + " concurrent requests",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func itoaMax(v int64) string {
	if v <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// ClientIP resolves the client address via the standard reverse-proxy cascade.
func ClientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Vercel-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type slidingWindow struct {
	mu        sync.Mutex
	window    time.Duration
	max       int
	perIP     map[string][]time.Time
	lastPrune time.Time
}

// RateLimiter returns a sliding-window limiter keyed by client IP.
func RateLimiter(max int, window time.Duration) func(http.Handler) http.Handler {
	l := &slidingWindow{
		window:    window,
		max:       max,
		perIP:     map[string][]time.Time{},
		lastPrune: time.Now(),
	}
	return l.middleware
}

func (l *slidingWindow) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsHealthPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		ip := ClientIP(r)
		remaining, ok := l.allow(ip)
		if !ok {
			w.Header().Set("Retry-After", remaining)
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error":   "Too Many Requests",
				"message": "Rate limit exceeded. Retry in " + remaining + " seconds.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *slidingWindow) allow(key string) (string, bool) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastPrune) > l.window*2 {
		for k, stamps := range l.perIP {
			if len(stamps) == 0 || now.Sub(stamps[len(stamps)-1]) > l.window*2 {
				delete(l.perIP, k)
			}
		}
		l.lastPrune = now
	}
	stamps := l.perIP[key]
	cutoff := now.Add(-l.window)
	keep := 0
	for i, t := range stamps {
		if t.After(cutoff) {
			keep = i
			break
		}
		keep = i + 1
	}
	stamps = stamps[keep:]
	if len(stamps) >= l.max {
		oldest := stamps[0]
		remaining := l.window - now.Sub(oldest)
		secs := int(remaining.Seconds())
		if secs < 1 {
			secs = 1
		}
		l.perIP[key] = stamps
		return itoaSec(secs), false
	}
	stamps = append(stamps, now)
	l.perIP[key] = stamps
	return "0", true
}

func itoaSec(v int) string {
	if v <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
