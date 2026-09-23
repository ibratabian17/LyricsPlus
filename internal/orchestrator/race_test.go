package orchestrator

import (
	"context"
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
	// A line-typed payload with actual syllable content grades as word sync.
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

	// If lyricsplus (phase1 index 1) returns P3, apple's pending P2 always loses.
	// Both phase-1 sources are instant; lyricsplus wins by priority.
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
	// Phase1 yields P2 from apple -> Phase2 should upgrade to deezer P3.
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
