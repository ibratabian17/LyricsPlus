package handlers

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/providers"
	"lyricsplus/backend/internal/similarity"
	"lyricsplus/backend/internal/storage"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

type Catalog struct {
	Store      *storage.Store
	AppleMusic *providers.AppleMusicProvider
	Spotify    *providers.SpotifyProvider
	Musixmatch *providers.MusixmatchProvider
	Deezer     *providers.DeezerProvider
	QQMusic    *providers.QQMusicProvider
	Logger     *logger.Logger
}

func (h *Catalog) Search(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing required parameter: q (query)"})
		return
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	var results []domain.SongCatalogItem
	var resMu sync.Mutex
	agg := func(items []domain.SongCatalogItem, err error) {
		if err != nil {
			h.logf("catalog search %q failed: %v", q, err)
			return
		}
		resMu.Lock()
		results = append(results, items...)
		resMu.Unlock()
	}

	type searchFn struct {
		name string
		call func() ([]domain.SongCatalogItem, error)
	}
	var fns []searchFn
	if h.Store != nil {
		fns = append(fns, searchFn{"database", func() ([]domain.SongCatalogItem, error) { return h.Store.SearchCatalog(ctx, q) }})
	}
	if h.AppleMusic != nil {
		fns = append(fns, searchFn{"apple", func() ([]domain.SongCatalogItem, error) { return h.AppleMusic.SearchCatalog(ctx, q) }})
	}
	if h.Spotify != nil {
		fns = append(fns, searchFn{"spotify", func() ([]domain.SongCatalogItem, error) { return h.Spotify.SearchCatalog(ctx, q) }})
	}
	if h.Deezer != nil {
		fns = append(fns, searchFn{"deezer", func() ([]domain.SongCatalogItem, error) { return h.Deezer.SearchCatalog(ctx, q) }})
	}
	if h.QQMusic != nil {
		fns = append(fns, searchFn{"qq", func() ([]domain.SongCatalogItem, error) { return h.QQMusic.SearchCatalog(ctx, q) }})
	}
	if h.Musixmatch != nil {
		fns = append(fns, searchFn{"musixmatch", func() ([]domain.SongCatalogItem, error) { return h.Musixmatch.SearchCatalog(ctx, q) }})
	}

	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func(name string, call func() ([]domain.SongCatalogItem, error)) {
			defer wg.Done()
			agg(call())
		}(fn.name, fn.call)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": mergeCatalogResults(results, q),
		"processingTime": map[string]int64{
			"timeElapsed":   time.Since(start).Milliseconds(),
			"lastProcessed": time.Now().UnixMilli(),
		},
	})
}

func mergeCatalogResults(results []domain.SongCatalogItem, query string) []domain.SongCatalogItem {
	sourceOrder := map[string]int{
		"Database":    1,
		"Apple Music": 2,
		"Spotify":     3,
		"Deezer":      4,
		"QQ Music":    5,
		"Musixmatch":  6,
	}
	rank := func(item domain.SongCatalogItem) int {
		if len(item.Availability) == 0 {
			return 99
		}
		if r, ok := sourceOrder[item.Availability[0]]; ok {
			return r
		}
		return 99
	}

	seen := make(map[string]int)
	merged := make([]domain.SongCatalogItem, 0, len(results))
	for _, song := range results {
		key := ""
		if song.ISRC != nil && *song.ISRC != "" {
			key = *song.ISRC
		} else {
			key = strings.ToLower(song.Title) + "-" + strings.ToLower(song.Artist) + "-" + strings.ToLower(song.Album)
		}

		if idx, ok := seen[key]; ok {
			existing := &merged[idx]
			if existing.ID == nil {
				existing.ID = map[string]string{}
			}
			for k, v := range song.ID {
				existing.ID[k] = v
			}
			if existing.ExternalURLs == nil {
				existing.ExternalURLs = map[string]string{}
			}
			for k, v := range song.ExternalURLs {
				existing.ExternalURLs[k] = v
			}
			existing.Songwriters = unionStrings(existing.Songwriters, song.Songwriters)
			existing.Availability = unionStrings(existing.Availability, song.Availability)
			if existing.AlbumArtURL == nil && song.AlbumArtURL != nil {
				existing.AlbumArtURL = song.AlbumArtURL
			}
			if existing.DurationMs == 0 && song.DurationMs != 0 {
				existing.DurationMs = song.DurationMs
			}
		} else {
			seen[key] = len(merged)
			merged = append(merged, song)
		}
	}

	if query != "" {
		scores := make([]float64, len(merged))
		for i, item := range merged {
			scores[i] = similarity.CatalogQuerySimilarity(item.Title, item.Artist, item.Album, query)
		}
		sort.SliceStable(merged, func(i, j int) bool {
			diff := scores[i] - scores[j]
			if math.Abs(diff) > 0.001 {
				return scores[i] > scores[j]
			}
			return rank(merged[i]) < rank(merged[j])
		})
	} else {
		sort.SliceStable(merged, func(i, j int) bool {
			return rank(merged[i]) < rank(merged[j])
		})
	}

	return merged
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	add := func(items []string) {
		for _, s := range items {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	add(a)
	add(b)
	return out
}

func (h *Catalog) Metadata(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	artist := strings.TrimSpace(r.URL.Query().Get("artist"))
	if title == "" || artist == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing required parameters: title and artist"})
		return
	}
	album := strings.TrimSpace(r.URL.Query().Get("album"))
	var durationSec float64
	if ds := strings.TrimSpace(r.URL.Query().Get("duration")); ds != "" {
		durationSec, _ = strconv.ParseFloat(ds, 64)
	}

	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	type metaGetter struct {
		name string
		call func() (map[string]interface{}, error)
	}
	var getters []metaGetter
	if h.AppleMusic != nil {
		getters = append(getters, metaGetter{"apple", func() (map[string]interface{}, error) {
			return h.AppleMusic.GetMetadata(ctx, title, artist, album, durationSec)
		}})
	}
	if h.Spotify != nil {
		getters = append(getters, metaGetter{"spotify", func() (map[string]interface{}, error) {
			return h.Spotify.GetMetadata(ctx, title, artist, album, durationSec)
		}})
	}
	if h.Deezer != nil {
		getters = append(getters, metaGetter{"deezer", func() (map[string]interface{}, error) {
			return h.Deezer.GetMetadata(ctx, title, artist, album, durationSec)
		}})
	}
	if h.QQMusic != nil {
		getters = append(getters, metaGetter{"qq", func() (map[string]interface{}, error) {
			return h.QQMusic.GetMetadata(ctx, title, artist, album, durationSec)
		}})
	}
	if h.Musixmatch != nil {
		getters = append(getters, metaGetter{"musixmatch", func() (map[string]interface{}, error) {
			return h.Musixmatch.GetMetadata(ctx, title, artist, album, durationSec)
		}})
	}
	if h.Store != nil {
		getters = append(getters, metaGetter{"database", func() (map[string]interface{}, error) {
			return h.Store.GetMetadata(ctx, title, artist, album, durationSec)
		}})
	}

	for _, g := range getters {
		meta, err := g.call()
		if err == nil && len(meta) > 0 {
			writeJSON(w, http.StatusOK, map[string]interface{}{"metadata": meta})
			return
		}
	}

	writeJSON(w, http.StatusNotFound, map[string]string{"error": "Could not find metadata"})
}

func (h *Catalog) logf(format string, args ...interface{}) {
	if h.Logger != nil {
		h.Logger.Errorf(format, args...)
	}
}
