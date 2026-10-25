package providers

import (
	"context"
	"errors"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/storage"
)

var ErrNotConfigured = errNotConfigured("provider not configured")

type errNotConfigured string

func (e errNotConfigured) Error() string { return string(e) }

type Source interface {
	orchestrator.Source
	Configured() bool
}

type Factory struct {
	Client  *proxy.Client
	Store   *storage.Store
	GDrive  *storage.GDriveClient
	Limiter *orchestrator.Limiter
	Config  *config.Config
	Logger  *logger.Logger
}

type Set struct {
	Sources    []Source
	AppleMusic *AppleMusicProvider
	Spotify    *SpotifyProvider
	Musixmatch *MusixmatchProvider
	Deezer     *DeezerProvider
	QQMusic    *QQMusicProvider
	LyricsPlus *LyricsPlusProvider
	Breaker    *orchestrator.Breaker
	Limiter    *orchestrator.Limiter
}

var errBreakerOpen = errors.New("provider circuit breaker open")

type guardedLineSource struct {
	name string
	src  LineSource
	br   *orchestrator.Breaker
}

func (g guardedLineSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	if !g.br.Allow(g.name) {
		return nil, errBreakerOpen
	}
	resp, err := g.src.FetchLyrics(ctx, q)
	switch {
	case errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled:
		g.br.Neutral(g.name)
	case err != nil && orchestrator.IsHealthFailure(err):
		g.br.Report(g.name, false)
	default:
		g.br.Report(g.name, true)
	}
	return resp, err
}

func (f *Factory) BuildSet() (*Set, error) {
	return buildAll(f.Client, f.Store, f.GDrive, f.Limiter, f.Config, f.Logger)
}

func (f *Factory) Build() ([]Source, error) {
	set, err := f.BuildSet()
	if err != nil {
		return nil, err
	}
	return set.Sources, nil
}

func buildAll(client *proxy.Client, store *storage.Store, gdrive *storage.GDriveClient, limiter *orchestrator.Limiter, cfg *config.Config, lg *logger.Logger) (*Set, error) {
	var pcfg config.Provider
	if cfg != nil {
		pcfg = cfg.Provider
	} else {
		pcfg = config.Load().Provider
	}

	if limiter == nil {
		limiter = orchestrator.NewLimiter(orchestrator.LimiterConfig{
			Global:    pcfg.MaxConcurrentFetches,
			PerSource: pcfg.MaxConcurrentPerSource,
			Wait:      pcfg.FetchAdmissionWait,
		})
	}

	apple := NewAppleMusicWithConfig(client, pcfg)
	qq := NewQQMusic(client, pcfg.QQCookie)
	mxm := NewMusixmatchWithConfig(client, pcfg, "musixmatch", false)
	mxmWord := NewMusixmatchWithConfig(client, pcfg, "musixmatch-word", true)
	mxmWord.ShareStateFrom(mxm)
	dz := NewDeezerWithConfig(client, pcfg)
	sp := NewSpotifyWithConfig(client, pcfg)

	if lg != nil {
		for _, s := range []Source{apple, qq, mxm, mxmWord, dz, sp} {
			if l, ok := s.(interface{ SetLogger(*logger.Logger) }); ok {
				l.SetLogger(lg)
			}
		}
	}

	breaker := orchestrator.NewBreaker(orchestrator.DefaultBreakerConfig())
	qapleSvc := NewQapleService(
		guardedLineSource{name: "qq", src: qq, br: breaker},
		guardedLineSource{name: "apple", src: apple, br: breaker},
		guardedLineSource{name: "musixmatch", src: mxm, br: breaker},
	)
	if store != nil {
		qapleSvc.SetStore(store)
	}
	lp := NewLyricsPlus(client)
	lp.SetQaple(qapleSvc)
	if store != nil {
		lp.SetStore(store)
	}
	if gdrive != nil {
		lp.SetGDrive(gdrive)
	}
	if lg != nil {
		lp.SetLogger(lg)
	}

	sources := []Source{
		apple,
		lp,
		dz,
		qq,
		sp,
		mxmWord,
		mxm,
	}

	return &Set{
		Sources:    sources,
		AppleMusic: apple,
		Spotify:    sp,
		Musixmatch: mxm,
		Deezer:     dz,
		QQMusic:    qq,
		LyricsPlus: lp,
		Breaker:    breaker,
		Limiter:    limiter,
	}, nil
}

func ProvidePlugin(_ *proxy.Client, _ map[string]orchestrator.Source) {}

func Empty(resp *domain.LyricsResponse) bool {
	return resp == nil || len(resp.Lyrics) == 0
}
