package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/storage"
)

func setupTestAdmin(t *testing.T) (*Admin, *storage.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "admin_handler_test.db")
	st, err := storage.NewStore(config.Storage{DBPath: dbPath, LRUSize: 64})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	adminH := &Admin{
		Store:     st,
		JWTSecret: "test-admin-secret",
	}
	return adminH, st
}

func TestAdminHandlersCRUD(t *testing.T) {
	adminH, st := setupTestAdmin(t)
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	// 1. Stats on empty DB
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/stats", nil)
		rr := httptest.NewRecorder()
		adminH.Stats(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 on stats, got %d", rr.Code)
		}
		var stats storage.AdminStats
		if err := json.Unmarshal(rr.Body.Bytes(), &stats); err != nil {
			t.Fatalf("failed to decode stats: %v", err)
		}
		if stats.TotalLyrics != 0 {
			t.Fatalf("expected 0 total lyrics, got %d", stats.TotalLyrics)
		}
	}

	// 2. Create Lyrics via POST
	var createdID int64
	{
		body := AdminCreateRequest{
			Title:       "Levitating",
			Artist:      "Dua Lipa",
			Source:      "apple",
			DurationMS:  203000,
			ISRC:        "GBAYE2000305",
			ContentText: `[{"time":1000,"text":"If you wanna run away with me"}]`,
		}
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/database/lyrics", bytes.NewReader(buf))
		rr := httptest.NewRecorder()
		adminH.CreateLyrics(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201 on create, got %d, body: %s", rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		if resp["success"] != true {
			t.Fatalf("expected success=true, got %v", resp)
		}
		createdID = int64(resp["id"].(float64))
		if createdID <= 0 {
			t.Fatalf("expected positive ID, got %d", createdID)
		}
	}

	// 3. List Lyrics via GET
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/lyrics?query=Levitating", nil)
		rr := httptest.NewRecorder()
		adminH.ListLyrics(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 on list, got %d", rr.Code)
		}
		var res storage.AdminListResult
		_ = json.Unmarshal(rr.Body.Bytes(), &res)
		if res.Total != 1 || len(res.Items) != 1 {
			t.Fatalf("expected 1 item in list, got total=%d items=%d", res.Total, len(res.Items))
		}
		if res.Items[0].Title != "Levitating" {
			t.Fatalf("expected title Levitating, got %q", res.Items[0].Title)
		}
	}

	// 4. Get Lyric by ID
	{
		r := chi.NewRouter()
		r.Get("/v1/admin/database/lyrics/{id}", adminH.GetLyric)
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/database/lyrics/1", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 on get by ID, got %d", rr.Code)
		}
	}

	// 5. Update Lyric by ID
	{
		newTitle := "Levitating (DaBaby Remix)"
		updatePayload := AdminUpdatePayload{
			Title: &newTitle,
		}
		buf, _ := json.Marshal(updatePayload)
		r := chi.NewRouter()
		r.Put("/v1/admin/database/lyrics/{id}", adminH.UpdateLyric)
		req := httptest.NewRequest(http.MethodPut, "/v1/admin/database/lyrics/1", bytes.NewReader(buf))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 on update, got %d, body: %s", rr.Code, rr.Body.String())
		}

		// Verify update in store
		row, err := st.AdminGetByID(ctx, createdID)
		if err != nil {
			t.Fatalf("AdminGetByID failed: %v", err)
		}
		if row.Title != newTitle {
			t.Fatalf("expected title %q, got %q", newTitle, row.Title)
		}
	}

	// 6. Delete Lyric by ID
	{
		r := chi.NewRouter()
		r.Delete("/v1/admin/database/lyrics/{id}", adminH.DeleteLyric)
		req := httptest.NewRequest(http.MethodDelete, "/v1/admin/database/lyrics/1", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 on delete, got %d", rr.Code)
		}

		// Verify deletion
		stats, _ := st.AdminStats(ctx)
		if stats.TotalLyrics != 0 {
			t.Fatalf("expected 0 lyrics after delete, got %d", stats.TotalLyrics)
		}
	}

	// 7. Batch Delete
	{
		// Create 2 rows first
		_ = st.AdminCreate(ctx, &storage.Row{Title: "Song A", Artist: "Artist A", Source: "apple", ContentJSON: []byte("{}"), CreatedAt: time.Now()})
		_ = st.AdminCreate(ctx, &storage.Row{Title: "Song B", Artist: "Artist B", Source: "apple", ContentJSON: []byte("{}"), CreatedAt: time.Now()})

		stats, _ := st.AdminStats(ctx)
		if stats.TotalLyrics != 2 {
			t.Fatalf("expected 2 lyrics, got %d", stats.TotalLyrics)
		}

		batchReq := BatchDeleteRequest{IDs: []int64{2, 3}}
		buf, _ := json.Marshal(batchReq)
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/database/lyrics/batch-delete", bytes.NewReader(buf))
		rr := httptest.NewRecorder()
		adminH.BatchDeleteLyrics(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 on batch delete, got %d", rr.Code)
		}

		stats, _ = st.AdminStats(ctx)
		if stats.TotalLyrics != 0 {
			t.Fatalf("expected 0 lyrics after batch delete, got %d", stats.TotalLyrics)
		}
	}
}

func TestAdminManualScrapeValidation(t *testing.T) {
	adminH, st := setupTestAdmin(t)
	defer func() { _ = st.Close() }()

	// Missing title & artist & isrc -> 400
	body := ScrapeRequest{}
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/database/scrape", bytes.NewReader(buf))
	rr := httptest.NewRecorder()
	adminH.ManualScrape(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on missing params, got %d", rr.Code)
	}
}

func TestParseDurationInput(t *testing.T) {
	tests := []struct {
		input    any
		expected int
	}{
		{input: 200.5, expected: 200500},
		{input: 180000, expected: 180000},
		{input: "3:30", expected: 210000},
		{input: "215", expected: 215000},
		{input: "215000", expected: 215000},
		{input: nil, expected: 0},
	}

	for _, tc := range tests {
		got := parseDurationInput(tc.input)
		if got != tc.expected {
			t.Errorf("parseDurationInput(%v) = %d, want %d", tc.input, got, tc.expected)
		}
	}
}

func TestExtForSource(t *testing.T) {
	tests := []struct {
		source   string
		expected string
	}{
		{"apple", "ttml"},
		{"Apple Music", "ttml"},
		{"qq", "qrc"},
		{"QQ Music", "qrc"},
		{"lyricsplus", "json"},
		{"spotify", "json"},
		{"musixmatch", "json"},
	}

	for _, tc := range tests {
		got := extForSource(tc.source)
		if got != tc.expected {
			t.Errorf("extForSource(%q) = %q, want %q", tc.source, got, tc.expected)
		}
	}
}

func TestCanonicalFilenameReference(t *testing.T) {
	fn := storage.CanonicalFilename("The Weeknd", "Blinding Lights", "", 200000, "USUM71920008", "", "json")
	if fn == "" {
		t.Fatalf("expected non-empty canonical filename")
	}
}

func TestDomainSearchQueryCompatibility(t *testing.T) {
	q := domain.SearchQuery{
		Title:  "Test Song",
		Artist: "Test Artist",
	}
	if q.Title != "Test Song" {
		t.Fatalf("expected test song")
	}
}
