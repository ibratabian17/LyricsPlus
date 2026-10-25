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

type Server struct {
	cfg      config.Config
	router   http.Handler
	logger   *logger.Logger
	store    *storage.Store
	watchdog *storage.Watchdog
	dumper   *storage.Dumper
	gdrive   *storage.GDriveClient
	httpSrv  *http.Server
	powH     *handlers.Pow
	adminH   *handlers.Admin
}

func New(cfg config.Config, lg *logger.Logger) (*Server, error) {
	if lg == nil {
		lg = logger.Nop()
	}
	store, err := storage.NewStore(cfg.Storage)
	if err != nil {
		return nil, err
	}

	httpCache := storage.NewMemoryCache(cfg.Cache)

	limiter := orchestrator.NewLimiter(orchestrator.LimiterConfig{
		Global:    cfg.Provider.MaxConcurrentFetches,
		PerSource: cfg.Provider.MaxConcurrentPerSource,
		Wait:      cfg.Provider.FetchAdmissionWait,
	})

	proxyClient := proxy.New(cfg.Server)
	gdrive := storage.NewGDriveClient(cfg.GDrive, nil)
	factory := &providers.Factory{
		Client:  proxyClient,
		Store:   store,
		GDrive:  gdrive,
		Limiter: limiter,
		Config:  &cfg,
		Logger:  lg,
	}
	providerSet, err := factory.BuildSet()
	if err != nil {
		return nil, err
	}

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
		metrics.Default.RegisterPlatform("gdrive", "Google Drive", gdrive.IsEnabled, "Google Drive Storage Availability")
	}
	if store != nil {
		metrics.Default.RegisterPlatform("database", "LyricsDB", func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return store.Reachable(ctx) == nil
		}, "Local LyricsPlus UGC + Cache Database Availability")
	}

	racer := orchestrator.NewRacer(toOrchestratorSources(providerSet.Sources), cfg.Provider.Timeout,
		orchestrator.WithLogger(lg),
		orchestrator.WithLimiter(limiter),
		orchestrator.WithBreaker(providerSet.Breaker),
	)
	dedup := orchestrator.NewDedup(racer)

	svc := &service.Service{
		Dedup:       dedup,
		Store:       store,
		MemCache:    httpCache,
		Logger:      lg,
		NegativeTTL: cfg.Storage.NegativeTTL,
	}
	racer.SetSaver(svc.SaveResultAsync)

	watchdog := storage.NewWatchdog(cfg.Cache.MemoryWatchdogByte, 10*time.Second, httpCache, store, svc)

	lyricsH := &handlers.Lyrics{Service: svc, Logger: lg, KpoeInfo: "lyricsplus"}
	catalogH := &handlers.Catalog{
		Store:      store,
		AppleMusic: providerSet.AppleMusic,
		Spotify:    providerSet.Spotify,
		Musixmatch: providerSet.Musixmatch,
		Deezer:     providerSet.Deezer,
		QQMusic:    providerSet.QQMusic,
		Logger:     lg,
	}
	powH := &handlers.Pow{
		Issuer:           lyricsplus.NewIssuer(cfg.LyricsPlus.JWTSecret, cfg.LyricsPlus.ChallengeTTL),
		Verifier:         lyricsplus.NewVerifier(cfg.LyricsPlus.JWTSecret, cfg.LyricsPlus.PoWDifficulty),
		LyricsPlus:       providerSet.LyricsPlus,
		Store:            store,
		GDrive:           gdrive,
		MaxBodyBytes:     cfg.LyricsPlus.MaxBodyBytes,
		AcceptVandal:     cfg.LyricsPlus.AcceptVandalism,
		AllowSubmissions: cfg.LyricsPlus.AllowSubmissions,
		Logger:           lg,
	}
	healthH := &handlers.Health{
		Store:     store,
		Started:   time.Now(),
		Version:   version.Version,
		BuildDate: version.BuildDate,
	}

	var dumper *storage.Dumper
	if cfg.GDrive.DailyDumpEnabled {
		dumper = storage.NewDumper(cfg, store, gdrive, lg)
	}

	effectiveAdminToken := cfg.LyricsPlus.EffectiveAdminToken()
	adminH := &handlers.Admin{
		Store:       store,
		Service:     svc,
		ProviderSet: providerSet,
		Racer:       racer,
		JWTSecret:   effectiveAdminToken,
		Logger:      lg,
	}

	router := newRouter(cfg, lg, lyricsH, catalogH, powH, healthH, adminH)

	return &Server{
		cfg:      cfg,
		router:   router,
		logger:   lg,
		store:    store,
		watchdog: watchdog,
		dumper:   dumper,
		gdrive:   gdrive,
		powH:     powH,
		adminH:   adminH,
	}, nil
}

func newRouter(cfg config.Config, lg *logger.Logger, l *handlers.Lyrics, c *handlers.Catalog, p *handlers.Pow, h *handlers.Health, adminHandler ...*handlers.Admin) http.Handler {
	var adm *handlers.Admin
	if len(adminHandler) > 0 && adminHandler[0] != nil {
		adm = adminHandler[0]
	} else {
		adm = &handlers.Admin{JWTSecret: cfg.LyricsPlus.JWTSecret, Logger: lg}
	}

	r := chi.NewRouter()
	r.Use(
		middleware.Tracing(lg),
		middleware.PanicRecovery(lg),
		middleware.CORS(),
		middleware.ExternalScraping(cfg.ExternalScraping, lg),
		middleware.QueryLimits(cfg.Server.MaxURLBytes, cfg.Server.MaxQueryParams, cfg.Server.MaxQueryValueLen),
		middleware.RateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window),
		middleware.ConcurrencyLimiter(int64(cfg.Server.MaxConcurrency)),
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

	r.Route("/v1/admin/database", func(admin chi.Router) {
		admin.Use(middleware.BodyLimit(1 << 20)) // 1MB payload limit
		admin.Use(middleware.AdminAuth(cfg.LyricsPlus.AdminToken, cfg.LyricsPlus.JWTSecret))
		admin.Get("/", adm.Stats)
		admin.Get("/stats", adm.Stats)
		admin.Get("/lyrics", adm.ListLyrics)
		admin.Post("/lyrics", adm.CreateLyrics)
		admin.Get("/lyrics/{id}", adm.GetLyric)
		admin.Put("/lyrics/{id}", adm.UpdateLyric)
		admin.Delete("/lyrics/{id}", adm.DeleteLyric)
		admin.Post("/lyrics/batch-delete", adm.BatchDeleteLyrics)
		admin.Post("/scrape", adm.ManualScrape)
		admin.Post("/scrape-sources", adm.ScrapeSources)
	})

	return r
}

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
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	s.logger.Infof("lyricsplus serving on :%s (version=%s, commit=%s, buildDate=%s)", s.cfg.Server.Addr, version.Version, version.Commit, version.BuildDate)
	if version.BuildDate == "unknown" || version.Commit == "none" {
		s.logger.Warnf("build metadata incomplete: buildDate=%s, commit=%s (consider building with build args or inside a git repository)", version.BuildDate, version.Commit)
	}
	return s.httpSrv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.dumper != nil {
		s.dumper.Stop()
	}
	var err error
	if s.httpSrv != nil {
		err = s.httpSrv.Shutdown(ctx)
	}
	if s.powH != nil {
		_ = s.powH.WaitBackground(ctx)
	}
	return err
}

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
