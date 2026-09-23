package providers

import (
	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/orchestrator"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/storage"
)

// ErrNotConfigured indicates missing credentials for a provider.
var ErrNotConfigured = errNotConfigured("provider not configured")

type errNotConfigured string

func (e errNotConfigured) Error() string { return string(e) }

// Source is implemented by every lyrics provider.
type Source interface {
	orchestrator.Source
	Configured() bool
}

// Factory assembles the provider set available to the racer.
type Factory struct {
	Client *proxy.Client
	Store  *storage.Store
	GDrive *storage.GDriveClient
	Config *config.Config
	Logger *logger.Logger
}

// Set holds typed references to each configured provider and the ordered source slice.
type Set struct {
	Sources    []Source
	AppleMusic *AppleMusicProvider
	Spotify    *SpotifyProvider
	Musixmatch *MusixmatchProvider
	Deezer     *DeezerProvider
	QQMusic    *QQMusicProvider
	LyricsPlus *LyricsPlusProvider
}

// BuildSet returns all providers as a typed Set.
func (f *Factory) BuildSet() (*Set, error) {
	return buildAll(f.Client, f.Store, f.GDrive, f.Config, f.Logger)
}

// Build returns all provider sources.
func (f *Factory) Build() ([]Source, error) {
	set, err := f.BuildSet()
	if err != nil {
		return nil, err
	}
	return set.Sources, nil
}

func buildAll(client *proxy.Client, store *storage.Store, gdrive *storage.GDriveClient, cfg *config.Config, lg *logger.Logger) (*Set, error) {
	var pcfg config.Provider
	if cfg != nil {
		pcfg = cfg.Provider
	} else {
		pcfg = config.Load().Provider
	}

	apple := NewAppleMusicWithConfig(client, pcfg)
	qq := NewQQMusic(client, pcfg.QQCookie)
	mxm := NewMusixmatchWithConfig(client, pcfg, "musixmatch", false)
	mxmWord := NewMusixmatchWithConfig(client, pcfg, "musixmatch-word", true)
	dz := NewDeezerWithConfig(client, pcfg)
	sp := NewSpotifyWithConfig(client, pcfg)

	if lg != nil {
		for _, s := range []Source{apple, qq, mxm, mxmWord, dz, sp} {
			if l, ok := s.(interface{ SetLogger(*logger.Logger) }); ok {
				l.SetLogger(lg)
			}
		}
	}

	qapleSvc := NewQapleService(qq, apple, mxm)
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
	}, nil
}

// ProvidePlugin is a hook reserved for custom source registrations.
func ProvidePlugin(_ *proxy.Client, _ map[string]orchestrator.Source) {}

// Empty marks an empty response as a miss.
func Empty(resp *domain.LyricsResponse) bool {
	return resp == nil || len(resp.Lyrics) == 0
}
