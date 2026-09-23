// Package config loads and validates all server configuration from the environment,
// .env files, or JSON auth/config files.
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Server holds HTTP server tuning.
type Server struct {
	Addr             string
	MaxConcurrency   int
	MaxURLBytes      int
	MaxQueryParams   int
	MaxQueryValueLen int
	Proxy            Proxy
	ShutdownTimeout  time.Duration
}

// Proxy configures the SSRF-safe forward proxy for outbound requests.
type Proxy struct {
	Enabled     bool
	URL         string
	URLs        []string
	TokenHeader string
	Token       string
}

// SpotifyAccount holds a single Spotify credential pair. Configure multiple
// via the SPOTIFY_ACCOUNTS env var (JSON array), otherwise the single scalar
// env vars are used.
type SpotifyAccount struct {
	NAMEID        string `json:"NAMEID"`
	CLIENT_ID     string `json:"CLIENT_ID"`
	CLIENT_SECRET string `json:"CLIENT_SECRET"`
	COOKIE        string `json:"COOKIE"`
}

// AppleAccount holds a single Apple Music credential (android or web auth).
type AppleAccount struct {
	NAMEID             string `json:"NAMEID"`
	AUTH_TYPE          string `json:"AUTH_TYPE"`
	ANDROID_AUTH_TOKEN string `json:"ANDROID_AUTH_TOKEN"`
	ANDROID_DSID       string `json:"ANDROID_DSID"`
	ANDROID_USER_AGENT string `json:"ANDROID_USER_AGENT"`
	ANDROID_COOKIE     string `json:"ANDROID_COOKIE"`
	STOREFRONT         string `json:"STOREFRONT"`
	MUSIC_AUTH_TOKEN   string `json:"MUSIC_AUTH_TOKEN"`
}

// MusixmatchAccount holds a single Musixmatch credential (web or android auth).
type MusixmatchAccount struct {
	NAMEID     string `json:"NAMEID"`
	AUTH_TYPE  string `json:"AUTH_TYPE"`
	USER_AGENT string `json:"USER_AGENT"`
	COOKIE     string `json:"COOKIE"`
	EMAIL      string `json:"EMAIL"`
	PASSWORD   string `json:"PASSWORD"`
}

// DeezerAccount holds a single Deezer credential.
type DeezerAccount struct {
	NAMEID        string `json:"NAMEID"`
	AUTH_TYPE     string `json:"AUTH_TYPE"`
	REFRESH_TOKEN string `json:"REFRESH_TOKEN"`
	ARL           string `json:"ARL"`
}

// IsConfigured reports whether the account carries any usable credential.
func (a SpotifyAccount) IsConfigured() bool {
	return a.COOKIE != "" || (a.CLIENT_ID != "" && a.CLIENT_SECRET != "")
}

// IsConfigured reports whether the account carries any usable credential.
func (a AppleAccount) IsConfigured() bool {
	if strings.EqualFold(a.AUTH_TYPE, "android") || a.AUTH_TYPE == "" {
		return a.ANDROID_AUTH_TOKEN != ""
	}
	return a.MUSIC_AUTH_TOKEN != ""
}

// IsConfigured reports whether the account carries any usable credential.
func (a MusixmatchAccount) IsConfigured() bool {
	return a.COOKIE != "" || a.USER_AGENT != "" || (a.EMAIL != "" && a.PASSWORD != "")
}

// IsConfigured reports whether the account carries any usable credential.
func (a DeezerAccount) IsConfigured() bool {
	return a.REFRESH_TOKEN != "" || a.ARL != ""
}

// Provider mirrors credentials and tuning knobs for each provider.
type Provider struct {
	Timeout time.Duration
	// Spotify
	SpotifySpotifySecretsURL string
	SpotifySpotifyDCCookie   string
	SpotifyClientID          string
	SpotifyClientSecret      string
	SpotifyFallbackSecrets   [][]int
	SpotifyAccounts          []SpotifyAccount
	// Musixmatch
	MusixmatchCookie          string
	MusixmatchUserAgent       string
	MusixmatchAndroidEmail    string
	MusixmatchAndroidPassword string
	MusixmatchAccounts        []MusixmatchAccount
	// Apple
	AppleAndroidToken     string
	AppleAndroidDsid      string
	AppleAndroidUserAgent string
	AppleAndroidCookie    string
	AppleStorefront       string
	AppleMediaUserToken   string
	AppleAccounts         []AppleAccount
	// QQ Music
	QQCookie string
	// Deezer
	DeezerARL          string
	DeezerRefreshToken string
	DeezerAccounts     []DeezerAccount
}

// LyricsPlus holds UGC PoW configuration.
type LyricsPlus struct {
	JWTSecret        string
	ChallengeTTL     time.Duration
	PoWDifficulty    int
	MaxBodyBytes     int64
	AcceptVandalism  bool
	AllowSubmissions bool
}

// RateLimit configures sliding-window limiter.
type RateLimit struct {
	Requests int
	Window   time.Duration
}

// Storage holds the two-tier cache knobs.
type Storage struct {
	DBPath                  string
	LRUSize                 int
	MaxActorKeysTempl       string
	ContentCacheBytes       int64
	ExactTTL                time.Duration
	ExistingTTL             time.Duration
	NegativeTTL             time.Duration
	GDriveFolders           []string
	CircuitBreakerThreshold int
	CircuitBreakerCooldown  time.Duration
}

// Cache holds in-memory HTTP cache sizing.
type Cache struct {
	MaxEntries         int
	MaxBytes           int64
	MaxBodyBytes       int64
	MemoryWatchdogByte int64
}

// Logger configures the structured application logger.
type Logger struct {
	Enabled bool
	Level   string // debug | info | warn | error | disabled
	Format  string // text | json
}

// GDriveAccount holds a single OAuth2 credential for Google Drive.
type GDriveAccount struct {
	ClientID     string `json:"CLIENT_ID"`
	ClientSecret string `json:"CLIENT_SECRET"`
	RefreshToken string `json:"REFRESH_TOKEN"`
	Root         string `json:"ROOT"`
}

// GDrive holds Google Drive configuration, multi-account rotation, and folder IDs.
type GDrive struct {
	Enabled           bool
	Accounts          []GDriveAccount
	FolderUserTML     []string
	FolderTTML        []string
	FolderSpotify     []string
	FolderMusixmatch  []string
	FolderQQ          []string
	FolderDeezer      []string
	FolderBackup      []string
	DailyDumpEnabled  bool
	DailyDumpInterval time.Duration
	DailyDumpDir      string
}

// Config is the root configuration tree.
type Config struct {
	Server     Server
	Provider   Provider
	LyricsPlus LyricsPlus
	RateLimit  RateLimit
	Storage    Storage
	Cache      Cache
	GDrive     GDrive
	Logger     Logger
}

// Load reads configuration from the environment, .env files, or JSON config files.
// If customPaths is specified (e.g. from --config flag), it loads those files.
// Otherwise, it auto-discovers auth.json, config.json, or .env in standard locations.
func Load(customPaths ...string) Config {
	loaded := false
	for _, p := range customPaths {
		if p != "" {
			_ = LoadFile(p)
			loaded = true
		}
	}
	if !loaded {
		AutoDiscoverAndLoad()
	}

	return Config{
		Server: Server{
			Addr:             env("PORT", "3000"),
			MaxConcurrency:   envInt("MAX_CONCURRENCY", 15000),
			MaxURLBytes:      envInt("MAX_URL_BYTES", 4096),
			MaxQueryParams:   envInt("MAX_QUERY_PARAMS", 20),
			MaxQueryValueLen: envInt("MAX_QUERY_VALUE_LEN", 500),
			Proxy: Proxy{
				Enabled:     envBool("SERVER_PROXY_ENABLED", false),
				URL:         env("SERVER_PROXY_URL", "https://proxy-lyplus.prjktla.workers.dev/?url="),
				URLs:        parseProxyURLs(),
				TokenHeader: env("SERVER_PROXY_TOKEN_HEADER", "x-proxy-token"),
				Token:       os.Getenv("SERVER_PROXY_TOKEN"),
			},
			ShutdownTimeout: time.Duration(envInt("SHUTDOWN_TIMEOUT_MS", 10000)) * time.Millisecond,
		},
		Provider: Provider{
			Timeout:                  time.Duration(envInt("PROVIDER_TIMEOUT_MS", 8000)) * time.Millisecond,
			SpotifySpotifySecretsURL: env("SPOTIFY_SECRETS_URL", "https://raw.githubusercontent.com/Thereallo1026/spotify-secrets/main/secrets/secretDict.json"),
			SpotifySpotifyDCCookie:   env("SPOTIFY_COOKIE", ""),
			SpotifyClientID:          env("SPOTIFY_CLIENT_ID", ""),
			SpotifyClientSecret:      env("SPOTIFY_CLIENT_SECRET", ""),
			SpotifyFallbackSecrets: [][]int{
				{62, 54, 109, 83, 107, 77, 41, 103, 45, 93, 114, 38, 41, 97, 64, 51, 95, 94, 95, 94},
				{59, 92, 64, 70, 99, 78, 117, 75, 99, 103, 116, 67, 103, 51, 87, 63, 93, 59, 70, 45, 32},
				{107, 81, 49, 57, 67, 93, 87, 81, 69, 67, 40, 93, 48, 50, 46, 91, 94, 113, 41, 108, 77, 107, 34},
			},
			SpotifyAccounts:           loadSpotifyAccounts(),
			MusixmatchCookie:          env("MUSIXMATCH_COOKIE", ""),
			MusixmatchUserAgent:       env("MUSIXMATCH_USER_AGENT", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"),
			MusixmatchAndroidEmail:    env("MUSIXMATCH_ANDROID_EMAIL", ""),
			MusixmatchAndroidPassword: env("MUSIXMATCH_ANDROID_PASSWORD", ""),
			MusixmatchAccounts:        loadMusixmatchAccounts(),
			AppleAndroidToken:         env("APPLE_MUSIC_ANDROID_AUTH_TOKEN", ""),
			AppleAndroidDsid:          env("APPLE_MUSIC_ANDROID_DSID", ""),
			AppleAndroidUserAgent:     env("APPLE_MUSIC_ANDROID_USER_AGENT", "Music/6.1 Android/15 model/XiaomiPOCOF1 build/1451 (dt:66)"),
			AppleAndroidCookie:        env("APPLE_MUSIC_ANDROID_COOKIE", ""),
			AppleStorefront:           env("APPLE_MUSIC_STOREFRONT", "in"),
			AppleMediaUserToken:       env("APPLE_MUSIC_AUTH_TOKEN", ""),
			AppleAccounts:             loadAppleAccounts(),
			QQCookie:                  os.Getenv("QQ_COOKIE"),
			DeezerARL:                 os.Getenv("DEEZER_ARL"),
			DeezerRefreshToken:        os.Getenv("DEEZER_REFRESH_TOKEN"),
			DeezerAccounts:            loadDeezerAccounts(),
		},
		LyricsPlus: LyricsPlus{
			JWTSecret:        env("JWT_SECRET", "lyricsplus-submit-opensource-yes-yes-yes"),
			ChallengeTTL:     time.Duration(envInt("LYRICSPLUS_CHALLENGE_TTL_MS", 600000)) * time.Millisecond,
			PoWDifficulty:    envInt("LYRICSPLUS_POW_DIFFICULTY", 5),
			MaxBodyBytes:     envInt64("MAX_BODY_BYTES", 1<<20),
			AcceptVandalism:  envBool("LYRICSPLUS_ACCEPT_VANDALISM", false),
			AllowSubmissions: envBool("ALLOW_SUBMISSIONS", true),
		},
		RateLimit: RateLimit{
			Requests: envInt("RATE_LIMIT_REQUESTS", 20),
			Window:   time.Duration(envInt("RATE_LIMIT_WINDOW_MS", 10000)) * time.Millisecond,
		},
		Storage: Storage{
			DBPath:                  env("SQLITE_PATH", env("CACHE_DB_PATH", "database/lyrics_cache.db")),
			LRUSize:                 envInt("LRU_SIZE", 20000),
			ContentCacheBytes:       envInt64("CONTENT_CACHE_BYTES", 48<<20),
			ExactTTL:                time.Duration(envInt("EXACT_TTL_MS", 900000)) * time.Millisecond,
			ExistingTTL:             time.Duration(envInt("EXISTING_TTL_MS", 900000)) * time.Millisecond,
			NegativeTTL:             time.Duration(envInt("NEGATIVE_TTL_MS", 30000)) * time.Millisecond,
			CircuitBreakerThreshold: envInt("GDRIVE_CIRCUIT_BREAKER_THRESHOLD", 5),
			CircuitBreakerCooldown:  time.Duration(envInt("GDRIVE_CIRCUIT_BREAKER_COOLDOWN_MS", 60000)) * time.Millisecond,
		},
		Cache: Cache{
			MaxEntries:         envInt("HTTP_CACHE_MAX_KEYS", 2000),
			MaxBytes:           envInt64("HTTP_CACHE_MAX_BYTES", 96<<20),
			MaxBodyBytes:       envInt64("HTTP_CACHE_MAX_BODY_BYTES", 1<<20*3/2),
			MemoryWatchdogByte: envInt64("MEMORY_WATCHDOG_BYTES", 3072<<20),
		},
		GDrive: GDrive{
			Enabled:           envBool("GDRIVE_ENABLED", true),
			Accounts:          loadGDriveAccounts(),
			FolderUserTML:     parseFolderIDs(os.Getenv("GDRIVE_USERTML_JSON"), "1RFoNsI5wAsRjQSVDOMaotDmMZNIQOWnW"),
			FolderTTML:        parseFolderIDs(os.Getenv("GDRIVE_CACHED_TTML"), ""),
			FolderSpotify:     parseFolderIDs(os.Getenv("GDRIVE_CACHED_SPOTIFY"), ""),
			FolderMusixmatch:  parseFolderIDs(os.Getenv("GDRIVE_CACHED_MUSIXMATCH"), ""),
			FolderQQ:          parseFolderIDs(os.Getenv("GDRIVE_CACHED_QQ"), ""),
			FolderDeezer:      parseFolderIDs(os.Getenv("GDRIVE_CACHED_DEEZER"), ""),
			FolderBackup:      parseFolderIDs(os.Getenv("GDRIVE_BACKUP_FOLDER"), ""),
			DailyDumpEnabled:  envBool("DAILY_DUMP_ENABLED", false),
			DailyDumpInterval: time.Duration(envInt("DAILY_DUMP_INTERVAL_HOURS", 24)) * time.Hour,
			DailyDumpDir:      env("DAILY_DUMP_DIR", "data/dumps"),
		},
		Logger: Logger{
			Enabled: !envBool("DISABLE_LOGGING", false),
			Level:   env("LOG_LEVEL", "info"),
			Format:  env("LOG_FORMAT", "text"),
		},
	}
}

// LoadFile reads configuration from a specific file (.json or .env).
// It populates environment variables that are not already set.
func LoadFile(path string) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	ext := strings.ToLower(filepath.Ext(clean))
	if ext == ".json" {
		return loadJSONConfig(clean)
	}
	return loadDotEnv(clean)
}

// AutoDiscoverAndLoad looks for auth.json, config.json, or .env in standard locations
// and loads them if present (without overwriting existing environment variables).
func AutoDiscoverAndLoad() {
	if custom := os.Getenv("CONFIG_PATH"); custom != "" {
		_ = LoadFile(custom)
		return
	}

	for _, jsonPath := range []string{"auth.json", "data/auth.json", "config.json"} {
		if _, err := os.Stat(jsonPath); err == nil {
			_ = loadJSONConfig(jsonPath)
			break
		}
	}

	for _, envPath := range []string{".env", "../.env"} {
		if _, err := os.Stat(envPath); err == nil {
			_ = loadDotEnv(envPath)
			break
		}
	}
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eqIdx])
		if key == "" {
			continue
		}
		val := strings.TrimSpace(line[eqIdx+1:])

		// Strip quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			quote := val[0]
			val = val[1 : len(val)-1]
			if quote == '"' {
				val = unescapeString(val)
			}
		} else {
			// Strip trailing inline comments if not quoted
			if cIdx := strings.Index(val, " #"); cIdx >= 0 {
				val = strings.TrimSpace(val[:cIdx])
			}
		}

		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}

func unescapeString(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\t`, "\t")
	s = strings.ReplaceAll(s, `\"`, "\"")
	s = strings.ReplaceAll(s, `\\`, "\\")
	return s
}

func loadJSONConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		// Array of GDrive accounts directly
		if _, exists := os.LookupEnv("GDRIVE_ACCOUNTS"); !exists {
			_ = os.Setenv("GDRIVE_ACCOUNTS", trimmed)
		}
		return nil
	}

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return fmt.Errorf("parse json config %s: %w", path, err)
	}

	setEnvIfUnset := func(key, val string) {
		if val == "" {
			return
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}

	// 1. Process root-level keys
	for k, v := range rawMap {
		var strVal string
		if err := json.Unmarshal(v, &strVal); err == nil {
			upperK := strings.ToUpper(k)
			switch upperK {
			case "CLIENT_ID":
				setEnvIfUnset("AUTH_KEY_CLIENT_ID", strVal)
			case "CLIENT_SECRET":
				setEnvIfUnset("AUTH_KEY_CLIENT_SECRET", strVal)
			case "REFRESH_TOKEN":
				setEnvIfUnset("AUTH_KEY_REFRESH_TOKEN", strVal)
			case "ROOT":
				setEnvIfUnset("AUTH_KEY_ROOT", strVal)
			case "JWT_SECRET":
				setEnvIfUnset("JWT_SECRET", strVal)
			default:
				setEnvIfUnset(upperK, strVal)
			}
			continue
		}

		var boolVal bool
		if err := json.Unmarshal(v, &boolVal); err == nil {
			setEnvIfUnset(strings.ToUpper(k), strconv.FormatBool(boolVal))
			continue
		}
		var intVal int64
		if err := json.Unmarshal(v, &intVal); err == nil {
			setEnvIfUnset(strings.ToUpper(k), strconv.FormatInt(intVal, 10))
			continue
		}
	}

	// 2. Check for GCP installed/web client credentials
	for _, wrapperKey := range []string{"installed", "web"} {
		if raw, ok := rawMap[wrapperKey]; ok {
			var wrapper struct {
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
				RefreshToken string `json:"refresh_token"`
				Root         string `json:"root"`
			}
			if err := json.Unmarshal(raw, &wrapper); err == nil {
				setEnvIfUnset("AUTH_KEY_CLIENT_ID", wrapper.ClientID)
				setEnvIfUnset("AUTH_KEY_CLIENT_SECRET", wrapper.ClientSecret)
				setEnvIfUnset("AUTH_KEY_REFRESH_TOKEN", wrapper.RefreshToken)
				setEnvIfUnset("AUTH_KEY_ROOT", wrapper.Root)
			}
		}
	}

	// 3. Check for structured "gdrive" object
	if raw, ok := rawMap["gdrive"]; ok {
		var gd struct {
			Enabled  *bool           `json:"enabled"`
			Folders  map[string]any  `json:"folders"`
			Accounts json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(raw, &gd); err == nil {
			if gd.Enabled != nil {
				setEnvIfUnset("GDRIVE_ENABLED", strconv.FormatBool(*gd.Enabled))
			}
			if len(gd.Accounts) > 0 {
				setEnvIfUnset("GDRIVE_ACCOUNTS", string(gd.Accounts))
			}
			for folderName, folderVal := range gd.Folders {
				valStr := fmt.Sprintf("%v", folderVal)
				switch strings.ToLower(folderName) {
				case "user_tml", "usertml", "gdrive_usertml_json":
					setEnvIfUnset("GDRIVE_USERTML_JSON", valStr)
				case "ttml", "cached_ttml", "gdrive_cached_ttml":
					setEnvIfUnset("GDRIVE_CACHED_TTML", valStr)
				case "spotify", "cached_spotify", "gdrive_cached_spotify":
					setEnvIfUnset("GDRIVE_CACHED_SPOTIFY", valStr)
				case "musixmatch", "cached_musixmatch", "gdrive_cached_musixmatch":
					setEnvIfUnset("GDRIVE_CACHED_MUSIXMATCH", valStr)
				case "qq", "cached_qq", "gdrive_cached_qq":
					setEnvIfUnset("GDRIVE_CACHED_QQ", valStr)
				case "deezer", "cached_deezer", "gdrive_cached_deezer":
					setEnvIfUnset("GDRIVE_CACHED_DEEZER", valStr)
				case "backup", "backup_folder", "gdrive_backup_folder":
					setEnvIfUnset("GDRIVE_BACKUP_FOLDER", valStr)
				}
			}
		}
	}

	// 4. Check for structured "provider" object
	if raw, ok := rawMap["provider"]; ok {
		var prov map[string]any
		if err := json.Unmarshal(raw, &prov); err == nil {
			for pk, pv := range prov {
				// Account arrays are stored as JSON arrays: SPOTIFY_ACCOUNTS,
				// APPLE_MUSIC_ACCOUNTS, MUSIXMATCH_ACCOUNTS, DEEZER_ACCOUNTS.
				switch strings.ToLower(pk) {
				case "spotify_accounts":
					setEnvIfUnset("SPOTIFY_ACCOUNTS", marshalJSONValue(pv))
					continue
				case "apple_accounts", "apple_music_accounts":
					setEnvIfUnset("APPLE_MUSIC_ACCOUNTS", marshalJSONValue(pv))
					continue
				case "musixmatch_accounts":
					setEnvIfUnset("MUSIXMATCH_ACCOUNTS", marshalJSONValue(pv))
					continue
				case "deezer_accounts":
					setEnvIfUnset("DEEZER_ACCOUNTS", marshalJSONValue(pv))
					continue
				}
				valStr := fmt.Sprintf("%v", pv)
				switch strings.ToLower(pk) {
				case "spotify_cookie", "spotify_dc_cookie":
					setEnvIfUnset("SPOTIFY_COOKIE", valStr)
				case "spotify_client_id":
					setEnvIfUnset("SPOTIFY_CLIENT_ID", valStr)
				case "spotify_client_secret":
					setEnvIfUnset("SPOTIFY_CLIENT_SECRET", valStr)
				case "spotify_secrets_url":
					setEnvIfUnset("SPOTIFY_SECRETS_URL", valStr)
				case "musixmatch_cookie":
					setEnvIfUnset("MUSIXMATCH_COOKIE", valStr)
				case "musixmatch_user_agent":
					setEnvIfUnset("MUSIXMATCH_USER_AGENT", valStr)
				case "musixmatch_android_email":
					setEnvIfUnset("MUSIXMATCH_ANDROID_EMAIL", valStr)
				case "musixmatch_android_password":
					setEnvIfUnset("MUSIXMATCH_ANDROID_PASSWORD", valStr)
				case "apple_music_android_auth_token", "apple_android_token":
					setEnvIfUnset("APPLE_MUSIC_ANDROID_AUTH_TOKEN", valStr)
				case "apple_music_android_dsid", "apple_android_dsid":
					setEnvIfUnset("APPLE_MUSIC_ANDROID_DSID", valStr)
				case "apple_music_android_user_agent", "apple_android_user_agent":
					setEnvIfUnset("APPLE_MUSIC_ANDROID_USER_AGENT", valStr)
				case "apple_music_android_cookie", "apple_android_cookie":
					setEnvIfUnset("APPLE_MUSIC_ANDROID_COOKIE", valStr)
				case "apple_music_storefront", "apple_storefront":
					setEnvIfUnset("APPLE_MUSIC_STOREFRONT", valStr)
				case "apple_music_auth_token", "apple_media_user_token":
					setEnvIfUnset("APPLE_MUSIC_AUTH_TOKEN", valStr)
				case "qq_cookie":
					setEnvIfUnset("QQ_COOKIE", valStr)
				case "deezer_arl":
					setEnvIfUnset("DEEZER_ARL", valStr)
				case "deezer_refresh_token":
					setEnvIfUnset("DEEZER_REFRESH_TOKEN", valStr)
				}
			}
		}
	}

	return nil
}

func marshalJSONValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseFolderIDs(envVal, defVal string) []string {
	raw := envVal
	if raw == "" {
		raw = defVal
	}
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	var res []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			res = append(res, trimmed)
		}
	}
	return res
}

func loadGDriveAccounts() []GDriveAccount {
	defAccount := GDriveAccount{
		ClientID:     env("AUTH_KEY_CLIENT_ID", ""),
		ClientSecret: env("AUTH_KEY_CLIENT_SECRET", ""),
		RefreshToken: env("AUTH_KEY_REFRESH_TOKEN", ""),
		Root:         env("AUTH_KEY_ROOT", ""),
	}

	envAccounts := os.Getenv("GDRIVE_ACCOUNTS")
	if envAccounts == "" {
		if defAccount.ClientID != "" || defAccount.RefreshToken != "" {
			return []GDriveAccount{defAccount}
		}
		return nil
	}

	var accounts []GDriveAccount
	trimmed := strings.TrimSpace(envAccounts)
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &accounts); err == nil && len(accounts) > 0 {
			return accounts
		}
	}

	// Parse comma/pipe-delimited format: CLIENT_ID|CLIENT_SECRET|REFRESH_TOKEN|ROOT, ...
	items := strings.Split(envAccounts, ",")
	for _, item := range items {
		fields := strings.Split(item, "|")
		if len(fields) >= 3 {
			acc := GDriveAccount{
				ClientID:     strings.TrimSpace(fields[0]),
				ClientSecret: strings.TrimSpace(fields[1]),
				RefreshToken: strings.TrimSpace(fields[2]),
			}
			if len(fields) >= 4 {
				acc.Root = strings.TrimSpace(fields[3])
			}
			accounts = append(accounts, acc)
		}
	}

	if len(accounts) == 0 && (defAccount.ClientID != "" || defAccount.RefreshToken != "") {
		return []GDriveAccount{defAccount}
	}
	return accounts
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseProxyURLs reads the proxy endpoint list. Prefer the comma-separated
// SERVER_PROXY_URLS list; fall back to the single SERVER_PROXY_URL value.
func parseProxyURLs() []string {
	raw := os.Getenv("SERVER_PROXY_URLS")
	if raw == "" {
		raw = os.Getenv("SERVER_PROXY_URL")
	}
	parts := strings.Split(raw, ",")
	var urls []string
	for _, p := range parts {
		if u := strings.TrimSpace(p); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}

// parseAccountList unmarshals a JSON array of accounts, normalizing object
// keys to UPPERCASE so both JS-style ("CLIENT_ID") and lowercase configs work.
func parseAccountList[T any](raw string) ([]T, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("empty account list")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	out := make([]T, 0, len(entries))
	for _, e := range entries {
		norm := make(map[string]json.RawMessage, len(e))
		for k, v := range e {
			norm[strings.ToUpper(k)] = v
		}
		b, err := json.Marshal(norm)
		if err != nil {
			continue
		}
		var item T
		if err := json.Unmarshal(b, &item); err != nil {
			continue
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid provider accounts in %q", raw)
	}
	return out, nil
}

func loadSpotifyAccounts() []SpotifyAccount {
	if raw := os.Getenv("SPOTIFY_ACCOUNTS"); raw != "" {
		if accounts, err := parseAccountList[SpotifyAccount](raw); err == nil {
			return accounts
		}
	}
	return []SpotifyAccount{{
		NAMEID:        "SpotifyDefault",
		CLIENT_ID:     env("SPOTIFY_CLIENT_ID", ""),
		CLIENT_SECRET: env("SPOTIFY_CLIENT_SECRET", ""),
		COOKIE:        env("SPOTIFY_COOKIE", ""),
	}}
}

func loadAppleAccounts() []AppleAccount {
	if raw := os.Getenv("APPLE_MUSIC_ACCOUNTS"); raw != "" {
		if accounts, err := parseAccountList[AppleAccount](raw); err == nil {
			return accounts
		}
	}
	return []AppleAccount{
		{
			NAMEID:             "AppleAndroid",
			AUTH_TYPE:          "android",
			ANDROID_AUTH_TOKEN: env("APPLE_MUSIC_ANDROID_AUTH_TOKEN", ""),
			ANDROID_DSID:       env("APPLE_MUSIC_ANDROID_DSID", ""),
			ANDROID_USER_AGENT: env("APPLE_MUSIC_ANDROID_USER_AGENT", "Music/6.1 Android/15 model/XiaomiPOCOF1 build/1451 (dt:66)"),
			ANDROID_COOKIE:     env("APPLE_MUSIC_ANDROID_COOKIE", ""),
			STOREFRONT:         env("APPLE_MUSIC_STOREFRONT", "in"),
		},
		{
			NAMEID:           "AppleWeb",
			AUTH_TYPE:        "web",
			MUSIC_AUTH_TOKEN: env("APPLE_MUSIC_AUTH_TOKEN", ""),
		},
	}
}

func loadMusixmatchAccounts() []MusixmatchAccount {
	if raw := os.Getenv("MUSIXMATCH_ACCOUNTS"); raw != "" {
		if accounts, err := parseAccountList[MusixmatchAccount](raw); err == nil {
			return accounts
		}
	}
	return []MusixmatchAccount{{
		NAMEID:     "Musixmatch-Guest",
		AUTH_TYPE:  "web",
		USER_AGENT: env("MUSIXMATCH_USER_AGENT", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"),
		COOKIE:     env("MUSIXMATCH_COOKIE", "AWSELB=55578B011601B1EF8BC274C33F9043CA947F99DCFF0A80541772015CA2B39C35C0F9E1C932D31725A7310BCAEB0C37431E024E2B45320B7F2C84490C2C97351FDE34690157"),
		EMAIL:      env("MUSIXMATCH_ANDROID_EMAIL", ""),
		PASSWORD:   env("MUSIXMATCH_ANDROID_PASSWORD", ""),
	}}
}

func loadDeezerAccounts() []DeezerAccount {
	if raw := os.Getenv("DEEZER_ACCOUNTS"); raw != "" {
		if accounts, err := parseAccountList[DeezerAccount](raw); err == nil {
			return accounts
		}
	}
	return []DeezerAccount{{
		NAMEID:        "DeezerDefault",
		AUTH_TYPE:     "refresh-token",
		REFRESH_TOKEN: os.Getenv("DEEZER_REFRESH_TOKEN"),
		ARL:           os.Getenv("DEEZER_ARL"),
	}}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}
