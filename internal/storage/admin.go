package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AdminStats struct {
	TotalLyrics       int64            `json:"total_lyrics"`
	BySource          map[string]int64 `json:"by_source"`
	DatabaseSizeBytes int64            `json:"database_size_bytes"`
	LastCreatedAt     *time.Time       `json:"last_created_at,omitempty"`
	IsResting         bool             `json:"is_resting"`
}

type AdminListFilter struct {
	Query      string `json:"query"`
	Source     string `json:"source"`
	ISRC       string `json:"isrc"`
	PlatformID string `json:"platform_id"`
	Page       int    `json:"page"`
	Limit      int    `json:"limit"`
	SortBy     string `json:"sort_by"`
	SortDir    string `json:"sort_dir"`
}

type AdminListResult struct {
	Items      []*Row `json:"items"`
	Total      int64  `json:"total"`
	Page       int    `json:"page"`
	Limit      int    `json:"limit"`
	TotalPages int    `json:"total_pages"`
}

type AdminUpdateRequest struct {
	Title       *string `json:"title,omitempty"`
	Artist      *string `json:"artist,omitempty"`
	ISRC        *string `json:"isrc,omitempty"`
	PlatformID  *string `json:"platform_id,omitempty"`
	Source      *string `json:"source,omitempty"`
	DurationMS  *int    `json:"duration_ms,omitempty"`
	ContentJSON []byte  `json:"content,omitempty"`
}

func (s *Store) AdminStats(ctx context.Context) (*AdminStats, error) {
	if s.IsResting() {
		return &AdminStats{IsResting: true}, nil
	}

	stats := &AdminStats{
		BySource:  make(map[string]int64),
		IsResting: false,
	}

	// 1. Total count
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM lyrics`).Scan(&stats.TotalLyrics); err != nil {
		s.recordQueryErr(ctx, err)
		return nil, fmt.Errorf("admin stats total count: %w", err)
	}

	// 2. Count by source
	rows, err := s.db.QueryContext(ctx, `SELECT source, count(*) FROM lyrics GROUP BY source`)
	if err != nil {
		s.recordQueryErr(ctx, err)
		return nil, fmt.Errorf("admin stats by source: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var src string
		var count int64
		if err := rows.Scan(&src, &count); err != nil {
			return nil, err
		}
		if src == "" {
			src = "unknown"
		}
		stats.BySource[src] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 3. Last created timestamp
	var lastMs sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(created_at) FROM lyrics`).Scan(&lastMs); err == nil && lastMs.Valid {
		t := time.UnixMilli(lastMs.Int64)
		stats.LastCreatedAt = &t
	}

	// 4. File size on disk
	dbPath := s.cfg.DBPath
	if dbPath == "" {
		dbPath = filepath.Join("database", "lyrics_cache.db")
	}
	if fi, err := os.Stat(dbPath); err == nil {
		stats.DatabaseSizeBytes = fi.Size()
	} else {
		// Fallback to SQLite PRAGMA
		var pageCount, pageSize int64
		if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err == nil {
			if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err == nil {
				stats.DatabaseSizeBytes = pageCount * pageSize
			}
		}
	}

	s.RecordDBSuccess()
	return stats, nil
}

func (s *Store) AdminList(ctx context.Context, filter AdminListFilter) (*AdminListResult, error) {
	if s.IsResting() {
		return nil, fmt.Errorf("storage: database resting")
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}
	offset := (page - 1) * limit

	var whereClauses []string
	var args []any

	if filter.Source != "" {
		whereClauses = append(whereClauses, "LOWER(source) = LOWER(?)")
		args = append(args, strings.TrimSpace(filter.Source))
	}
	if filter.ISRC != "" {
		whereClauses = append(whereClauses, "LOWER(isrc) = LOWER(?)")
		args = append(args, strings.TrimSpace(filter.ISRC))
	}
	if filter.PlatformID != "" {
		whereClauses = append(whereClauses, "LOWER(platform_id) = LOWER(?)")
		args = append(args, strings.TrimSpace(filter.PlatformID))
	}
	if filter.Query != "" {
		q := "%" + strings.TrimSpace(strings.ToLower(filter.Query)) + "%"
		rawQ := strings.TrimSpace(filter.Query)
		whereClauses = append(whereClauses, "(LOWER(title) LIKE ? OR LOWER(artist) LIKE ? OR isrc = ? OR platform_id = ?)")
		args = append(args, q, q, rawQ, rawQ)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = " WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Count total
	countSQL := "SELECT count(*) FROM lyrics" + whereSQL
	var total int64
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		s.recordQueryErr(ctx, err)
		return nil, fmt.Errorf("admin list count: %w", err)
	}

	// Validate sorting
	sortCol := "id"
	switch strings.ToLower(filter.SortBy) {
	case "title":
		sortCol = "title COLLATE NOCASE"
	case "artist":
		sortCol = "artist COLLATE NOCASE"
	case "source":
		sortCol = "source"
	case "duration_ms", "duration":
		sortCol = "duration_ms"
	case "created_at":
		sortCol = "created_at"
	case "id":
		sortCol = "id"
	}

	sortDir := "DESC"
	if strings.EqualFold(filter.SortDir, "asc") {
		sortDir = "ASC"
	}

	querySQL := fmt.Sprintf(
		"SELECT id, filename, isrc, platform_id, source, title, artist, duration_ms, created_at FROM lyrics%s ORDER BY %s %s LIMIT ? OFFSET ?",
		whereSQL, sortCol, sortDir,
	)
	queryArgs := append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, querySQL, queryArgs...)
	if err != nil {
		s.recordQueryErr(ctx, err)
		return nil, fmt.Errorf("admin list query: %w", err)
	}
	defer rows.Close()

	items := make([]*Row, 0, limit)
	for rows.Next() {
		var row Row
		var isrc, platformID, source, title, artist sql.NullString
		var createdAt int64
		if err := rows.Scan(
			&row.ID,
			&row.Filename,
			&isrc,
			&platformID,
			&source,
			&title,
			&artist,
			&row.DurationMS,
			&createdAt,
		); err != nil {
			return nil, err
		}
		if isrc.Valid {
			row.ISRC = isrc.String
		}
		if platformID.Valid {
			row.PlatformID = platformID.String
		}
		if source.Valid {
			row.Source = source.String
		}
		if title.Valid {
			row.Title = title.String
		}
		if artist.Valid {
			row.Artist = artist.String
		}
		row.CreatedAt = time.UnixMilli(createdAt)
		items = append(items, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.RecordDBSuccess()

	totalPages := int((total + int64(limit) - 1) / int64(limit))
	if totalPages == 0 {
		totalPages = 1
	}

	return &AdminListResult{
		Items:      items,
		Total:      total,
		Page:       page,
		Limit:      limit,
		TotalPages: totalPages,
	}, nil
}

func (s *Store) AdminGetByID(ctx context.Context, id int64) (*Row, error) {
	if s.IsResting() {
		return nil, fmt.Errorf("storage: database resting")
	}

	const q = `SELECT id, filename, content, isrc, platform_id, source, title, artist, duration_ms, created_at FROM lyrics WHERE id = ?`
	var row Row
	var raw []byte
	var isrc, platformID, source, title, artist sql.NullString
	var createdAt int64

	if err := s.db.QueryRowContext(ctx, q, id).Scan(
		&row.ID,
		&row.Filename,
		&raw,
		&isrc,
		&platformID,
		&source,
		&title,
		&artist,
		&row.DurationMS,
		&createdAt,
	); err != nil {
		s.recordQueryErr(ctx, err)
		return nil, err
	}
	s.RecordDBSuccess()

	if isrc.Valid {
		row.ISRC = isrc.String
	}
	if platformID.Valid {
		row.PlatformID = platformID.String
	}
	if source.Valid {
		row.Source = source.String
	}
	if title.Valid {
		row.Title = title.String
	}
	if artist.Valid {
		row.Artist = artist.String
	}
	row.CreatedAt = time.UnixMilli(createdAt)
	row.ContentJSON = DecompressContent(raw)

	return &row, nil
}

func (s *Store) AdminCreate(ctx context.Context, row *Row) error {
	if row == nil {
		return fmt.Errorf("row is nil")
	}
	if strings.TrimSpace(row.Title) == "" && strings.TrimSpace(row.Artist) == "" && row.ISRC == "" && row.PlatformID == "" {
		return fmt.Errorf("title/artist or isrc/platform_id is required")
	}
	if row.Source == "" {
		row.Source = "lyricsplus"
	}
	if row.Filename == "" {
		row.Filename = CanonicalFilename(row.Artist, row.Title, "", row.DurationMS, row.ISRC, row.PlatformID, "json")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	return s.SaveLyrics(ctx, row)
}

func (s *Store) AdminCreateBatch(ctx context.Context, rows []*Row) (int64, error) {
	return s.SaveLyricsBatch(ctx, rows)
}

func (s *Store) AdminUpdate(ctx context.Context, id int64, req AdminUpdateRequest) (*Row, error) {
	if s.IsResting() {
		return nil, fmt.Errorf("storage: database resting")
	}

	curr, err := s.AdminGetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Invalidate caches before changing keys
	s.invalidate(curr, id)

	if req.Title != nil {
		curr.Title = strings.TrimSpace(*req.Title)
	}
	if req.Artist != nil {
		curr.Artist = strings.TrimSpace(*req.Artist)
	}
	if req.ISRC != nil {
		curr.ISRC = strings.TrimSpace(*req.ISRC)
	}
	if req.PlatformID != nil {
		curr.PlatformID = strings.TrimSpace(*req.PlatformID)
	}
	if req.Source != nil && strings.TrimSpace(*req.Source) != "" {
		curr.Source = strings.TrimSpace(*req.Source)
	}
	if req.DurationMS != nil {
		curr.DurationMS = *req.DurationMS
	}
	if len(req.ContentJSON) > 0 {
		curr.ContentJSON = req.ContentJSON
	}

	curr.Filename = CanonicalFilename(curr.Artist, curr.Title, "", curr.DurationMS, curr.ISRC, curr.PlatformID, "json")
	compressed := CompressContent(curr.ContentJSON)

	const updateSQL = `UPDATE lyrics SET
		filename = ?,
		content = ?,
		isrc = ?,
		platform_id = ?,
		source = ?,
		title = ?,
		artist = ?,
		duration_ms = ?
		WHERE id = ?`

	if _, err := s.wdb.ExecContext(ctx, updateSQL,
		curr.Filename, compressed, curr.ISRC, curr.PlatformID, curr.Source,
		curr.Title, curr.Artist, curr.DurationMS, id,
	); err != nil {
		s.recordQueryErr(ctx, err)
		return nil, fmt.Errorf("admin update lyrics: %w", err)
	}

	s.RecordDBSuccess()
	s.invalidate(curr, id)

	return curr, nil
}

func (s *Store) AdminDelete(ctx context.Context, id int64) error {
	if s.IsResting() {
		return fmt.Errorf("storage: database resting")
	}

	curr, err := s.AdminGetByID(ctx, id)
	if err != nil {
		return err
	}

	s.invalidate(curr, id)

	const deleteSQL = `DELETE FROM lyrics WHERE id = ?`
	if _, err := s.wdb.ExecContext(ctx, deleteSQL, id); err != nil {
		s.recordQueryErr(ctx, err)
		return fmt.Errorf("admin delete lyrics: %w", err)
	}

	s.RecordDBSuccess()
	return nil
}

func (s *Store) AdminDeleteBatch(ctx context.Context, ids []int64) (int64, error) {
	if s.IsResting() {
		return 0, fmt.Errorf("storage: database resting")
	}
	if len(ids) == 0 {
		return 0, nil
	}

	var deleted int64
	for _, id := range ids {
		if err := s.AdminDelete(ctx, id); err == nil {
			deleted++
		}
	}
	return deleted, nil
}
