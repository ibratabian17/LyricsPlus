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
	"lyricsplus/backend/internal/metrics"
)

// gzipPool reuses gzip.Writers to avoid per-request heap allocations under load.
var gzipPool = sync.Pool{
	New: func() interface{} {
		gw, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		return gw
	},
}

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
			metrics.Default.RecordHTTPRequest(sw.status)
			l.Debug("request",
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

// PanicRecovery catches unhandled panics, logs the stack, and returns a 500 JSON error.
func PanicRecovery(l *logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if l != nil {
						l.Errorf("unhandled panic recovered on %s %s: %v", r.Method, r.URL.Path, rec)
					}
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.Header().Set("Cache-Control", "no-store")
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"error":   "Internal Server Error",
						"message": "An unexpected error occurred",
					})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
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
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			_ = gz.Close()
			gzipPool.Put(gz)
		}()
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
			if IsHealthPath(r) {
				next.ServeHTTP(w, r)
				return
			}
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			if max > 0 && cur > max {
				w.Header().Set("Content-Type", "application/json")
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

const numShards = 64

// ipEntry holds a fixed-capacity ring buffer of request timestamps (unix nano)
// to avoid heap allocations on the hot path. maxStamps must be >= max config value.
const maxStamps = 128

type ipEntry struct {
	ts   [maxStamps]int64 // ring buffer of arrival times (unix ns)
	head int              // write head
	n    int              // count of entries currently valid
	last int64            // last-seen timestamp (ns) for pruning
}

type rateLimiterShard struct {
	mu        sync.Mutex
	perIP     map[string]*ipEntry
	lastPrune time.Time
}

type shardedRateLimiter struct {
	window time.Duration
	max    int
	shards [numShards]rateLimiterShard
}

func fnv32(key string) uint32 {
	var hash uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= 16777619
	}
	return hash
}

// RateLimiter returns a sharded sliding-window limiter keyed by client IP.
func RateLimiter(max int, window time.Duration) func(http.Handler) http.Handler {
	if max <= 0 {
		return func(next http.Handler) http.Handler {
			return next
		}
	}
	if max > maxStamps {
		max = maxStamps // safety clamp
	}
	l := &shardedRateLimiter{
		window: window,
		max:    max,
	}
	now := time.Now()
	for i := 0; i < numShards; i++ {
		l.shards[i].perIP = make(map[string]*ipEntry, 32)
		l.shards[i].lastPrune = now
	}
	return l.middleware
}

func (l *shardedRateLimiter) middleware(next http.Handler) http.Handler {
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

func (l *shardedRateLimiter) allow(key string) (string, bool) {
	shardIdx := fnv32(key) % numShards
	shard := &l.shards[shardIdx]

	now := time.Now()
	nowNs := now.UnixNano()
	cutoffNs := now.Add(-l.window).UnixNano()

	shard.mu.Lock()
	defer shard.mu.Unlock()

	// Periodic prune: remove IPs that haven't been seen in 2 windows.
	if now.Sub(shard.lastPrune) > l.window*2 {
		pruneAfter := now.Add(-l.window * 2).UnixNano()
		for k, e := range shard.perIP {
			if e.last < pruneAfter {
				delete(shard.perIP, k)
			}
		}
		shard.lastPrune = now
	}

	e := shard.perIP[key]
	if e == nil {
		e = &ipEntry{}
		shard.perIP[key] = e
	}
	e.last = nowNs

	// Count how many entries fall within the current window.
	count := 0
	for i := 0; i < e.n; i++ {
		idx := (e.head - e.n + i + maxStamps) % maxStamps
		if e.ts[idx] >= cutoffNs {
			count++
		}
	}

	if count >= l.max {
		// Find the oldest timestamp in the window to compute retry-after.
		oldest := nowNs
		for i := 0; i < e.n; i++ {
			idx := (e.head - e.n + i + maxStamps) % maxStamps
			if e.ts[idx] >= cutoffNs && e.ts[idx] < oldest {
				oldest = e.ts[idx]
			}
		}
		remaining := l.window - time.Duration(nowNs-oldest)
		secs := int(remaining.Seconds())
		if secs < 1 {
			secs = 1
		}
		return itoaSec(secs), false
	}

	// Write new timestamp into the ring buffer.
	e.ts[e.head] = nowNs
	e.head = (e.head + 1) % maxStamps
	if e.n < maxStamps {
		e.n++
	}
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
