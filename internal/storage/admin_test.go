package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lyricsplus/backend/internal/config"
)

func TestStoreAdminOperations(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "admin_test.db")
	st, err := NewStore(config.Storage{DBPath: dbPath, LRUSize: 64})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	// 1. Initial stats on empty DB
	stats, err := st.AdminStats(ctx)
	if err != nil {
		t.Fatalf("AdminStats failed: %v", err)
	}
	if stats.TotalLyrics != 0 {
		t.Fatalf("expected 0 total lyrics, got %d", stats.TotalLyrics)
	}
	if stats.IsResting {
		t.Fatalf("expected store not resting")
	}

	// 2. Create row
	row1 := &Row{
		Title:       "Blinding Lights",
		Artist:      "The Weeknd",
		Source:      "apple",
		DurationMS:  200000,
		ISRC:        "USUM71920008",
		PlatformID:  "1499378607",
		ContentJSON: []byte(`{"lyrics":[{"time":1000,"text":"Yeah"}]}`),
		CreatedAt:   time.Now(),
	}
	if err := st.AdminCreate(ctx, row1); err != nil {
		t.Fatalf("AdminCreate failed: %v", err)
	}
	if row1.ID <= 0 {
		t.Fatalf("expected positive row ID, got %d", row1.ID)
	}

	// 3. Create second row
	row2 := &Row{
		Title:       "Save Your Tears",
		Artist:      "The Weeknd",
		Source:      "spotify",
		DurationMS:  215000,
		ContentJSON: []byte(`{"lyrics":[{"time":2000,"text":"Ooh"}]}`),
		CreatedAt:   time.Now(),
	}
	if err := st.AdminCreate(ctx, row2); err != nil {
		t.Fatalf("AdminCreate row2 failed: %v", err)
	}

	// 4. Verify stats after inserts
	stats, err = st.AdminStats(ctx)
	if err != nil {
		t.Fatalf("AdminStats failed: %v", err)
	}
	if stats.TotalLyrics != 2 {
		t.Fatalf("expected 2 lyrics, got %d", stats.TotalLyrics)
	}
	if stats.BySource["apple"] != 1 || stats.BySource["spotify"] != 1 {
		t.Fatalf("unexpected source breakdown: %v", stats.BySource)
	}

	// 5. AdminList with query filter
	res, err := st.AdminList(ctx, AdminListFilter{
		Query: "Blinding",
		Page:  1,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("AdminList failed: %v", err)
	}
	if res.Total != 1 || len(res.Items) != 1 {
		t.Fatalf("expected 1 result for query 'Blinding', got total=%d items=%d", res.Total, len(res.Items))
	}
	if res.Items[0].Title != "Blinding Lights" {
		t.Fatalf("expected 'Blinding Lights', got %q", res.Items[0].Title)
	}

	// 6. AdminGetByID
	fetched, err := st.AdminGetByID(ctx, row1.ID)
	if err != nil {
		t.Fatalf("AdminGetByID failed: %v", err)
	}
	if fetched.Title != "Blinding Lights" || string(fetched.ContentJSON) != string(row1.ContentJSON) {
		t.Fatalf("unexpected fetched row: %+v, content: %s", fetched, string(fetched.ContentJSON))
	}

	// 7. AdminUpdate
	newTitle := "Blinding Lights (Remix)"
	updated, err := st.AdminUpdate(ctx, row1.ID, AdminUpdateRequest{
		Title: &newTitle,
	})
	if err != nil {
		t.Fatalf("AdminUpdate failed: %v", err)
	}
	if updated.Title != newTitle {
		t.Fatalf("expected updated title %q, got %q", newTitle, updated.Title)
	}

	// 8. AdminDelete
	if err := st.AdminDelete(ctx, row1.ID); err != nil {
		t.Fatalf("AdminDelete failed: %v", err)
	}

	// 9. AdminDeleteBatch
	deletedCount, err := st.AdminDeleteBatch(ctx, []int64{row2.ID})
	if err != nil {
		t.Fatalf("AdminDeleteBatch failed: %v", err)
	}
	if deletedCount != 1 {
		t.Fatalf("expected 1 deleted, got %d", deletedCount)
	}

	stats, _ = st.AdminStats(ctx)
	if stats.TotalLyrics != 0 {
		t.Fatalf("expected 0 lyrics after deletes, got %d", stats.TotalLyrics)
	}
}
