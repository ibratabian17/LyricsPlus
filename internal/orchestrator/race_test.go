package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lyricsplus/backend/internal/domain"
)

func wordResp(source string) *domain.LyricsResponse {
	return &domain.LyricsResponse{
		Type:     domain.SyncTypeWord,
		Metadata: domain.LyricsMetadata{Source: source, SongWriters: []string{}},
		Lyrics: []domain.Line{
			{Time: 0, Duration: 1000, Text: "x", Syllabus: []domain.Syllable{{Time: 0, Duration: 500, Text: "x "}}},
		},
	}
}

func lineResp(source string) *domain.LyricsResponse {
	return &domain.LyricsResponse{
		Type:     domain.SyncTypeLine,
		Metadata: domain.LyricsMetadata{Source: source, SongWriters: []string{}},
		Lyrics:   []domain.Line{{Time: 0, Duration: 1000, Text: "x"}},
	}
}

type stubSource struct {
	name string
	resp *domain.LyricsResponse
	err  error
}

func (s *stubSource) Name() string { return s.name }
func (s *stubSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	return s.resp, s.err
}

func TestGrade(t *testing.T) {
	if Grade(wordResp("spotify"), "spotify") != PriorityWord {
		t.Error("spotify word should be priority 3")
	}
	if Grade(wordResp("qq"), "qq") != PriorityWord {
		t.Error("qq word should be priority 3")
	}
	if Grade(lineResp("deezer"), "deezer") != PriorityLine {
		t.Error("line should be priority 2")
	}

	apple := lineResp("apple")
	apple.Lyrics[0].Syllabus = []domain.Syllable{{Time: 0, Duration: 500, Text: "x "}}
	if Grade(apple, "apple") != PriorityWord {
		t.Error("apple with syllabus should be priority 3")
	}
	if Grade(nil, "x") != PriorityFailed {
		t.Error("nil should be priority 0")
	}
}

func TestRacerWordSyncWinsImmediately(t *testing.T) {
	racer := NewRacer([]Source{
		&stubSource{name: "apple", resp: lineResp("apple")},
		&stubSource{name: "lyricsplus", resp: wordResp("lyricsplus")},
		&stubSource{name: "deezer", resp: nil},
	}, 2*time.Second)

	res := racer.Race(context.Background(), domain.SearchQuery{Title: "t", Artist: "a"}, nil)
	if res == nil || res.Source != "lyricsplus" || res.Priority != PriorityWord {
		t.Fatalf("expected lyricsplus P3 winner, got %+v", res)
	}
}

func TestRacerPhases(t *testing.T) {
	racer := NewRacer([]Source{
		&stubSource{name: "apple", resp: lineResp("apple")},
		&stubSource{name: "lyricsplus", resp: nil},
		&stubSource{name: "deezer", resp: wordResp("deezer")},
		&stubSource{name: "qq", resp: nil},
	}, 2*time.Second)

	res := racer.Race(context.Background(), domain.SearchQuery{Title: "t", Artist: "a"}, nil)
	if res == nil || res.Source != "deezer" || res.Priority != PriorityWord {
		t.Fatalf("expected deezer P3 phase-2 upgrade, got %+v", res)
	}
}

func TestRacerKeepsLineSyncWhenNoUpgrade(t *testing.T) {
	racer := NewRacer([]Source{
		&stubSource{name: "apple", resp: lineResp("apple")},
		&stubSource{name: "lyricsplus", resp: nil},
		&stubSource{name: "deezer", resp: lineResp("deezer")},
	}, 2*time.Second)
	res := racer.Race(context.Background(), domain.SearchQuery{Title: "t", Artist: "a"}, nil)
	if res == nil || res.Source != "apple" {
		t.Fatalf("expected apple P2 fallback, got %+v", res)
	}
}

func TestDedupSharesResult(t *testing.T) {
	racer := NewRacer([]Source{
		&stubSource{name: "apple", resp: lineResp("apple")},
		&stubSource{name: "lyricsplus", resp: nil},
	}, 2*time.Second)
	d := NewDedup(racer)
	q := domain.SearchQuery{Title: "same", Artist: "song"}
	a, err := d.Get(context.Background(), q, nil)
	if err != nil || a == nil {
		t.Fatalf("first call failed: %v", err)
	}
	b, err := d.Get(context.Background(), q, nil)
	if err != nil || b == nil {
		t.Fatalf("second call failed: %v", err)
	}
	if a.Source != b.Source {
		t.Errorf("dedup should share the winner")
	}
	if a.Resp == b.Resp {
		t.Errorf("callers should receive independent copies")
	}
}

func TestRacePriorityUpgradeNoLimiterLeak(t *testing.T) {
	limiter := NewLimiter(LimiterConfig{Global: 2, PerSource: 2, Wait: 50 * time.Millisecond})
	racer := NewRacer([]Source{
		&stubSource{name: "apple", resp: lineResp("apple")},
		&stubSource{name: "lyricsplus", resp: nil},
		&stubSource{name: "qq", resp: wordResp("qq")},
	}, 2*time.Second, WithLimiter(limiter))

	// If raceForPriority or runPhase leaks semaphore tokens, iterations > Global (2) will fail with OVERLOAD/SKIP
	for i := 0; i < 10; i++ {
		res := racer.Race(context.Background(), domain.SearchQuery{Title: "t", Artist: "a"}, nil)
		if res == nil || res.Source != "qq" || res.Priority != PriorityWord {
			t.Fatalf("iteration %d: expected qq P3 winner, got %+v", i, res)
		}
	}
}

func TestRaceSiblingCancelNotRTO(t *testing.T) {
	slowCalled := make(chan struct{})
	racer := NewRacer([]Source{
		&stubSource{name: "apple", resp: wordResp("apple")},
		&delayStubSource{name: "lyricsplus", delay: 200 * time.Millisecond, onStart: func() { close(slowCalled) }},
	}, 2*time.Second)

	res := racer.Race(context.Background(), domain.SearchQuery{Title: "t", Artist: "a"}, nil)
	if res == nil || res.Source != "apple" {
		t.Fatalf("expected apple winner, got %+v", res)
	}

	lpStatus, ok := res.SourcesStatus["lyricsplus"]
	if !ok {
		t.Fatalf("expected lyricsplus in SourcesStatus")
	}
	if lpStatus.Status == "RTO" {
		t.Fatalf("cancelled sibling should not have RTO status, got %s", lpStatus.Status)
	}
}

type delayStubSource struct {
	name    string
	delay   time.Duration
	onStart func()
}

func (s *delayStubSource) Name() string { return s.name }
func (s *delayStubSource) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	if s.onStart != nil {
		s.onStart()
	}
	select {
	case <-time.After(s.delay):
		return wordResp(s.name), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRaceBackgroundSiblingSavesToDB(t *testing.T) {
	var savedResult atomic.Pointer[Result]
	racer := NewRacer([]Source{
		&stubSource{name: "apple", resp: wordResp("apple")},
		&delayStubSource{name: "qq", delay: 40 * time.Millisecond},
	}, 2*time.Second, WithSaver(func(q domain.SearchQuery, res *Result) {
		if res.Source == "qq" {
			savedResult.Store(res)
		}
	}))

	res := racer.Race(context.Background(), domain.SearchQuery{Title: "t", Artist: "a"}, []string{"apple", "qq"})
	if res == nil || res.Source != "apple" {
		t.Fatalf("expected apple winner immediately, got %+v", res)
	}

	// Wait for background sibling to finish and save
	time.Sleep(100 * time.Millisecond)

	saved := savedResult.Load()
	if saved == nil {
		t.Fatalf("expected background sibling qq to save to DB, but saver was not called")
	}
	if saved.Source != "qq" || len(saved.Resp.Lyrics) == 0 {
		t.Fatalf("unexpected saved result: %+v", saved)
	}
}
