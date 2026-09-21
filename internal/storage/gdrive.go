package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/similarity"
)

var (
	ErrGDriveDisabled     = errors.New("google drive storage is disabled")
	ErrGDriveNotConfig    = errors.New("google drive credentials not configured")
	ErrCircuitBreakerOpen = errors.New("google drive circuit breaker is open")
)

// FileItem represents a Google Drive file descriptor.
type FileItem struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	MimeType     string    `json:"mimeType"`
	CreatedTime  time.Time `json:"createdTime,omitempty"`
	ModifiedTime time.Time `json:"modifiedTime,omitempty"`
}

// FileListResponse is the paginated response from Google Drive files.list.
type FileListResponse struct {
	Files         []FileItem `json:"files"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
}

type tokenEntry struct {
	accessToken string
	expiresAt   time.Time
}

// GDriveClient provides resilient, multi-account Google Drive file operations.
type GDriveClient struct {
	cfg        config.GDrive
	accounts   []config.GDriveAccount
	accountIdx int
	tokens     map[int]*tokenEntry
	mu         sync.RWMutex

	circuitOpen       bool
	circuitOpenUntil  time.Time
	consecutiveErrors int

	httpClient  *http.Client
	fileCache   *lru.Cache[string, []byte]
	searchCache *lru.Cache[string, []FileItem]
}

// NewGDriveClient constructs a Google Drive client.
func NewGDriveClient(cfg config.GDrive, httpClient *http.Client) *GDriveClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	fc, _ := lru.New[string, []byte](1000)
	sc, _ := lru.New[string, []FileItem](1000)

	return &GDriveClient{
		cfg:         cfg,
		accounts:    cfg.Accounts,
		tokens:      make(map[int]*tokenEntry),
		httpClient:  httpClient,
		fileCache:   fc,
		searchCache: sc,
	}
}

// IsConfigured checks if credentials exist.
func (g *GDriveClient) IsConfigured() bool {
	if !g.cfg.Enabled || len(g.accounts) == 0 {
		return false
	}
	acc := g.accounts[0]
	return acc.ClientID != "" && acc.RefreshToken != ""
}

// Config exposes the underlying GDrive config.
func (g *GDriveClient) Config() config.GDrive {
	return g.cfg
}

func (g *GDriveClient) isCircuitOpen() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.consecutiveErrors < 0 || !g.circuitOpen {
		return false
	}
	return time.Now().Before(g.circuitOpenUntil)
}

func (g *GDriveClient) recordRateLimitError() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.consecutiveErrors < 0 {
		return
	}
	g.consecutiveErrors++
	if g.consecutiveErrors >= 5 {
		g.circuitOpen = true
		g.circuitOpenUntil = time.Now().Add(60 * time.Second)
		g.consecutiveErrors = 0
	}
}

// ResetCircuitBreaker clears any tripped circuit breaker state.
func (g *GDriveClient) ResetCircuitBreaker() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.circuitOpen = false
	g.consecutiveErrors = 0
}

// CircuitBreakerRemaining returns remaining duration if breaker is open, or 0.
func (g *GDriveClient) CircuitBreakerRemaining() time.Duration {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.circuitOpen {
		return 0
	}
	rem := time.Until(g.circuitOpenUntil)
	if rem <= 0 {
		return 0
	}
	return rem
}

// DisableCircuitBreaker disables the circuit breaker mechanism.
func (g *GDriveClient) DisableCircuitBreaker() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.circuitOpen = false
	g.consecutiveErrors = -1
}

func (g *GDriveClient) recordSuccess() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.consecutiveErrors > 0 {
		g.consecutiveErrors--
	}
}

func (g *GDriveClient) currentAccount() config.GDriveAccount {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.accounts) == 0 {
		return config.GDriveAccount{}
	}
	return g.accounts[g.accountIdx%len(g.accounts)]
}

func (g *GDriveClient) rotateAccount() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.accounts) > 1 {
		g.accountIdx = (g.accountIdx + 1) % len(g.accounts)
	}
}

// Authenticate returns a valid Google OAuth2 access token, refreshing if needed.
func (g *GDriveClient) Authenticate(ctx context.Context, forceRefresh bool) (string, error) {
	if g.isCircuitOpen() {
		return "", ErrCircuitBreakerOpen
	}
	if !g.IsConfigured() {
		return "", ErrGDriveNotConfig
	}

	g.mu.RLock()
	idx := g.accountIdx % len(g.accounts)
	cached := g.tokens[idx]
	g.mu.RUnlock()

	if !forceRefresh && cached != nil && time.Now().Add(60*time.Second).Before(cached.expiresAt) {
		return cached.accessToken, nil
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Double check after lock
	idx = g.accountIdx % len(g.accounts)
	cached = g.tokens[idx]
	if !forceRefresh && cached != nil && time.Now().Add(60*time.Second).Before(cached.expiresAt) {
		return cached.accessToken, nil
	}

	acc := g.accounts[idx]
	vals := url.Values{}
	vals.Set("client_id", acc.ClientID)
	vals.Set("client_secret", acc.ClientSecret)
	vals.Set("refresh_token", acc.RefreshToken)
	vals.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(vals.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gdrive token refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gdrive token refresh error (%d): %s", resp.StatusCode, string(body))
	}

	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	expiresIn := res.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	g.tokens[idx] = &tokenEntry{
		accessToken: res.AccessToken,
		expiresAt:   time.Now().Add(time.Duration(expiresIn) * time.Second),
	}

	return res.AccessToken, nil
}

func escapeQueryLiteral(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	return s
}

// SearchFiles queries Google Drive files.list.
func (g *GDriveClient) SearchFiles(ctx context.Context, folderIDs []string, query string, pageSize int, pageToken string) (*FileListResponse, error) {
	if g.isCircuitOpen() {
		return nil, ErrCircuitBreakerOpen
	}
	tok, err := g.Authenticate(ctx, false)
	if err != nil {
		return nil, err
	}

	var parentsQuery string
	if len(folderIDs) > 0 {
		var parts []string
		for _, f := range folderIDs {
			if strings.TrimSpace(f) != "" {
				parts = append(parts, fmt.Sprintf("'%s' in parents", escapeQueryLiteral(strings.TrimSpace(f))))
			}
		}
		if len(parts) > 0 {
			parentsQuery = "(" + strings.Join(parts, " or ") + ")"
		}
	}

	fullQuery := query
	if parentsQuery != "" {
		if fullQuery != "" {
			fullQuery = fullQuery + " and " + parentsQuery
		} else {
			fullQuery = parentsQuery
		}
	}
	if fullQuery != "" {
		fullQuery = fullQuery + " and trashed = false"
	} else {
		fullQuery = "trashed = false"
	}

	vals := url.Values{}
	vals.Set("q", fullQuery)
	vals.Set("fields", "nextPageToken,files(id,name,mimeType,createdTime,modifiedTime)")
	if pageSize > 0 {
		vals.Set("pageSize", strconv.Itoa(pageSize))
	} else {
		vals.Set("pageSize", "100")
	}
	if pageToken != "" {
		vals.Set("pageToken", pageToken)
	}

	reqURL := "https://www.googleapis.com/drive/v3/files?" + vals.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		g.recordRateLimitError()
		g.rotateAccount()
		return nil, fmt.Errorf("gdrive rate limited (%d)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gdrive search error (%d): %s", resp.StatusCode, string(body))
	}

	g.recordSuccess()

	var res FileListResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FetchFile downloads the file content from Google Drive (alt=media).
func (g *GDriveClient) FetchFile(ctx context.Context, fileID string) ([]byte, error) {
	if g.isCircuitOpen() {
		return nil, ErrCircuitBreakerOpen
	}
	if val, ok := g.fileCache.Get(fileID); ok {
		return val, nil
	}

	tok, err := g.Authenticate(ctx, false)
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?alt=media", fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		g.recordRateLimitError()
		g.rotateAccount()
		return nil, fmt.Errorf("gdrive rate limited (%d)", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gdrive fetch error (%d): %s", resp.StatusCode, string(body))
	}

	g.recordSuccess()
	g.fileCache.Add(fileID, body)
	return body, nil
}

// UploadFile uploads a file with multi-folder quota overflow fallback.
func (g *GDriveClient) UploadFile(ctx context.Context, folderIDs []string, fileName, mimeType string, data []byte) (*FileItem, error) {
	if g.isCircuitOpen() {
		return nil, ErrCircuitBreakerOpen
	}

	folders := folderIDs
	if len(folders) == 0 {
		folders = []string{""}
	}

	var lastErr error
	for _, folderID := range folders {
		res, err := g.doUploadMultipart(ctx, folderID, fileName, mimeType, data)
		if err == nil && res != nil {
			return res, nil
		}
		lastErr = err
		// If quota or permission error on folder, continue to next folder in overflow list
		if isQuotaOrFolderFull(err) {
			continue
		}
		break
	}
	return nil, lastErr
}

func isQuotaOrFolderFull(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "storagequotaexceeded") ||
		strings.Contains(s, "teamdrivefilelimitexceeded") ||
		strings.Contains(s, "folderitemlimitexceeded") ||
		strings.Contains(s, "numchildrenexceeded") ||
		strings.Contains(s, "quota")
}

func (g *GDriveClient) doUploadMultipart(ctx context.Context, folderID, fileName, mimeType string, data []byte) (*FileItem, error) {
	tok, err := g.Authenticate(ctx, false)
	if err != nil {
		return nil, err
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Part 1: metadata
	metaHeader := make(textproto.MIMEHeader)
	metaHeader.Set("Content-Type", "application/json; charset=UTF-8")
	metaPart, err := writer.CreatePart(metaHeader)
	if err != nil {
		return nil, err
	}

	metaObj := map[string]interface{}{
		"name":     fileName,
		"mimeType": mimeType,
	}
	if strings.TrimSpace(folderID) != "" {
		metaObj["parents"] = []string{strings.TrimSpace(folderID)}
	}
	metaBytes, _ := json.Marshal(metaObj)
	_, _ = metaPart.Write(metaBytes)

	// Part 2: file content
	mediaHeader := make(textproto.MIMEHeader)
	mediaHeader.Set("Content-Type", mimeType)
	mediaPart, err := writer.CreatePart(mediaHeader)
	if err != nil {
		return nil, err
	}
	_, _ = mediaPart.Write(data)
	_ = writer.Close()

	uploadURL := "https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		g.recordRateLimitError()
		g.rotateAccount()
		return nil, fmt.Errorf("gdrive upload rate limited (%d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("gdrive upload error (%d): %s", resp.StatusCode, string(respBytes))
	}

	g.recordSuccess()

	var item FileItem
	if err := json.Unmarshal(respBytes, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// FindExactMatchByIds searches GDrive for a file matching the song's ISRC or Platform ID.
func (g *GDriveClient) FindExactMatchByIds(ctx context.Context, folderIDs []string, isrc, platformID, mimeType string) (*FileItem, error) {
	if isrc == "" && platformID == "" {
		return nil, nil
	}
	searchTerm := isrc
	if searchTerm == "" {
		searchTerm = platformID
	}

	var qParts []string
	if mimeType != "" {
		qParts = append(qParts, fmt.Sprintf("mimeType = '%s'", escapeQueryLiteral(mimeType)))
	}
	qParts = append(qParts, fmt.Sprintf("name contains '%s'", escapeQueryLiteral(searchTerm)))
	query := strings.Join(qParts, " and ")

	cacheKey := fmt.Sprintf("exact:%s:%s", searchTerm, mimeType)
	if cached, ok := g.searchCache.Get(cacheKey); ok && len(cached) > 0 {
		return &cached[0], nil
	}

	res, err := g.SearchFiles(ctx, folderIDs, query, 10, "")
	if err != nil || res == nil || len(res.Files) == 0 {
		return nil, err
	}

	for _, f := range res.Files {
		parsed := ParseFilename(f.Name)
		if isrc != "" && strings.EqualFold(parsed.ISRC, isrc) {
			g.searchCache.Add(cacheKey, []FileItem{f})
			return &f, nil
		}
		if platformID != "" && strings.EqualFold(parsed.PlatformID, platformID) {
			g.searchCache.Add(cacheKey, []FileItem{f})
			return &f, nil
		}
	}
	return nil, nil
}

// FindExistingFile searches GDrive for a file matching metadata keywords using similarity scoring.
func (g *GDriveClient) FindExistingFile(ctx context.Context, folderIDs []string, title, artist, album string, durationSec float64, isrc, platformID, mimeType string) (*FileItem, error) {
	if title == "" || artist == "" {
		return nil, nil
	}

	keywords := append(ExtractKeywords(title), ExtractKeywords(artist)...)
	if len(keywords) == 0 {
		return g.FindExactMatchByIds(ctx, folderIDs, isrc, platformID, mimeType)
	}

	var qParts []string
	if mimeType != "" {
		qParts = append(qParts, fmt.Sprintf("mimeType = '%s'", escapeQueryLiteral(mimeType)))
	}
	for _, kw := range keywords {
		qParts = append(qParts, fmt.Sprintf("name contains '%s'", escapeQueryLiteral(kw)))
	}
	query := strings.Join(qParts, " and ")

	res, err := g.SearchFiles(ctx, folderIDs, query, 15, "")
	if err != nil || res == nil || len(res.Files) == 0 {
		return nil, err
	}

	candidates := make([]similarity.SongCandidate, len(res.Files))
	for i, f := range res.Files {
		parsed := ParseFilename(f.Name)
		candidates[i] = similarity.SongCandidate{
			Title:      parsed.Title,
			Artist:     parsed.Artist,
			Album:      parsed.Album,
			DurationMs: parsed.DurationMS,
			ISRC:       parsed.ISRC,
			PlatformID: parsed.PlatformID,
			Data:       i,
		}
	}

	best := similarity.FindBestSongMatch(candidates, title, artist, album, durationSec, isrc, platformID)
	if best == nil {
		return nil, nil
	}
	idx := best.Candidate.Data.(int)
	return &res.Files[idx], nil
}

// UploadUserLyrics uploads user-submitted lyrics into the USERTML_JSON folder.
func (g *GDriveClient) UploadUserLyrics(ctx context.Context, fileName string, data []byte) (*FileItem, error) {
	folders := g.cfg.FolderUserTML
	return g.UploadFile(ctx, folders, fileName, "application/json", data)
}

// UploadBackupFile uploads a database backup file to the backup folder.
func (g *GDriveClient) UploadBackupFile(ctx context.Context, fileName string, data []byte) (*FileItem, error) {
	folders := g.cfg.FolderBackup
	if len(folders) == 0 {
		folders = g.cfg.FolderUserTML
	}
	return g.UploadFile(ctx, folders, fileName, "application/octet-stream", data)
}
