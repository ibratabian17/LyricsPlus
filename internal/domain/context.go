package domain

import "context"

type contextKey string

const ctxCacheOnlyKey contextKey = "lyricsplus:cache_only"

// WithCacheOnly sets the cache-only flag in the context.
func WithCacheOnly(ctx context.Context, cacheOnly bool) context.Context {
	return context.WithValue(ctx, ctxCacheOnlyKey, cacheOnly)
}

// IsCacheOnly checks whether the context requires serving only cached data.
func IsCacheOnly(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(ctxCacheOnlyKey).(bool)
	return ok && v
}
