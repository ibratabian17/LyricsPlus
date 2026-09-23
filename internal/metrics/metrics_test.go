package metrics

import (
	"testing"
)

func TestMetricsCollector(t *testing.T) {
	c := NewCollector()

	// Register dummy platforms
	c.RegisterPlatform("apple", "Apple Music", func() bool { return true }, "test")
	c.RegisterPlatform("spotify", "Spotify", func() bool { return false }, "test")

	// Record HTTP requests
	c.RecordHTTPRequest(200)
	c.RecordHTTPRequest(200)
	c.RecordHTTPRequest(404)
	c.RecordHTTPRequest(500)

	// Record Lyrics Lookups
	c.RecordLyricsLookup("spotify", "none", true)
	c.RecordLyricsLookup("apple", "database", true)
	c.RecordLyricsLookup("", "", false)

	// Record Provider Calls
	c.RecordProviderCall("apple", "OK", 50)
	c.RecordProviderCall("apple", "OK", 60)
	c.RecordProviderCall("spotify", "BAD", 100)

	snap := c.Snapshot()

	if snap.Requests.TotalRequests != 4 {
		t.Fatalf("expected 4 total requests, got %d", snap.Requests.TotalRequests)
	}
	if snap.Requests.SuccessRequests != 2 {
		t.Fatalf("expected 2 success requests, got %d", snap.Requests.SuccessRequests)
	}
	if snap.Requests.FailedRequests != 2 {
		t.Fatalf("expected 2 failed requests, got %d", snap.Requests.FailedRequests)
	}
	if snap.Requests.SuccessRatePercent != 50.0 {
		t.Fatalf("expected 50.0 success rate, got %f", snap.Requests.SuccessRatePercent)
	}
	if snap.Requests.FailureRatePercent != 50.0 {
		t.Fatalf("expected 50.0 failure rate, got %f", snap.Requests.FailureRatePercent)
	}

	if snap.Lyrics.TotalLookups != 3 {
		t.Fatalf("expected 3 total lookups, got %d", snap.Lyrics.TotalLookups)
	}
	if snap.Lyrics.Found != 2 || snap.Lyrics.NotFound != 1 {
		t.Fatalf("expected 2 found, 1 not found; got %d, %d", snap.Lyrics.Found, snap.Lyrics.NotFound)
	}

	applePlat, ok := snap.Platforms["apple"]
	if !ok || !applePlat.Active || applePlat.Status != "ONLINE" {
		t.Fatalf("expected online active apple platform, got %+v", applePlat)
	}
	if applePlat.TotalCalls != 2 || applePlat.SuccessCalls != 2 {
		t.Fatalf("expected 2 total and 2 success calls for apple, got %+v", applePlat)
	}

	spotifyPlat, ok := snap.Platforms["spotify"]
	if !ok || spotifyPlat.Active || spotifyPlat.Status != "NOT_CONFIGURED" {
		t.Fatalf("expected not configured spotify platform, got %+v", spotifyPlat)
	}
}
