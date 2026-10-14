package providers

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/storage"
)

type mockLineSource struct {
	fetchCount int64
	resp       *domain.LyricsResponse
	err        error
}

func (m *mockLineSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	atomic.AddInt64(&m.fetchCount, 1)
	return m.resp, m.err
}

func TestQapleWithStore(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_qaple.db")
	st, err := storage.NewStore(config.Storage{DBPath: dbPath, LRUSize: 100})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Seed QQ QRC lyric into store
	qrcRaw := `<?xml version="1.0" encoding="utf-8"?>
<QrcInfos>
<LyricInfo LyricCount="1">
<Lyric_1 LyricType="1" LyricContent="[ti:Test Song]
[ar:Test Artist]
[0,3000]Hello(0,1500) world(1500,1500)
"/>
</LyricInfo>
</QrcInfos>`

	st.SaveLyricsAsync(&storage.Row{
		Filename:    "Test Artist - Test Song.qrc",
		ContentJSON: []byte(qrcRaw),
		Source:      "qq",
		Title:       "Test Song",
		Artist:      "Test Artist",
		DurationMS:  180000,
		CreatedAt:   time.Now(),
	}, 5*time.Second)

	// Seed Apple TTML lyric into store
	ttmlRaw := `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml">
  <body>
    <div>
      <p begin="00:00.000" end="00:03.000">Hello world</p>
    </div>
  </body>
</tt>`

	st.SaveLyricsAsync(&storage.Row{
		Filename:    "Test Artist - Test Song.ttml",
		ContentJSON: []byte(ttmlRaw),
		Source:      "apple",
		Title:       "Test Song",
		Artist:      "Test Artist",
		DurationMS:  180000,
		CreatedAt:   time.Now(),
	}, 5*time.Second)

	time.Sleep(50 * time.Millisecond)

	qqMock := &mockLineSource{resp: nil}
	appleMock := &mockLineSource{resp: nil}
	mxmMock := &mockLineSource{resp: nil}

	svc := NewQapleService(qqMock, appleMock, mxmMock)
	svc.SetStore(st)

	q := domain.SearchQuery{
		Title:    "Test Song",
		Artist:   "Test Artist",
		Duration: 180000,
	}

	resp, err := svc.FetchLyrics(ctx, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("expected non-nil response from Qaple with DB cache")
	}
	if len(resp.Lyrics) == 0 {
		t.Fatalf("expected lyrics lines synthesized from DB cache")
	}

	// Live network sources should NOT have been called because both were in DB
	if atomic.LoadInt64(&qqMock.fetchCount) != 0 {
		t.Errorf("expected qqMock not to be called, got %d", atomic.LoadInt64(&qqMock.fetchCount))
	}
	if atomic.LoadInt64(&appleMock.fetchCount) != 0 {
		t.Errorf("expected appleMock not to be called, got %d", atomic.LoadInt64(&appleMock.fetchCount))
	}
}

func TestQaplePartialStoreHit(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_qaple_partial.db")
	st, err := storage.NewStore(config.Storage{DBPath: dbPath, LRUSize: 100})
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer st.Close()

	ctx := context.Background()

	// Seed QQ QRC only
	qrcRaw := `<?xml version="1.0" encoding="utf-8"?>
<QrcInfos>
<LyricInfo LyricCount="1">
<Lyric_1 LyricType="1" LyricContent="[ti:Partial Song]
[ar:Partial Artist]
[0,3000]Hello(0,1500) world(1500,1500)
"/>
</LyricInfo>
</QrcInfos>`

	st.SaveLyricsAsync(&storage.Row{
		Filename:    "Partial Artist - Partial Song.qrc",
		ContentJSON: []byte(qrcRaw),
		Source:      "qq",
		Title:       "Partial Song",
		Artist:      "Partial Artist",
		DurationMS:  180000,
		CreatedAt:   time.Now(),
	}, 5*time.Second)

	time.Sleep(50 * time.Millisecond)

	qqMock := &mockLineSource{resp: nil}
	appleMock := &mockLineSource{
		resp: &domain.LyricsResponse{
			Lyrics: []domain.Line{
				{Text: "Hello world", Time: 0, Duration: 3000},
			},
			Metadata: domain.LyricsMetadata{Source: "Apple"},
		},
	}

	svc := NewQapleService(qqMock, appleMock, nil)
	svc.SetStore(st)

	q := domain.SearchQuery{
		Title:    "Partial Song",
		Artist:   "Partial Artist",
		Duration: 180000,
	}

	resp, err := svc.FetchLyrics(ctx, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatalf("expected non-nil response")
	}

	// QQ was in DB -> qqMock should not be called
	if atomic.LoadInt64(&qqMock.fetchCount) != 0 {
		t.Errorf("expected qqMock not to be called, got %d", atomic.LoadInt64(&qqMock.fetchCount))
	}
	// Apple was not in DB -> appleMock should have been called
	if atomic.LoadInt64(&appleMock.fetchCount) != 1 {
		t.Errorf("expected appleMock to be called once, got %d", atomic.LoadInt64(&appleMock.fetchCount))
	}
}
