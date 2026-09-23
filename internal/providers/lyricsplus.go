package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
	"lyricsplus/backend/internal/parsers"
	"lyricsplus/backend/internal/proxy"
	"lyricsplus/backend/internal/similarity"
	"lyricsplus/backend/internal/storage"
)

const lyricsplusName = "lyricsplus"

type qapleEngine interface {
	FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error)
}

// LyricsPlusProvider serves user-generated lyrics from the UGC store and falls back to Qaple.
type LyricsPlusProvider struct {
	client     *proxy.Client
	jwtSecret  string
	difficulty int
	ttl        time.Duration
	store      *storage.Store
	gdrive     *storage.GDriveClient
	qaple      qapleEngine
	logger     *logger.Logger
	mu         sync.RWMutex
}

func NewLyricsPlus(client *proxy.Client) *LyricsPlusProvider {
	return &LyricsPlusProvider{
		client:     client,
		jwtSecret:  jwtSecretFromEnv(),
		difficulty: 5,
		ttl:        10 * time.Minute,
	}
}

// SetLogger attaches a logger for debug output.
func (p *LyricsPlusProvider) SetLogger(lg *logger.Logger) { p.logger = lg }

func (p *LyricsPlusProvider) debugf(format string, args ...any) {
	if p.logger == nil {
		return
	}
	p.logger.Debugf("lyricsplus: "+format, args...)
}

func (p *LyricsPlusProvider) Name() string     { return lyricsplusName }
func (p *LyricsPlusProvider) Configured() bool { return true }

func (p *LyricsPlusProvider) SetStore(s *storage.Store) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

func (p *LyricsPlusProvider) SetGDrive(g *storage.GDriveClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gdrive = g
}

func (p *LyricsPlusProvider) SetQaple(q qapleEngine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.qaple = q
}

func (p *LyricsPlusProvider) FetchLyrics(ctx context.Context, q domain.SearchQuery) (*domain.LyricsResponse, error) {
	p.mu.RLock()
	st := p.store
	gd := p.gdrive
	qp := p.qaple
	p.mu.RUnlock()

	var lpResult *domain.LyricsResponse

	if !q.ForceReload && st != nil {
		var row *storage.Row
		if q.ISRC != "" || q.PlatformID != "" {
			if r, ok := st.GetExactUser(ctx, q.ISRC, q.PlatformID); ok && r != nil {
				row = r
			}
		}
		if row == nil && (q.Title != "" && q.Artist != "") {
			if rows, ok := st.GetByTitleArtist(ctx, q.Title, q.Artist); ok && len(rows) > 0 {
				row = matchBestUserRow(rows, q)
			}
		}
		if row == nil && (q.Title != "" || q.Artist != "") {
			if rows, ok := st.GetByFTS5(ctx, q.Title, q.Artist); ok && len(rows) > 0 {
				row = matchBestUserRow(rows, q)
			}
		}

		if row != nil && row.Source == "lyricsplus" {
			if len(row.ContentJSON) == 0 {
				content, err := st.GetContent(ctx, row.ID)
				if err == nil {
					row.ContentJSON = content
				}
			}
			if len(row.ContentJSON) > 0 {
				var resp domain.LyricsResponse
				if json.Unmarshal(row.ContentJSON, &resp) == nil && len(resp.Lyrics) > 0 {
					norm := parsers.NormalizeV2(&resp)
					norm.Metadata.Source = "Lyrics+"
					norm.Cached = domain.CacheUserJSON
					norm.RawData = string(row.ContentJSON)

					var pf storage.ParsedFilename
					if row.Filename != "" {
						pf = storage.ParseFilename(row.Filename)
					}
					title := row.Title
					if title == "" && pf.Title != "" {
						title = pf.Title
					}
					if title == "" {
						title = q.Title
					}
					artist := row.Artist
					if artist == "" && pf.Artist != "" {
						artist = pf.Artist
					}
					if artist == "" {
						artist = q.Artist
					}
					album := pf.Album
					if album == "" {
						album = q.Album
					}
					isrc := row.ISRC
					if isrc == "" && pf.ISRC != "" {
						isrc = pf.ISRC
					}
					if isrc == "" {
						isrc = q.ISRC
					}
					platID := row.PlatformID
					if platID == "" && pf.PlatformID != "" {
						platID = pf.PlatformID
					}
					if platID == "" {
						platID = q.PlatformID
					}
					var durSec *float64
					durMs := row.DurationMS
					if durMs <= 0 && pf.DurationMS > 0 {
						durMs = pf.DurationMS
					}
					if durMs <= 0 && q.Duration > 0 {
						durMs = q.Duration
					}
					if durMs > 0 {
						s := float64(durMs) / 1000.0
						durSec = &s
					}
					norm.ProcessingTime = &domain.ProcessTiming{
						SelectedSongMetadata: &domain.PickedSongMetadata{
							Source:         "Lyrics+",
							Title:          title,
							Artist:         artist,
							Album:          album,
							Duration:       durSec,
							SongISRC:       isrc,
							SongPlatformID: platID,
						},
					}

					lpResult = norm
					p.debugf("hit user store row=%d lines=%d word=%t", row.ID, len(norm.Lyrics), hasWordSync(norm))
					if hasWordSync(norm) {
						return norm, nil
					}
				}
			}
		}
	}

	// Check Google Drive USERTML_JSON if not found or word sync needed
	if !q.ForceReload && (lpResult == nil || !hasWordSync(lpResult)) && gd != nil && gd.IsConfigured() {
		folders := gd.Config().FolderUserTML
		var gfile *storage.FileItem
		if q.ISRC != "" || q.PlatformID != "" {
			gfile, _ = gd.FindExactMatchByIds(ctx, folders, q.ISRC, q.PlatformID, "application/json")
		}
		if gfile == nil && q.Title != "" && q.Artist != "" {
			durationSec := float64(q.Duration) / 1000.0
			gfile, _ = gd.FindExistingFile(ctx, folders, q.Title, q.Artist, q.Album, durationSec, q.ISRC, q.PlatformID, "application/json")
		}
		if gfile != nil {
			content, err := gd.FetchFile(ctx, gfile.ID)
			if err == nil && len(content) > 0 {
				var resp domain.LyricsResponse
				if json.Unmarshal(content, &resp) == nil && len(resp.Lyrics) > 0 {
					norm := parsers.NormalizeV2(&resp)
					norm.Metadata.Source = "Lyrics+"
					norm.Cached = domain.CacheGDrive
					norm.RawData = string(content)

					var pf storage.ParsedFilename
					if gfile.Name != "" {
						pf = storage.ParseFilename(gfile.Name)
					}
					title := pf.Title
					if title == "" {
						title = q.Title
					}
					artist := pf.Artist
					if artist == "" {
						artist = q.Artist
					}
					album := pf.Album
					if album == "" {
						album = q.Album
					}
					isrc := pf.ISRC
					if isrc == "" {
						isrc = q.ISRC
					}
					platID := pf.PlatformID
					if platID == "" {
						platID = q.PlatformID
					}
					var durSec *float64
					durMs := pf.DurationMS
					if durMs <= 0 && q.Duration > 0 {
						durMs = q.Duration
					}
					if durMs > 0 {
						s := float64(durMs) / 1000.0
						durSec = &s
					}
					norm.ProcessingTime = &domain.ProcessTiming{
						SelectedSongMetadata: &domain.PickedSongMetadata{
							Source:         "Lyrics+",
							Title:          title,
							Artist:         artist,
							Album:          album,
							Duration:       durSec,
							SongISRC:       isrc,
							SongPlatformID: platID,
						},
					}

					lpResult = norm
					if st != nil {
						go func(c []byte, query domain.SearchQuery) {
							_ = st.SaveUserLyrics(context.Background(), query, c)
						}(content, q)
					}
					if hasWordSync(norm) {
						return norm, nil
					}
				}
			}
		}
	}

	// Attempt Qaple as part of LyricsPlus fallback
	if qp != nil {
		qapleResult, err := qp.FetchLyrics(ctx, q)
		if err == nil && qapleResult != nil && len(qapleResult.Lyrics) > 0 {
			p.debugf("qaple result lines=%d priority=%d", len(qapleResult.Lyrics), getSyncPriority(qapleResult))
			if lpResult == nil || getSyncPriority(qapleResult) > getSyncPriority(lpResult) {
				return qapleResult, nil
			}
		} else if err != nil {
			p.debugf("qaple failed (err=%v)", err)
		}
	}

	p.debugf("returning lpResult lines=%d", func() int {
		if lpResult == nil {
			return 0
		}
		return len(lpResult.Lyrics)
	}())
	return lpResult, nil
}

func hasWordSync(resp *domain.LyricsResponse) bool {
	if resp == nil {
		return false
	}
	if resp.Type == domain.SyncTypeWord || resp.Type == domain.SyncTypeSyllable {
		return true
	}
	for _, l := range resp.Lyrics {
		if len(l.Syllabus) > 0 {
			return true
		}
	}
	return false
}

func getSyncPriority(resp *domain.LyricsResponse) int {
	if resp == nil || len(resp.Lyrics) == 0 {
		return 0
	}
	if hasWordSync(resp) {
		return 3
	}
	if resp.Type == domain.SyncTypeLine {
		return 2
	}
	return 1
}

// IsVandalismUpdate checks whether an update constitutes vandalism.
func IsVandalismUpdate(prev, next *domain.LyricsResponse) bool {
	if next == nil || len(next.Lyrics) == 0 {
		return true
	}
	for _, line := range next.Lyrics {
		if line.Time < 0 || line.Duration < 0 {
			return true
		}
	}

	textLen := func(r *domain.LyricsResponse) int {
		if r == nil {
			return 0
		}
		total := 0
		for _, l := range r.Lyrics {
			total += len(strings.TrimSpace(l.Text))
		}
		return total
	}

	nextLen := textLen(next)
	if nextLen == 0 {
		return true
	}
	if prev != nil {
		prevLen := textLen(prev)
		if prevLen >= 100 && float64(nextLen) < float64(prevLen)*0.2 {
			return true
		}
	}
	return false
}

// SetChallenge configures the PoW parameters (used when synthesized).
func (p *LyricsPlusProvider) SetChallenge(secret string, difficulty int, ttl time.Duration) {
	if secret != "" {
		p.jwtSecret = secret
	}
	if difficulty > 0 {
		p.difficulty = difficulty
	}
	if ttl > 0 {
		p.ttl = ttl
	}
}

// challengeClaim is the issued PoW challenge JWT payload.
type challengeClaim struct {
	Challenge string `json:"challenge"`
	jwt.RegisteredClaims
}

// Issuer issues a challenge JWT with a UUID challenge.
type Issuer struct {
	secret string
	ttl    time.Duration
}

func NewIssuer(secret string, ttl time.Duration) *Issuer {
	return &Issuer{secret: secret, ttl: ttl}
}

// Issue returns a signed challenge token.
func (i *Issuer) Issue(now time.Time) (string, string, error) {
	challenge := uuid.NewString()
	claims := challengeClaim{
		Challenge: challenge,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(i.secret))
	return tok, challenge, err
}

// Verifier validates the PoW nonce against a challenge token.
type Verifier struct {
	secret     string
	difficulty int
}

func NewVerifier(secret string, difficulty int) *Verifier {
	return &Verifier{secret: secret, difficulty: difficulty}
}

// Difficulty exposes the required leading-zero count.
func (v *Verifier) Difficulty() int {
	return v.difficulty
}

// Verify checks JWT signature/expiry and the SHA-256(prefix) nonce.
func (v *Verifier) Verify(token, nonce string) (string, error) {
	claims := &challengeClaim{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(v.secret), nil
	})
	if err != nil || !parsed.Valid {
		return "", errors.New("invalid challenge token")
	}
	if claims.ExpiresAt != nil && time.Now().After(claims.ExpiresAt.Time) {
		return "", errors.New("challenge expired")
	}
	sum := sha256.Sum256([]byte(claims.Challenge + nonce))
	prefix := hex.EncodeToString(sum[:])
	need := v.difficulty
	if len(prefix) < need || prefix[:need] != repeatZero(need) {
		return "", errors.New("proof of work failed")
	}
	return claims.Challenge, nil
}

func repeatZero(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

func jwtSecretFromEnv() string {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		return s
	}
	return "lyricsplus-submit-opensource-yes-yes-yes"
}

func matchBestUserRow(rows []*storage.Row, q domain.SearchQuery) *storage.Row {
	var userRows []*storage.Row
	for _, r := range rows {
		if r != nil && r.Source == "lyricsplus" {
			userRows = append(userRows, r)
		}
	}
	if len(userRows) == 0 {
		return nil
	}
	return matchBestRow(userRows, q)
}

func matchBestRow(rows []*storage.Row, q domain.SearchQuery) *storage.Row {
	if len(rows) == 0 {
		return nil
	}
	if len(rows) == 1 {
		r := rows[0]
		if q.Duration > 0 && r.DurationMS > 0 {
			diff := math.Abs(float64(q.Duration-r.DurationMS)) / 1000.0
			if diff > 15.0 {
				return nil
			}
		}
		return r
	}

	durationSec := float64(q.Duration) / 1000.0
	if durationSec > 0 || q.Album != "" {
		candidates := make([]similarity.SongCandidate, len(rows))
		for i, r := range rows {
			album := ""
			durMs := r.DurationMS
			if r.Filename != "" {
				pf := storage.ParseFilename(r.Filename)
				if pf.Album != "" {
					album = pf.Album
				}
				if durMs <= 0 && pf.DurationMS > 0 {
					durMs = pf.DurationMS
				}
			}
			candidates[i] = similarity.SongCandidate{
				Title:      r.Title,
				Artist:     r.Artist,
				Album:      album,
				DurationMs: durMs,
				ISRC:       r.ISRC,
				PlatformID: r.PlatformID,
				Data:       r,
			}
		}

		best := similarity.FindBestSongMatch(candidates, q.Title, q.Artist, q.Album, durationSec, q.ISRC, q.PlatformID)
		if best != nil {
			return best.Candidate.Data.(*storage.Row)
		}
		if durationSec > 0 {
			return nil
		}
	}
	return rows[0]
}
