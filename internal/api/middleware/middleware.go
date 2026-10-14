package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/metrics"
)

type ctxKey int

const (
	ctxReqID ctxKey = iota
	ctxStartTime
)

var reqIDSeq atomic.Uint64

func IsHealthPath(r *http.Request) bool {
	switch r.URL.Path {
	case "/health", "/readyz":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxReqID).(string); ok {
		return v
	}
	return ""
}

func Tracing(l *logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := shortHexID()
			start := time.Now()
			ctx := context.WithValue(r.Context(), ctxReqID, id)
			ctx = context.WithValue(ctx, ctxStartTime, start)
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r.WithContext(ctx))
			metrics.Default.RecordHTTPRequest(sw.status)
			if l != nil && l.Enabled(slog.LevelDebug) {
				l.Debug("request",
					slog.String("id", id),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", sw.status),
					slog.Duration("took", time.Since(start).Round(time.Millisecond)),
				)
			}
		})
	}
}

func shortHexID() string {
	const hexChars = "0123456789abcdef"
	n := uint32(reqIDSeq.Add(1))
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = hexChars[n&0xf]
		n >>= 4
	}
	return string(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

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

func Compression(next http.Handler) http.Handler {
	return chimiddleware.Compress(5, "text/*", "application/*")(next)
}

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

func firstCommaValue(v string) string {
	if idx := strings.IndexByte(v, ','); idx >= 0 {
		v = v[:idx]
	}
	return strings.TrimSpace(v)
}

func ClientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return firstCommaValue(v)
	}
	if v := r.Header.Get("X-Vercel-Forwarded-For"); v != "" {
		return firstCommaValue(v)
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return firstCommaValue(v)
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

type ipEntry struct {
	windowStart int64
	count       int
	last        int64
}

type rateLimiterShard struct {
	mu        sync.Mutex
	perIP     map[string]*ipEntry
	lastPrune time.Time
}

type shardedRateLimiter struct {
	window   time.Duration
	windowNs int64
	max      int
	shards   [numShards]rateLimiterShard
}

func fnv32(key string) uint32 {
	var hash uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= 16777619
	}
	return hash
}

func RateLimiter(max int, window time.Duration) func(http.Handler) http.Handler {
	if max <= 0 {
		return func(next http.Handler) http.Handler {
			return next
		}
	}
	if window <= 0 {
		window = 10 * time.Second
	}
	l := &shardedRateLimiter{
		window:   window,
		windowNs: window.Nanoseconds(),
		max:      max,
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

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if now.Sub(shard.lastPrune) > l.window*2 {
		pruneAfter := nowNs - l.windowNs*2
		beforeLen := len(shard.perIP)
		for k, e := range shard.perIP {
			if e.last < pruneAfter {
				delete(shard.perIP, k)
			}
		}
		if beforeLen > 256 && len(shard.perIP) < beforeLen/4 {
			rebuilt := make(map[string]*ipEntry, len(shard.perIP)+16)
			for k, v := range shard.perIP {
				rebuilt[k] = v
			}
			shard.perIP = rebuilt
		}
		shard.lastPrune = now
	}

	e := shard.perIP[key]
	if e == nil {
		e = &ipEntry{
			windowStart: nowNs,
			count:       1,
			last:        nowNs,
		}
		shard.perIP[key] = e
		return "0", true
	}
	e.last = nowNs

	elapsed := nowNs - e.windowStart
	if elapsed >= l.windowNs {
		e.windowStart = nowNs
		e.count = 1
		return "0", true
	}

	if e.count >= l.max {
		remNs := l.windowNs - elapsed
		secs := int(time.Duration(remNs).Seconds())
		if secs < 1 {
			secs = 1
		}
		return itoaSec(secs), false
	}

	e.count++
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
