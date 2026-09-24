// Package config loads and validates all server configuration from JSON auth/config files,
// .env files, and environment variables.
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

// SpotifyAccount holds a single Spotify credential.
type SpotifyAccount struct {
	NAMEID        string `json:"NAMEID"`
	CLIENT_ID     string `json:"CLIENT_ID"`
	CLIENT_SECRET string `json:"CLIENT_SECRET"`
	COOKIE        string `json:"COOKIE"`
}

// AppleAccount holds a single Apple Music credential (android or web auth).
type AppleAccount struct {
	NAMEID             string `json:"NAMEID"`
	AUTH_TYPE          string `json:"AUTH_TYPE"` // "android" | "web"
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
	AUTH_TYPE  string `json:"AUTH_TYPE"` // "web" | "android"
	USER_AGENT string `json:"USER_AGENT"`
	COOKIE     string `json:"COOKIE"`
	EMAIL      string `json:"EMAIL"`
	PASSWORD   string `json:"PASSWORD"`
}

// DeezerAccount holds a single Deezer credential (refresh-token or arl).
type DeezerAccount struct {
	NAMEID        string `json:"NAMEID"`
	AUTH_TYPE     string `json:"AUTH_TYPE"` // "refresh-token" | "arl"
	REFRESH_TOKEN string `json:"REFRESH_TOKEN"`
	ARL           string `json:"ARL"`
}

// GDriveAccount holds a single OAuth2 credential for Google Drive.
type GDriveAccount struct {
	NAMEID       string `json:"NAMEID,omitempty"`
	ClientID     string `json:"CLIENT_ID"`
	ClientSecret string `json:"CLIENT_SECRET"`
	RefreshToken string `json:"REFRESH_TOKEN"`
	Root         string `json:"ROOT"`
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

// IsConfigured reports whether the GDrive account carries usable credentials.
func (a GDriveAccount) IsConfigured() bool {
	return a.ClientID != "" || a.RefreshToken != ""
}

// Provider mirrors credentials and tuning knobs for each provider.
type Provider struct {
	Timeout                  time.Duration
	SpotifySpotifySecretsURL string // Alias for SpotifySecretsURL for backward compatibility
	SpotifySecretsURL        string
	SpotifyFallbackSecrets   [][]int
	SpotifyAccounts          []SpotifyAccount

	// Legacy single-account aliases (automatically populated from Accounts)
	SpotifySpotifyDCCookie    string
	SpotifyClientID           string
	SpotifyClientSecret       string
	MusixmatchCookie          string
	MusixmatchUserAgent       string
	MusixmatchAndroidEmail    string
	MusixmatchAndroidPassword string
	MusixmatchAccounts        []MusixmatchAccount
	AppleAndroidToken         string
	AppleAndroidDsid          string
	AppleAndroidUserAgent     string
	AppleAndroidCookie        string
	AppleStorefront           string
	AppleMediaUserToken       string
	AppleAccounts             []AppleAccount
	QQCookie                  string
	DeezerARL                 string
	DeezerRefreshToken        string
	DeezerAccounts            []DeezerAccount
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

// Default returns a clean Config with safe production defaults.
func Default() Config {
	secretsURL := "https://raw.githubusercontent.com/Thereallo1026/spotify-secrets/main/secrets/secretDict.json"
	return Config{
		Server: Server{
			Addr:             "3000",
			MaxConcurrency:   15000,
			MaxURLBytes:      4096,
			MaxQueryParams:   20,
			MaxQueryValueLen: 500,
			Proxy: Proxy{
				Enabled:     false,
				URL:         "https://proxy-lyplus.prjktla.workers.dev/?url=",
				URLs:        []string{"https://proxy-lyplus.prjktla.workers.dev/?url="},
				TokenHeader: "x-proxy-token",
				Token:       "",
			},
			ShutdownTimeout: 10 * time.Second,
		},
		Provider: Provider{
			Timeout:                  8 * time.Second,
			SpotifySpotifySecretsURL: secretsURL,
			SpotifySecretsURL:        secretsURL,
			SpotifyFallbackSecrets: [][]int{
				{62, 54, 109, 83, 107, 77, 41, 103, 45, 93, 114, 38, 41, 97, 64, 51, 95, 94, 95, 94},
				{59, 92, 64, 70, 99, 78, 117, 75, 99, 103, 116, 67, 103, 51, 87, 63, 93, 59, 70, 45, 32},
				{107, 81, 49, 57, 67, 93, 87, 81, 69, 67, 40, 93, 48, 50, 46, 91, 94, 113, 41, 108, 77, 107, 34},
			},
			SpotifyAccounts:    nil,
			MusixmatchAccounts: nil,
			AppleAccounts:      nil,
			DeezerAccounts:     nil,
		},
		LyricsPlus: LyricsPlus{
			JWTSecret:        "lyricsplus-submit-opensource-yes-yes-yes",
			ChallengeTTL:     10 * time.Minute,
			PoWDifficulty:    5,
			MaxBodyBytes:     1 << 20,
			AcceptVandalism:  false,
			AllowSubmissions: true,
		},
		RateLimit: RateLimit{
			Requests: 20,
			Window:   10 * time.Second,
		},
		Storage: Storage{
			DBPath:                  "database/lyrics_cache.db",
			LRUSize:                 20000,
			ContentCacheBytes:       48 << 20,
			ExactTTL:                15 * time.Minute,
			ExistingTTL:             15 * time.Minute,
			NegativeTTL:             30 * time.Second,
			CircuitBreakerThreshold: 5,
			CircuitBreakerCooldown:  60 * time.Second,
		},
		Cache: Cache{
			MaxEntries:         2000,
			MaxBytes:           96 << 20,
			MaxBodyBytes:       1 << 20 * 3 / 2,
			MemoryWatchdogByte: 3072 << 20,
		},
		GDrive: GDrive{
			Enabled:           false,
			Accounts:          nil,
			FolderUserTML:     []string{"1RFoNsI5wAsRjQSVDOMaotDmMZNIQOWnW"},
			FolderTTML:        nil,
			FolderSpotify:     nil,
			FolderMusixmatch:  nil,
			FolderQQ:          nil,
			FolderDeezer:      nil,
			FolderBackup:      nil,
			DailyDumpEnabled:  false,
			DailyDumpInterval: 24 * time.Hour,
			DailyDumpDir:      "data/dumps",
		},
		Logger: Logger{
			Enabled: true,
			Level:   "info",
			Format:  "text",
		},
	}
}

// Load reads configuration by layering:
// 1. Defaults
// 2. Config files (JSON or .env)
// 3. Environment variable overrides (highest precedence)
func Load(customPaths ...string) Config {
	cfg := Default()
	loaded := false

	for _, p := range customPaths {
		if p != "" {
			_ = loadConfigFile(&cfg, p)
			loaded = true
		}
	}

	if !loaded {
		autoDiscover(&cfg)
	}

	// Layer real environment variables on top
	applyEnvOverrides(&cfg)

	// Post-process accounts & backward compatibility aliases
	finalizeConfig(&cfg)

	return cfg
}

// LoadFile reads configuration from a specific file (.json or .env) and overlays it.
func LoadFile(path string) error {
	cfg := Default()
	err := loadConfigFile(&cfg, path)
	if err != nil {
		return err
	}
	// For backward-compatibility with tests or code expecting env vars set by LoadFile:
	return nil
}

// AutoDiscoverAndLoad discovers and applies configuration files in standard locations.
func AutoDiscoverAndLoad() {
	_ = Load()
}

func autoDiscover(cfg *Config) {
	if custom := os.Getenv("CONFIG_PATH"); custom != "" {
		_ = loadConfigFile(cfg, custom)
		return
	}

	// 1. JSON configurations
	for _, jsonPath := range []string{"auth.json", "data/auth.json", "config.json"} {
		if _, err := os.Stat(jsonPath); err == nil {
			_ = loadConfigFile(cfg, jsonPath)
			break
		}
	}

	// 2. DotEnv configurations
	for _, envPath := range []string{".env", "../.env"} {
		if _, err := os.Stat(envPath); err == nil {
			_ = loadConfigFile(cfg, envPath)
			break
		}
	}
}

func loadConfigFile(cfg *Config, path string) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	ext := strings.ToLower(filepath.Ext(clean))
	if ext == ".json" {
		return loadJSONConfig(cfg, clean)
	}
	return loadDotEnv(cfg, clean)
}

// ============================================================================
// JSON Config Parser
// ============================================================================

func loadJSONConfig(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		// Raw JSON array of GDrive accounts
		var accounts []GDriveAccount
		if err := parseAccountListToSlice(trimmed, &accounts); err == nil && len(accounts) > 0 {
			cfg.GDrive.Accounts = accounts
			cfg.GDrive.Enabled = true
		}
		return nil
	}

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return fmt.Errorf("parse json config %s: %w", path, err)
	}

	// 1. Root-level GCP installed / web credentials
	for _, wrapperKey := range []string{"installed", "web"} {
		if raw, ok := rawMap[wrapperKey]; ok {
			var wrapper struct {
				ClientID     string `json:"client_id"`
				ClientSecret string `json:"client_secret"`
				RefreshToken string `json:"refresh_token"`
				Root         string `json:"root"`
			}
			if err := json.Unmarshal(raw, &wrapper); err == nil {
				if wrapper.ClientID != "" || wrapper.RefreshToken != "" {
					cfg.GDrive.Accounts = append(cfg.GDrive.Accounts, GDriveAccount{
						NAMEID:       "gcp-" + wrapperKey,
						ClientID:     wrapper.ClientID,
						ClientSecret: wrapper.ClientSecret,
						RefreshToken: wrapper.RefreshToken,
						Root:         wrapper.Root,
					})
					cfg.GDrive.Enabled = true
				}
			}
		}
	}

	// 2. Root-level GDrive scalar fields
	var rootGDrive struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
		Root         string `json:"root"`
	}
	if err := json.Unmarshal(data, &rootGDrive); err == nil {
		if rootGDrive.ClientID != "" || rootGDrive.RefreshToken != "" {
			cfg.GDrive.Accounts = append(cfg.GDrive.Accounts, GDriveAccount{
				NAMEID:       "root-gdrive",
				ClientID:     rootGDrive.ClientID,
				ClientSecret: rootGDrive.ClientSecret,
				RefreshToken: rootGDrive.RefreshToken,
				Root:         rootGDrive.Root,
			})
		}
	}

	// 3. Root-level common fields
	if raw, ok := rawMap["jwt_secret"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			cfg.LyricsPlus.JWTSecret = s
		}
	}
	if raw, ok := rawMap["port"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			cfg.Server.Addr = s
		} else {
			var i int
			if err := json.Unmarshal(raw, &i); err == nil && i > 0 {
				cfg.Server.Addr = strconv.Itoa(i)
			}
		}
	}
	if raw, ok := rawMap["server_proxy_urls"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			cfg.Server.Proxy.URLs = parseCommaSlice(s)
		} else {
			var arr []string
			if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
				cfg.Server.Proxy.URLs = arr
			}
		}
	}
	if raw, ok := rawMap["server_proxy_url"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			cfg.Server.Proxy.URL = s
			if len(cfg.Server.Proxy.URLs) == 0 {
				cfg.Server.Proxy.URLs = []string{s}
			}
		}
	}
	if raw, ok := rawMap["server_proxy_enabled"]; ok {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			cfg.Server.Proxy.Enabled = b
		}
	}

	// 4. Structured "server"
	if raw, ok := rawMap["server"]; ok {
		var s struct {
			Port             *string  `json:"port"`
			Addr             *string  `json:"addr"`
			MaxConcurrency   *int     `json:"max_concurrency"`
			MaxURLBytes      *int     `json:"max_url_bytes"`
			MaxQueryParams   *int     `json:"max_query_params"`
			MaxQueryValueLen *int     `json:"max_query_value_len"`
			ProxyEnabled     *bool    `json:"proxy_enabled"`
			ProxyURL         *string  `json:"proxy_url"`
			ProxyURLs        []string `json:"proxy_urls"`
			ProxyToken       *string  `json:"proxy_token"`
			ProxyTokenHeader *string  `json:"proxy_token_header"`
			ShutdownMS       *int     `json:"shutdown_timeout_ms"`
		}
		if err := json.Unmarshal(raw, &s); err == nil {
			if s.Port != nil && *s.Port != "" {
				cfg.Server.Addr = *s.Port
			}
			if s.Addr != nil && *s.Addr != "" {
				cfg.Server.Addr = *s.Addr
			}
			if s.MaxConcurrency != nil {
				cfg.Server.MaxConcurrency = *s.MaxConcurrency
			}
			if s.MaxURLBytes != nil {
				cfg.Server.MaxURLBytes = *s.MaxURLBytes
			}
			if s.MaxQueryParams != nil {
				cfg.Server.MaxQueryParams = *s.MaxQueryParams
			}
			if s.MaxQueryValueLen != nil {
				cfg.Server.MaxQueryValueLen = *s.MaxQueryValueLen
			}
			if s.ProxyEnabled != nil {
				cfg.Server.Proxy.Enabled = *s.ProxyEnabled
			}
			if s.ProxyURL != nil && *s.ProxyURL != "" {
				cfg.Server.Proxy.URL = *s.ProxyURL
			}
			if len(s.ProxyURLs) > 0 {
				cfg.Server.Proxy.URLs = s.ProxyURLs
			}
			if s.ProxyToken != nil {
				cfg.Server.Proxy.Token = *s.ProxyToken
			}
			if s.ProxyTokenHeader != nil {
				cfg.Server.Proxy.TokenHeader = *s.ProxyTokenHeader
			}
			if s.ShutdownMS != nil {
				cfg.Server.ShutdownTimeout = time.Duration(*s.ShutdownMS) * time.Millisecond
			}
		}
	}

	// 5. Structured "gdrive"
	if raw, ok := rawMap["gdrive"]; ok {
		var gd struct {
			Enabled           *bool           `json:"enabled"`
			Folders           map[string]any  `json:"folders"`
			Accounts          []GDriveAccount `json:"accounts"`
			AccountsRaw       json.RawMessage `json:"accounts_raw"`
			DailyDumpEnabled  *bool           `json:"daily_dump_enabled"`
			DailyDumpInterval *int            `json:"daily_dump_interval_hours"`
			DailyDumpDir      *string         `json:"daily_dump_dir"`
			Threshold         *int            `json:"circuit_breaker_threshold"`
			CooldownMS        *int            `json:"circuit_breaker_cooldown_ms"`
		}
		if err := json.Unmarshal(raw, &gd); err == nil {
			if gd.Enabled != nil {
				cfg.GDrive.Enabled = *gd.Enabled
			}
			if len(gd.Accounts) > 0 {
				cfg.GDrive.Accounts = gd.Accounts
			} else if len(gd.AccountsRaw) > 0 {
				var accs []GDriveAccount
				if err := parseAccountListToSlice(string(gd.AccountsRaw), &accs); err == nil && len(accs) > 0 {
					cfg.GDrive.Accounts = accs
				}
			}
			if gd.DailyDumpEnabled != nil {
				cfg.GDrive.DailyDumpEnabled = *gd.DailyDumpEnabled
			}
			if gd.DailyDumpInterval != nil {
				cfg.GDrive.DailyDumpInterval = time.Duration(*gd.DailyDumpInterval) * time.Hour
			}
			if gd.DailyDumpDir != nil && *gd.DailyDumpDir != "" {
				cfg.GDrive.DailyDumpDir = *gd.DailyDumpDir
			}
			if gd.Threshold != nil {
				cfg.Storage.CircuitBreakerThreshold = *gd.Threshold
			}
			if gd.CooldownMS != nil {
				cfg.Storage.CircuitBreakerCooldown = time.Duration(*gd.CooldownMS) * time.Millisecond
			}
			for k, v := range gd.Folders {
				valSlice := parseFolderInterface(v)
				if len(valSlice) == 0 {
					continue
				}
				switch strings.ToLower(k) {
				case "user_tml", "usertml", "gdrive_usertml_json":
					cfg.GDrive.FolderUserTML = valSlice
				case "ttml", "cached_ttml", "gdrive_cached_ttml":
					cfg.GDrive.FolderTTML = valSlice
				case "spotify", "cached_spotify", "gdrive_cached_spotify":
					cfg.GDrive.FolderSpotify = valSlice
				case "musixmatch", "cached_musixmatch", "gdrive_cached_musixmatch":
					cfg.GDrive.FolderMusixmatch = valSlice
				case "qq", "cached_qq", "gdrive_cached_qq":
					cfg.GDrive.FolderQQ = valSlice
				case "deezer", "cached_deezer", "gdrive_cached_deezer":
					cfg.GDrive.FolderDeezer = valSlice
				case "backup", "backup_folder", "gdrive_backup_folder":
					cfg.GDrive.FolderBackup = valSlice
				}
			}
		}
	}

	// 6. Structured "provider"
	if raw, ok := rawMap["provider"]; ok {
		var prov map[string]json.RawMessage
		if err := json.Unmarshal(raw, &prov); err == nil {
			for pk, pv := range prov {
				kLower := strings.ToLower(pk)
				switch kLower {
				case "timeout_ms":
					var t int
					if err := json.Unmarshal(pv, &t); err == nil {
						cfg.Provider.Timeout = time.Duration(t) * time.Millisecond
					}
				case "spotify_secrets_url":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil {
						cfg.Provider.SpotifySecretsURL = s
						cfg.Provider.SpotifySpotifySecretsURL = s
					}
				case "spotify_accounts":
					var accs []SpotifyAccount
					if err := parseAccountListToSlice(string(pv), &accs); err == nil {
						cfg.Provider.SpotifyAccounts = accs
					}
				case "apple_music_accounts", "apple_accounts":
					var accs []AppleAccount
					if err := parseAccountListToSlice(string(pv), &accs); err == nil {
						cfg.Provider.AppleAccounts = accs
					}
				case "musixmatch_accounts":
					var accs []MusixmatchAccount
					if err := parseAccountListToSlice(string(pv), &accs); err == nil {
						cfg.Provider.MusixmatchAccounts = accs
					}
				case "deezer_accounts":
					var accs []DeezerAccount
					if err := parseAccountListToSlice(string(pv), &accs); err == nil {
						cfg.Provider.DeezerAccounts = accs
					}
				case "qq_cookie":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil {
						cfg.Provider.QQCookie = s
					}
				// Single account scalar fallbacks
				case "spotify_cookie", "spotify_dc_cookie":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil && s != "" && len(cfg.Provider.SpotifyAccounts) == 0 {
						cfg.Provider.SpotifyAccounts = append(cfg.Provider.SpotifyAccounts, SpotifyAccount{
							NAMEID: "spotify-default",
							COOKIE: s,
						})
					}
				case "spotify_client_id":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil && s != "" {
						if len(cfg.Provider.SpotifyAccounts) == 0 {
							cfg.Provider.SpotifyAccounts = append(cfg.Provider.SpotifyAccounts, SpotifyAccount{NAMEID: "spotify-default", CLIENT_ID: s})
						} else {
							cfg.Provider.SpotifyAccounts[0].CLIENT_ID = s
						}
					}
				case "spotify_client_secret":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil && s != "" && len(cfg.Provider.SpotifyAccounts) > 0 {
						cfg.Provider.SpotifyAccounts[0].CLIENT_SECRET = s
					}
				case "musixmatch_cookie":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil && s != "" && len(cfg.Provider.MusixmatchAccounts) == 0 {
						cfg.Provider.MusixmatchAccounts = append(cfg.Provider.MusixmatchAccounts, MusixmatchAccount{
							NAMEID:    "mxm-web",
							AUTH_TYPE: "web",
							COOKIE:    s,
						})
					}
				case "apple_music_auth_token", "apple_music_android_auth_token":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil && s != "" && len(cfg.Provider.AppleAccounts) == 0 {
						cfg.Provider.AppleAccounts = append(cfg.Provider.AppleAccounts, AppleAccount{
							NAMEID:             "apple-default",
							AUTH_TYPE:          "android",
							ANDROID_AUTH_TOKEN: s,
						})
					}
				case "deezer_arl", "deezer_refresh_token":
					var s string
					if err := json.Unmarshal(pv, &s); err == nil && s != "" && len(cfg.Provider.DeezerAccounts) == 0 {
						cfg.Provider.DeezerAccounts = append(cfg.Provider.DeezerAccounts, DeezerAccount{
							NAMEID:        "deezer-default",
							REFRESH_TOKEN: s,
						})
					}
				}
			}
		}
	}

	// 7. Structured "lyricsplus", "ratelimit", "storage", "cache", "logger"
	if raw, ok := rawMap["lyricsplus"]; ok {
		_ = json.Unmarshal(raw, &cfg.LyricsPlus)
	}
	if raw, ok := rawMap["rate_limit"]; ok {
		var rl struct {
			Requests *int `json:"requests"`
			WindowMS *int `json:"window_ms"`
		}
		if err := json.Unmarshal(raw, &rl); err == nil {
			if rl.Requests != nil {
				cfg.RateLimit.Requests = *rl.Requests
			}
			if rl.WindowMS != nil {
				cfg.RateLimit.Window = time.Duration(*rl.WindowMS) * time.Millisecond
			}
		}
	}
	if raw, ok := rawMap["storage"]; ok {
		var st struct {
			DBPath            *string `json:"db_path"`
			SQLitePath        *string `json:"sqlite_path"`
			LRUSize           *int    `json:"lru_size"`
			ContentCacheBytes *int64  `json:"content_cache_bytes"`
			ExactTTLMS        *int    `json:"exact_ttl_ms"`
			ExistingTTLMS     *int    `json:"existing_ttl_ms"`
			NegativeTTLMS     *int    `json:"negative_ttl_ms"`
		}
		if err := json.Unmarshal(raw, &st); err == nil {
			if st.SQLitePath != nil && *st.SQLitePath != "" {
				cfg.Storage.DBPath = *st.SQLitePath
			} else if st.DBPath != nil && *st.DBPath != "" {
				cfg.Storage.DBPath = *st.DBPath
			}
			if st.LRUSize != nil {
				cfg.Storage.LRUSize = *st.LRUSize
			}
			if st.ContentCacheBytes != nil {
				cfg.Storage.ContentCacheBytes = *st.ContentCacheBytes
			}
			if st.ExactTTLMS != nil {
				cfg.Storage.ExactTTL = time.Duration(*st.ExactTTLMS) * time.Millisecond
			}
			if st.ExistingTTLMS != nil {
				cfg.Storage.ExistingTTL = time.Duration(*st.ExistingTTLMS) * time.Millisecond
			}
			if st.NegativeTTLMS != nil {
				cfg.Storage.NegativeTTL = time.Duration(*st.NegativeTTLMS) * time.Millisecond
			}
		}
	}
	if raw, ok := rawMap["logger"]; ok {
		_ = json.Unmarshal(raw, &cfg.Logger)
	}

	return nil
}

func parseFolderInterface(v any) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		return parseCommaSlice(val)
	case []any:
		var res []string
		for _, item := range val {
			if s, ok := item.(string); ok && s != "" {
				res = append(res, strings.TrimSpace(s))
			}
		}
		return res
	case []string:
		return val
	}
	return nil
}

// ============================================================================
// DotEnv Parser
// ============================================================================

func loadDotEnv(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	envMap := make(map[string]string)
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
		envMap[key] = val
	}

	applyMapToConfig(cfg, envMap)
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

// ============================================================================
// Environment Overrides & Mapping
// ============================================================================

func applyEnvOverrides(cfg *Config) {
	envMap := make(map[string]string)
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	applyMapToConfig(cfg, envMap)
}

func applyMapToConfig(cfg *Config, m map[string]string) {
	// Server
	if v, ok := m["PORT"]; ok && v != "" {
		cfg.Server.Addr = v
	}
	if v, ok := m["MAX_CONCURRENCY"]; ok {
		cfg.Server.MaxConcurrency = parseInt(v, cfg.Server.MaxConcurrency)
	}
	if v, ok := m["MAX_URL_BYTES"]; ok {
		cfg.Server.MaxURLBytes = parseInt(v, cfg.Server.MaxURLBytes)
	}
	if v, ok := m["MAX_QUERY_PARAMS"]; ok {
		cfg.Server.MaxQueryParams = parseInt(v, cfg.Server.MaxQueryParams)
	}
	if v, ok := m["MAX_QUERY_VALUE_LEN"]; ok {
		cfg.Server.MaxQueryValueLen = parseInt(v, cfg.Server.MaxQueryValueLen)
	}
	if v, ok := m["SERVER_PROXY_ENABLED"]; ok {
		cfg.Server.Proxy.Enabled = parseBool(v, cfg.Server.Proxy.Enabled)
	}
	if v, ok := m["SERVER_PROXY_URL"]; ok && v != "" {
		cfg.Server.Proxy.URL = v
	}
	if v, ok := m["SERVER_PROXY_URLS"]; ok && v != "" {
		cfg.Server.Proxy.URLs = parseCommaSlice(v)
	}
	if v, ok := m["SERVER_PROXY_TOKEN_HEADER"]; ok && v != "" {
		cfg.Server.Proxy.TokenHeader = v
	}
	if v, ok := m["SERVER_PROXY_TOKEN"]; ok {
		cfg.Server.Proxy.Token = v
	}
	if v, ok := m["SHUTDOWN_TIMEOUT_MS"]; ok {
		cfg.Server.ShutdownTimeout = time.Duration(parseInt(v, int(cfg.Server.ShutdownTimeout/time.Millisecond))) * time.Millisecond
	}

	// Logging
	if v, ok := m["DISABLE_LOGGING"]; ok {
		cfg.Logger.Enabled = !parseBool(v, false)
	}
	if v, ok := m["LOG_LEVEL"]; ok && v != "" {
		cfg.Logger.Level = v
	}
	if v, ok := m["LOG_FORMAT"]; ok && v != "" {
		cfg.Logger.Format = v
	}

	// RateLimit
	if v, ok := m["RATE_LIMIT_REQUESTS"]; ok {
		cfg.RateLimit.Requests = parseInt(v, cfg.RateLimit.Requests)
	}
	if v, ok := m["RATE_LIMIT_WINDOW_MS"]; ok {
		cfg.RateLimit.Window = time.Duration(parseInt(v, int(cfg.RateLimit.Window/time.Millisecond))) * time.Millisecond
	}

	// LyricsPlus
	if v, ok := m["JWT_SECRET"]; ok && v != "" {
		cfg.LyricsPlus.JWTSecret = v
	}
	if v, ok := m["LYRICSPLUS_CHALLENGE_TTL_MS"]; ok {
		cfg.LyricsPlus.ChallengeTTL = time.Duration(parseInt(v, int(cfg.LyricsPlus.ChallengeTTL/time.Millisecond))) * time.Millisecond
	}
	if v, ok := m["LYRICSPLUS_POW_DIFFICULTY"]; ok {
		cfg.LyricsPlus.PoWDifficulty = parseInt(v, cfg.LyricsPlus.PoWDifficulty)
	}
	if v, ok := m["MAX_BODY_BYTES"]; ok {
		cfg.LyricsPlus.MaxBodyBytes = parseInt64(v, cfg.LyricsPlus.MaxBodyBytes)
	}
	if v, ok := m["LYRICSPLUS_ACCEPT_VANDALISM"]; ok {
		cfg.LyricsPlus.AcceptVandalism = parseBool(v, cfg.LyricsPlus.AcceptVandalism)
	}
	if v, ok := m["ALLOW_SUBMISSIONS"]; ok {
		cfg.LyricsPlus.AllowSubmissions = parseBool(v, cfg.LyricsPlus.AllowSubmissions)
	}

	// Storage & Cache
	if v, ok := m["SQLITE_PATH"]; ok && v != "" {
		cfg.Storage.DBPath = v
	} else if v, ok := m["CACHE_DB_PATH"]; ok && v != "" {
		cfg.Storage.DBPath = v
	}
	if v, ok := m["LRU_SIZE"]; ok {
		cfg.Storage.LRUSize = parseInt(v, cfg.Storage.LRUSize)
	}
	if v, ok := m["CONTENT_CACHE_BYTES"]; ok {
		cfg.Storage.ContentCacheBytes = parseInt64(v, cfg.Storage.ContentCacheBytes)
	}
	if v, ok := m["EXACT_TTL_MS"]; ok {
		cfg.Storage.ExactTTL = time.Duration(parseInt(v, int(cfg.Storage.ExactTTL/time.Millisecond))) * time.Millisecond
	}
	if v, ok := m["EXISTING_TTL_MS"]; ok {
		cfg.Storage.ExistingTTL = time.Duration(parseInt(v, int(cfg.Storage.ExistingTTL/time.Millisecond))) * time.Millisecond
	}
	if v, ok := m["NEGATIVE_TTL_MS"]; ok {
		cfg.Storage.NegativeTTL = time.Duration(parseInt(v, int(cfg.Storage.NegativeTTL/time.Millisecond))) * time.Millisecond
	}
	if v, ok := m["HTTP_CACHE_MAX_KEYS"]; ok {
		cfg.Cache.MaxEntries = parseInt(v, cfg.Cache.MaxEntries)
	}
	if v, ok := m["HTTP_CACHE_MAX_BYTES"]; ok {
		cfg.Cache.MaxBytes = parseInt64(v, cfg.Cache.MaxBytes)
	}
	if v, ok := m["HTTP_CACHE_MAX_BODY_BYTES"]; ok {
		cfg.Cache.MaxBodyBytes = parseInt64(v, cfg.Cache.MaxBodyBytes)
	}
	if v, ok := m["MEMORY_WATCHDOG_BYTES"]; ok {
		cfg.Cache.MemoryWatchdogByte = parseInt64(v, cfg.Cache.MemoryWatchdogByte)
	}

	// Google Drive
	if v, ok := m["GDRIVE_ENABLED"]; ok {
		cfg.GDrive.Enabled = parseBool(v, cfg.GDrive.Enabled)
	}
	if v, ok := m["GDRIVE_CIRCUIT_BREAKER_THRESHOLD"]; ok {
		cfg.Storage.CircuitBreakerThreshold = parseInt(v, cfg.Storage.CircuitBreakerThreshold)
	}
	if v, ok := m["GDRIVE_CIRCUIT_BREAKER_COOLDOWN_MS"]; ok {
		cfg.Storage.CircuitBreakerCooldown = time.Duration(parseInt(v, int(cfg.Storage.CircuitBreakerCooldown/time.Millisecond))) * time.Millisecond
	}
	if v, ok := m["DAILY_DUMP_ENABLED"]; ok {
		cfg.GDrive.DailyDumpEnabled = parseBool(v, cfg.GDrive.DailyDumpEnabled)
	}
	if v, ok := m["DAILY_DUMP_INTERVAL_HOURS"]; ok {
		cfg.GDrive.DailyDumpInterval = time.Duration(parseInt(v, int(cfg.GDrive.DailyDumpInterval/time.Hour))) * time.Hour
	}
	if v, ok := m["DAILY_DUMP_DIR"]; ok && v != "" {
		cfg.GDrive.DailyDumpDir = v
	}
	if v, ok := m["GDRIVE_USERTML_JSON"]; ok && v != "" {
		cfg.GDrive.FolderUserTML = parseCommaSlice(v)
	}
	if v, ok := m["GDRIVE_CACHED_TTML"]; ok && v != "" {
		cfg.GDrive.FolderTTML = parseCommaSlice(v)
	}
	if v, ok := m["GDRIVE_CACHED_SPOTIFY"]; ok && v != "" {
		cfg.GDrive.FolderSpotify = parseCommaSlice(v)
	}
	if v, ok := m["GDRIVE_CACHED_MUSIXMATCH"]; ok && v != "" {
		cfg.GDrive.FolderMusixmatch = parseCommaSlice(v)
	}
	if v, ok := m["GDRIVE_CACHED_QQ"]; ok && v != "" {
		cfg.GDrive.FolderQQ = parseCommaSlice(v)
	}
	if v, ok := m["GDRIVE_CACHED_DEEZER"]; ok && v != "" {
		cfg.GDrive.FolderDeezer = parseCommaSlice(v)
	}
	if v, ok := m["GDRIVE_BACKUP_FOLDER"]; ok && v != "" {
		cfg.GDrive.FolderBackup = parseCommaSlice(v)
	}

	// GDrive Accounts
	if raw, ok := m["GDRIVE_ACCOUNTS"]; ok && raw != "" {
		var accounts []GDriveAccount
		if err := parseAccountListToSlice(raw, &accounts); err == nil && len(accounts) > 0 {
			cfg.GDrive.Accounts = accounts
		} else {
			// Try pipe-delimited format: CLIENT_ID|CLIENT_SECRET|REFRESH_TOKEN|ROOT, ...
			accounts = parsePipeGDriveAccounts(raw)
			if len(accounts) > 0 {
				cfg.GDrive.Accounts = accounts
			}
		}
	} else {
		// Single scalar auth credentials
		cid := m["AUTH_KEY_CLIENT_ID"]
		csec := m["AUTH_KEY_CLIENT_SECRET"]
		rtok := m["AUTH_KEY_REFRESH_TOKEN"]
		root := m["AUTH_KEY_ROOT"]
		if cid != "" || rtok != "" {
			cfg.GDrive.Accounts = []GDriveAccount{{
				NAMEID:       "gdrive-env",
				ClientID:     cid,
				ClientSecret: csec,
				RefreshToken: rtok,
				Root:         root,
			}}
		}
	}

	// Provider Tuning
	if v, ok := m["PROVIDER_TIMEOUT_MS"]; ok {
		cfg.Provider.Timeout = time.Duration(parseInt(v, int(cfg.Provider.Timeout/time.Millisecond))) * time.Millisecond
	}
	if v, ok := m["SPOTIFY_SECRETS_URL"]; ok && v != "" {
		cfg.Provider.SpotifySecretsURL = v
		cfg.Provider.SpotifySpotifySecretsURL = v
	}

	// Spotify Accounts
	if raw, ok := m["SPOTIFY_ACCOUNTS"]; ok && raw != "" {
		var accs []SpotifyAccount
		if err := parseAccountListToSlice(raw, &accs); err == nil && len(accs) > 0 {
			cfg.Provider.SpotifyAccounts = accs
		}
	} else if m["SPOTIFY_COOKIE"] != "" || m["SPOTIFY_CLIENT_ID"] != "" {
		cfg.Provider.SpotifyAccounts = []SpotifyAccount{{
			NAMEID:        "spotify-env",
			COOKIE:        m["SPOTIFY_COOKIE"],
			CLIENT_ID:     m["SPOTIFY_CLIENT_ID"],
			CLIENT_SECRET: m["SPOTIFY_CLIENT_SECRET"],
		}}
	}

	// Apple Music Accounts
	if raw, ok := m["APPLE_MUSIC_ACCOUNTS"]; ok && raw != "" {
		var accs []AppleAccount
		if err := parseAccountListToSlice(raw, &accs); err == nil && len(accs) > 0 {
			cfg.Provider.AppleAccounts = accs
		}
	} else if m["APPLE_MUSIC_ANDROID_AUTH_TOKEN"] != "" || m["APPLE_MUSIC_AUTH_TOKEN"] != "" {
		var accs []AppleAccount
		if m["APPLE_MUSIC_ANDROID_AUTH_TOKEN"] != "" {
			accs = append(accs, AppleAccount{
				NAMEID:             "apple-android",
				AUTH_TYPE:          "android",
				ANDROID_AUTH_TOKEN: m["APPLE_MUSIC_ANDROID_AUTH_TOKEN"],
				ANDROID_DSID:       m["APPLE_MUSIC_ANDROID_DSID"],
				ANDROID_USER_AGENT: m["APPLE_MUSIC_ANDROID_USER_AGENT"],
				ANDROID_COOKIE:     m["APPLE_MUSIC_ANDROID_COOKIE"],
				STOREFRONT:         m["APPLE_MUSIC_STOREFRONT"],
			})
		}
		if m["APPLE_MUSIC_AUTH_TOKEN"] != "" {
			accs = append(accs, AppleAccount{
				NAMEID:           "apple-web",
				AUTH_TYPE:        "web",
				MUSIC_AUTH_TOKEN: m["APPLE_MUSIC_AUTH_TOKEN"],
				STOREFRONT:       m["APPLE_MUSIC_STOREFRONT"],
			})
		}
		if len(accs) > 0 {
			cfg.Provider.AppleAccounts = accs
		}
	}

	// Musixmatch Accounts
	if raw, ok := m["MUSIXMATCH_ACCOUNTS"]; ok && raw != "" {
		var accs []MusixmatchAccount
		if err := parseAccountListToSlice(raw, &accs); err == nil && len(accs) > 0 {
			cfg.Provider.MusixmatchAccounts = accs
		}
	} else if m["MUSIXMATCH_COOKIE"] != "" || m["MUSIXMATCH_ANDROID_EMAIL"] != "" {
		var accs []MusixmatchAccount
		if m["MUSIXMATCH_COOKIE"] != "" || m["MUSIXMATCH_USER_AGENT"] != "" {
			accs = append(accs, MusixmatchAccount{
				NAMEID:     "mxm-web",
				AUTH_TYPE:  "web",
				COOKIE:     m["MUSIXMATCH_COOKIE"],
				USER_AGENT: m["MUSIXMATCH_USER_AGENT"],
			})
		}
		if m["MUSIXMATCH_ANDROID_EMAIL"] != "" {
			accs = append(accs, MusixmatchAccount{
				NAMEID:    "mxm-android",
				AUTH_TYPE: "android",
				EMAIL:     m["MUSIXMATCH_ANDROID_EMAIL"],
				PASSWORD:  m["MUSIXMATCH_ANDROID_PASSWORD"],
			})
		}
		if len(accs) > 0 {
			cfg.Provider.MusixmatchAccounts = accs
		}
	}

	// Deezer Accounts
	if raw, ok := m["DEEZER_ACCOUNTS"]; ok && raw != "" {
		var accs []DeezerAccount
		if err := parseAccountListToSlice(raw, &accs); err == nil && len(accs) > 0 {
			cfg.Provider.DeezerAccounts = accs
		}
	} else if m["DEEZER_ARL"] != "" || m["DEEZER_REFRESH_TOKEN"] != "" {
		cfg.Provider.DeezerAccounts = []DeezerAccount{{
			NAMEID:        "deezer-env",
			ARL:           m["DEEZER_ARL"],
			REFRESH_TOKEN: m["DEEZER_REFRESH_TOKEN"],
		}}
	}

	// QQ Music
	if v, ok := m["QQ_COOKIE"]; ok {
		cfg.Provider.QQCookie = v
	}
}

// finalizeConfig sets backward compatibility scalar fields and defaults.
func finalizeConfig(cfg *Config) {
	// Provider aliases from Account 0 if set
	if len(cfg.Provider.SpotifyAccounts) > 0 {
		first := cfg.Provider.SpotifyAccounts[0]
		cfg.Provider.SpotifyClientID = first.CLIENT_ID
		cfg.Provider.SpotifyClientSecret = first.CLIENT_SECRET
		cfg.Provider.SpotifySpotifyDCCookie = first.COOKIE
	}
	if len(cfg.Provider.AppleAccounts) > 0 {
		for _, a := range cfg.Provider.AppleAccounts {
			if strings.EqualFold(a.AUTH_TYPE, "android") && cfg.Provider.AppleAndroidToken == "" {
				cfg.Provider.AppleAndroidToken = a.ANDROID_AUTH_TOKEN
				cfg.Provider.AppleAndroidDsid = a.ANDROID_DSID
				cfg.Provider.AppleAndroidUserAgent = a.ANDROID_USER_AGENT
				cfg.Provider.AppleAndroidCookie = a.ANDROID_COOKIE
				cfg.Provider.AppleStorefront = a.STOREFRONT
			} else if strings.EqualFold(a.AUTH_TYPE, "web") && cfg.Provider.AppleMediaUserToken == "" {
				cfg.Provider.AppleMediaUserToken = a.MUSIC_AUTH_TOKEN
			}
		}
	}
	if len(cfg.Provider.MusixmatchAccounts) > 0 {
		for _, a := range cfg.Provider.MusixmatchAccounts {
			if strings.EqualFold(a.AUTH_TYPE, "android") && cfg.Provider.MusixmatchAndroidEmail == "" {
				cfg.Provider.MusixmatchAndroidEmail = a.EMAIL
				cfg.Provider.MusixmatchAndroidPassword = a.PASSWORD
			} else if cfg.Provider.MusixmatchCookie == "" {
				cfg.Provider.MusixmatchCookie = a.COOKIE
				cfg.Provider.MusixmatchUserAgent = a.USER_AGENT
			}
		}
	}
	if len(cfg.Provider.DeezerAccounts) > 0 {
		first := cfg.Provider.DeezerAccounts[0]
		cfg.Provider.DeezerARL = first.ARL
		cfg.Provider.DeezerRefreshToken = first.REFRESH_TOKEN
	}
}

// parseAccountListToSlice unmarshals a JSON array of accounts, normalizing object
// keys to UPPERCASE so both lowercase ("client_id") and uppercase keys work.
func parseAccountListToSlice[T any](raw string, out *[]T) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("empty accounts payload")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return err
	}
	res := make([]T, 0, len(entries))
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
		res = append(res, item)
	}
	*out = res
	return nil
}

func parsePipeGDriveAccounts(raw string) []GDriveAccount {
	var accounts []GDriveAccount
	items := strings.Split(raw, ",")
	for i, item := range items {
		fields := strings.Split(item, "|")
		if len(fields) >= 3 {
			acc := GDriveAccount{
				NAMEID:       fmt.Sprintf("gdrive-%d", i+1),
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
	return accounts
}

func parseCommaSlice(s string) []string {
	parts := strings.Split(s, ",")
	var res []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			res = append(res, t)
		}
	}
	return res
}

func parseInt(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func parseInt64(s string, def int64) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return n
	}
	return def
}

func parseBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
