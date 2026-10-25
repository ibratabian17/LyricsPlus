package service

import (
	"testing"

	"lyricsplus/backend/internal/orchestrator"
)

func TestShouldCacheNegativeResult(t *testing.T) {
	mk := func(m map[string]orchestrator.SourceOutcome) *orchestrator.Result {
		return &orchestrator.Result{SourcesStatus: m}
	}
	cases := []struct {
		name string
		res  *orchestrator.Result
		want bool
	}{
		{"nil", nil, false},
		{"empty", mk(nil), false},
		{"clean miss", mk(map[string]orchestrator.SourceOutcome{
			"apple": {Status: "BAD"}, "qq": {Status: "BAD"}, "deezer": {Status: "SKIP"},
		}), true},
		{"one provider failed", mk(map[string]orchestrator.SourceOutcome{
			"apple": {Status: "BAD"}, "musixmatch": {Status: "BAD", Degraded: true},
		}), false},
		{"overloaded skip", mk(map[string]orchestrator.SourceOutcome{
			"apple": {Status: "BAD"}, "qq": {Status: "SKIP", Degraded: true},
		}), false},
		{"timeout", mk(map[string]orchestrator.SourceOutcome{
			"apple": {Status: "BAD"}, "qq": {Status: "RTO", Degraded: true},
		}), false},
		{"cancelled", mk(map[string]orchestrator.SourceOutcome{
			"apple": {Status: "BAD"}, "qq": {Status: "CANCEL"},
		}), false},
		{"all skipped", mk(map[string]orchestrator.SourceOutcome{
			"apple": {Status: "SKIP", Degraded: true},
		}), false},
		{"unconfigured only", mk(map[string]orchestrator.SourceOutcome{
			"deezer": {Status: "SKIP"},
		}), false},
	}
	for _, c := range cases {
		if got := shouldCacheNegativeResult(c.res); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
