// Package api wires the HTTP router, middleware, and handlers for the LyricsPlus server.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"lyricsplus/backend/internal/api/handlers"
	"lyricsplus/backend/internal/api/middleware"
	"lyricsplus/backend/internal/api/openapi"
	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/metrics"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/providers"
	"lyricsplus/backend/internal/providers/lyricsplus"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/service"
	"lyricsplus/backend/internal/storage"
	"lyricsplus/backend/internal/version"
)

// Server wires the full HTTP API.
type Server struct {
	cfg      config.Config
	router   http.Handler
	logger   *logger.Logger
	store    *storage.Store
	watchdog *storage.Watchdog
	dumper   *storage.Dumper
	gdrive   *storage.GDriveClient
	httpSrv  *http.Server
}

// New builds and connects all layers.
func New(cfg config.Config, lg *logger.Logger) (*Server, error) {
	if lg == nil {
		lg = logger.Nop()
	}
	store, err := storage.NewStore(cfg.Storage)
	if err != nil {
		return nil, err
	}

	httpCache := storage.NewMemoryCache(cfg.Cache)
	watchdog := storage.NewWatchdog(cfg.Cache.MemoryWatchdogByte, 10*time.Second, httpCache)

	proxyClient := proxy.New(cfg.Server)
	gdrive := storage.NewGDriveClient(cfg.GDrive, nil)
	factory := &providers.Factory{Client: proxyClient, Store: store, GDrive: gdrive, Config: &cfg, Logger: lg}
	providerSet, err := factory.BuildSet()
	if err != nil {
		return nil, err
	}

	// Register platforms for live health and status monitoring
	if providerSet.AppleMusic != nil {
		metrics.Default.RegisterPlatform("apple", "Apple Music", providerSet.AppleMusic.Configured, "Apple Music API Availability")
	}
	if providerSet.Spotify != nil {
		metrics.Default.RegisterPlatform("spotify", "Spotify", providerSet.Spotify.Configured, "Spotify Web API Availability")
	}
	if providerSet.Musixmatch != nil {
		metrics.Default.RegisterPlatform("musixmatch", "Musixmatch", providerSet.Musixmatch.Configured, "Musixmatch API Availability")
	}
	if providerSet.Deezer != nil {
		metrics.Default.RegisterPlatform("deezer", "Deezer", providerSet.Deezer.Configured, "Deezer API Availability")
	}
	if providerSet.QQMusic != nil {
		metrics.Default.RegisterPlatform("qq", "QQ Music", providerSet.QQMusic.Configured, "QQ Music QRC API Availability")
	}
	if providerSet.LyricsPlus != nil {
		metrics.Default.RegisterPlatform("lyricsplus", "Lyrics+", providerSet.LyricsPlus.Configured, "LyricsPlus User Store Availability")
	}
	if gdrive != nil {
		metrics.Default.RegisterPlatform("gdrive", "Google Drive", gdrive.IsConfigured, "Google Drive Storage Availability")
	}
	if store != nil {
		metrics.Default.RegisterPlatform("database", "LyricsDB", func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			return store.Ping(ctx) == nil
		}, "Local LyricsPlus UGC + Cache Database Availability")
	}

	racer := orchestrator.NewRacer(toOrchestratorSources(providerSet.Sources), cfg.Provider.Timeout, orchestrator.WithLogger(lg))
	dedup := orchestrator.NewDedup(racer)

	svc := &service.Service{
		Dedup:    dedup,
		Store:    store,
		MemCache: httpCache,
		Logger:   lg,
	}

	lyricsH := &handlers.Lyrics{Service: svc, Logger: lg, KpoeInfo: "lyricsplus"}
	catalogH := &handlers.Catalog{
		AppleMusic: providerSet.AppleMusic,
		Spotify:    providerSet.Spotify,
		Musixmatch: providerSet.Musixmatch,
		Logger:     lg,
	}
	powH := &handlers.Pow{
		Issuer:           lyricsplus.NewIssuer(cfg.LyricsPlus.JWTSecret, cfg.LyricsPlus.ChallengeTTL),
		Verifier:         lyricsplus.NewVerifier(cfg.LyricsPlus.JWTSecret, cfg.LyricsPlus.PoWDifficulty),
		Store:            store,
		GDrive:           gdrive,
		MaxBodyBytes:     cfg.LyricsPlus.MaxBodyBytes,
		AcceptVandal:     cfg.LyricsPlus.AcceptVandalism,
		AllowSubmissions: cfg.LyricsPlus.AllowSubmissions,
		Logger:           lg,
	}
	healthH := &handlers.Health{Store: store, Started: time.Now(), Version: version.Version}

	var dumper *storage.Dumper
	if cfg.GDrive.DailyDumpEnabled {
		dumper = storage.NewDumper(cfg, store, gdrive, lg)
	}

	router := newRouter(cfg, lg, lyricsH, catalogH, powH, healthH)

	return &Server{
		cfg:      cfg,
		router:   router,
		logger:   lg,
		store:    store,
		watchdog: watchdog,
		dumper:   dumper,
		gdrive:   gdrive,
	}, nil
}

func newRouter(cfg config.Config, lg *logger.Logger, l *handlers.Lyrics, c *handlers.Catalog, p *handlers.Pow, h *handlers.Health) http.Handler {
	r := chi.NewRouter()
	r.Use(
		middleware.Tracing(lg),
		middleware.CORS(),
		middleware.QueryLimits(cfg.Server.MaxURLBytes, cfg.Server.MaxQueryParams, cfg.Server.MaxQueryValueLen),
		middleware.ConcurrencyLimiter(int64(cfg.Server.MaxConcurrency)),
		middleware.RateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window),
		middleware.Compression,
	)

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Seems, you trying to find out about our api huh?"))
	})
	r.Get("/health", h.Handle)
	r.Get("/readyz", h.Ready)
	r.Get("/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(openapi.Spec)
	})
	r.Get("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(openapi.DocsHTML)
	})
	r.Get("/v1/lyrics/get", l.GetV1)
	r.Get("/v2/lyrics/get", l.GetV2)
	r.Get("/v1/ttml/get", l.GetTTML)
	r.Get("/v1/raw/get", l.GetRaw)
	r.Get("/v1/songlist/search", c.Search)
	r.Get("/v1/metadata/get", c.Metadata)
	r.Get("/v1/lyricsplus/challenge", p.GetChallenge)
	r.Post("/v1/lyricsplus/submit", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		middleware.BodyLimit(cfg.LyricsPlus.MaxBodyBytes)(http.HandlerFunc(p.Submit)).ServeHTTP(w, r)
	}))

	return r
}

// Start begins serving with graceful shutdown.
func (s *Server) Start() error {
	s.watchdog.Start()
	defer s.watchdog.Stop()
	if s.dumper != nil {
		s.dumper.Start()
		defer s.dumper.Stop()
	}
	s.httpSrv = &http.Server{
		Addr:              ":" + s.cfg.Server.Addr,
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.logger.Infof("lyricsplus serving on :%s (version=%s, commit=%s)", s.cfg.Server.Addr, version.Version, version.Commit)
	return s.httpSrv.ListenAndServe()
}

// Shutdown performs a graceful stop.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.dumper != nil {
		s.dumper.Stop()
	}
	if s.httpSrv != nil {
		return s.httpSrv.Shutdown(ctx)
	}
	return nil
}

// Close releases backend resources.
func (s *Server) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func toOrchestratorSources(sources []providers.Source) []orchestrator.Source {
	out := make([]orchestrator.Source, len(sources))
	for i, s := range sources {
		out[i] = s
	}
	return out
}
