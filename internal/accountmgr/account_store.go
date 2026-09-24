// Package accountmgr manages provider and Google Drive accounts in auth.json or config.json.
package accountmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lyricsplus/backend/internal/config"
)

// Store manages provider accounts and preserves arbitrary JSON keys in the config file.
type Store struct {
	filePath string
	rawDoc   map[string]json.RawMessage

	// For .env files: preserved lines (comments + unrelated keys stay intact),
	// plus provider accounts / QQ cookie / GDrive accounts.
	envLines []envLine

	// Provider accounts
	SpotifyAccounts    []config.SpotifyAccount
	AppleAccounts      []config.AppleAccount
	MusixmatchAccounts []config.MusixmatchAccount
	DeezerAccounts     []config.DeezerAccount
	QQCookie           string

	// Google Drive accounts
	GDriveAccounts []config.GDriveAccount
}

// envLine is a single preserved .env line. raw stays byte-for-byte identical
// unless the key is updated, so comments and formatting survive round-trips.
type envLine struct {
	raw string
	key string
	val string
}

// FindConfigFile attempts to locate the default auth/config file.
func FindConfigFile(customPath string) string {
	if customPath != "" {
		return customPath
	}
	candidates := []string{
		"auth.json",
		"data/auth.json",
		"config.json",
		".env",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "auth.json"
}

// IsEnvPath reports whether the path looks like a dotenv file (.env, .env.local, ...).
func IsEnvPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == ".env" || strings.HasPrefix(base, ".env.")
}

// LoadStore reads and parses the configuration file. If the file does not exist,
// it initializes an empty Store for that path. Both JSON auth/config files and
// .env files (provider + GDrive credential keys) are supported.
func LoadStore(path string) (*Store, error) {
	st := &Store{
		filePath: path,
		rawDoc:   make(map[string]json.RawMessage),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return st, nil
	}

	if IsEnvPath(path) {
		st.loadFromEnv(string(data))
		return st, nil
	}

	if err := json.Unmarshal(data, &st.rawDoc); err != nil {
		return nil, fmt.Errorf("parse %s as JSON: %w", path, err)
	}

	st.loadGDrive()
	st.loadProviders()

	return st, nil
}

func (s *Store) loadGDrive() {
	// 1. Check structured gdrive.accounts
	if raw, ok := s.rawDoc["gdrive"]; ok {
		var gd struct {
			Accounts []config.GDriveAccount `json:"accounts"`
		}
		if err := json.Unmarshal(raw, &gd); err == nil && len(gd.Accounts) > 0 {
			s.GDriveAccounts = gd.Accounts
			return
		}
	}

	// 2. Check root-level accounts array
	if raw, ok := s.rawDoc["gdrive_accounts"]; ok {
		var accs []config.GDriveAccount
		if err := json.Unmarshal(raw, &accs); err == nil && len(accs) > 0 {
			s.GDriveAccounts = accs
			return
		}
	}

	// 3. Fallback to root-level scalar credentials
	var rootCreds struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
		Root         string `json:"root"`
	}
	if err := json.Unmarshal(mustMarshal(s.rawDoc), &rootCreds); err == nil {
		if rootCreds.ClientID != "" || rootCreds.RefreshToken != "" {
			s.GDriveAccounts = append(s.GDriveAccounts, config.GDriveAccount{
				ClientID:     rootCreds.ClientID,
				ClientSecret: rootCreds.ClientSecret,
				RefreshToken: rootCreds.RefreshToken,
				Root:         rootCreds.Root,
			})
		}
	}
}

func (s *Store) loadProviders() {
	rawProv, ok := s.rawDoc["provider"]
	if !ok {
		return
	}

	var provMap map[string]json.RawMessage
	if err := json.Unmarshal(rawProv, &provMap); err != nil {
		return
	}

	// Spotify accounts
	if raw, ok := provMap["spotify_accounts"]; ok {
		_ = json.Unmarshal(raw, &s.SpotifyAccounts)
	}
	if len(s.SpotifyAccounts) == 0 {
		var cookie, clientID, clientSecret string
		_ = json.Unmarshal(provMap["spotify_cookie"], &cookie)
		_ = json.Unmarshal(provMap["spotify_client_id"], &clientID)
		_ = json.Unmarshal(provMap["spotify_client_secret"], &clientSecret)
		if cookie != "" || clientID != "" {
			s.SpotifyAccounts = append(s.SpotifyAccounts, config.SpotifyAccount{
				NAMEID:        "spotify-default",
				COOKIE:        cookie,
				CLIENT_ID:     clientID,
				CLIENT_SECRET: clientSecret,
			})
		}
	}

	// Apple Music accounts
	if raw, ok := provMap["apple_music_accounts"]; ok {
		_ = json.Unmarshal(raw, &s.AppleAccounts)
	} else if raw, ok := provMap["apple_accounts"]; ok {
		_ = json.Unmarshal(raw, &s.AppleAccounts)
	}
	if len(s.AppleAccounts) == 0 {
		var andToken, andDsid, andUA, andCookie, sf, webToken string
		_ = json.Unmarshal(provMap["apple_music_android_auth_token"], &andToken)
		_ = json.Unmarshal(provMap["apple_music_android_dsid"], &andDsid)
		_ = json.Unmarshal(provMap["apple_music_android_user_agent"], &andUA)
		_ = json.Unmarshal(provMap["apple_music_android_cookie"], &andCookie)
		_ = json.Unmarshal(provMap["apple_music_storefront"], &sf)
		_ = json.Unmarshal(provMap["apple_music_auth_token"], &webToken)

		if andToken != "" {
			s.AppleAccounts = append(s.AppleAccounts, config.AppleAccount{
				NAMEID:             "apple-android",
				AUTH_TYPE:          "android",
				ANDROID_AUTH_TOKEN: andToken,
				ANDROID_DSID:       andDsid,
				ANDROID_USER_AGENT: andUA,
				ANDROID_COOKIE:     andCookie,
				STOREFRONT:         sf,
			})
		}
		if webToken != "" {
			s.AppleAccounts = append(s.AppleAccounts, config.AppleAccount{
				NAMEID:           "apple-web",
				AUTH_TYPE:        "web",
				MUSIC_AUTH_TOKEN: webToken,
				STOREFRONT:       sf,
			})
		}
	}

	// Musixmatch accounts
	if raw, ok := provMap["musixmatch_accounts"]; ok {
		_ = json.Unmarshal(raw, &s.MusixmatchAccounts)
	}
	if len(s.MusixmatchAccounts) == 0 {
		var cookie, ua, email, pass string
		_ = json.Unmarshal(provMap["musixmatch_cookie"], &cookie)
		_ = json.Unmarshal(provMap["musixmatch_user_agent"], &ua)
		_ = json.Unmarshal(provMap["musixmatch_android_email"], &email)
		_ = json.Unmarshal(provMap["musixmatch_android_password"], &pass)

		if cookie != "" || ua != "" {
			s.MusixmatchAccounts = append(s.MusixmatchAccounts, config.MusixmatchAccount{
				NAMEID:     "mxm-web",
				AUTH_TYPE:  "web",
				USER_AGENT: ua,
				COOKIE:     cookie,
			})
		}
		if email != "" && pass != "" {
			s.MusixmatchAccounts = append(s.MusixmatchAccounts, config.MusixmatchAccount{
				NAMEID:    "mxm-android",
				AUTH_TYPE: "android",
				EMAIL:     email,
				PASSWORD:  pass,
			})
		}
	}

	// Deezer accounts
	if raw, ok := provMap["deezer_accounts"]; ok {
		_ = json.Unmarshal(raw, &s.DeezerAccounts)
	}
	if len(s.DeezerAccounts) == 0 {
		var arl, refToken string
		_ = json.Unmarshal(provMap["deezer_arl"], &arl)
		_ = json.Unmarshal(provMap["deezer_refresh_token"], &refToken)
		if arl != "" || refToken != "" {
			s.DeezerAccounts = append(s.DeezerAccounts, config.DeezerAccount{
				NAMEID:        "deezer-default",
				ARL:           arl,
				REFRESH_TOKEN: refToken,
			})
		}
	}

	// QQ Music cookie
	if raw, ok := provMap["qq_cookie"]; ok {
		_ = json.Unmarshal(raw, &s.QQCookie)
	}
}

// Save serializes the updated accounts back to the file, preserving all other fields.
func (s *Store) Save() error {
	if s.filePath == "" {
		return fmt.Errorf("no file path specified")
	}
	if IsEnvPath(s.filePath) {
		return s.saveEnv()
	}

	// 1. Prepare provider object
	var provMap map[string]json.RawMessage
	if raw, ok := s.rawDoc["provider"]; ok {
		_ = json.Unmarshal(raw, &provMap)
	}
	if provMap == nil {
		provMap = make(map[string]json.RawMessage)
	}

	// Sync Spotify
	provMap["spotify_accounts"] = mustMarshalJSON(s.SpotifyAccounts)
	switch len(s.SpotifyAccounts) {
	case 1:
		first := s.SpotifyAccounts[0]
		provMap["spotify_cookie"] = mustMarshalJSON(first.COOKIE)
		provMap["spotify_client_id"] = mustMarshalJSON(first.CLIENT_ID)
		provMap["spotify_client_secret"] = mustMarshalJSON(first.CLIENT_SECRET)
	case 0:
		delete(provMap, "spotify_cookie")
		delete(provMap, "spotify_client_id")
		delete(provMap, "spotify_client_secret")
	}

	// Sync Apple Music
	provMap["apple_music_accounts"] = mustMarshalJSON(s.AppleAccounts)
	switch len(s.AppleAccounts) {
	case 1:
		acc := s.AppleAccounts[0]
		if strings.EqualFold(acc.AUTH_TYPE, "android") || acc.AUTH_TYPE == "" {
			provMap["apple_music_android_auth_token"] = mustMarshalJSON(acc.ANDROID_AUTH_TOKEN)
			provMap["apple_music_android_dsid"] = mustMarshalJSON(acc.ANDROID_DSID)
			provMap["apple_music_android_user_agent"] = mustMarshalJSON(acc.ANDROID_USER_AGENT)
			provMap["apple_music_android_cookie"] = mustMarshalJSON(acc.ANDROID_COOKIE)
			provMap["apple_music_storefront"] = mustMarshalJSON(acc.STOREFRONT)
		} else {
			provMap["apple_music_auth_token"] = mustMarshalJSON(acc.MUSIC_AUTH_TOKEN)
			provMap["apple_music_storefront"] = mustMarshalJSON(acc.STOREFRONT)
		}
	case 0:
		delete(provMap, "apple_music_android_auth_token")
		delete(provMap, "apple_music_android_dsid")
		delete(provMap, "apple_music_android_user_agent")
		delete(provMap, "apple_music_android_cookie")
		delete(provMap, "apple_music_auth_token")
		delete(provMap, "apple_music_storefront")
	}

	// Sync Musixmatch
	provMap["musixmatch_accounts"] = mustMarshalJSON(s.MusixmatchAccounts)
	switch len(s.MusixmatchAccounts) {
	case 1:
		acc := s.MusixmatchAccounts[0]
		if strings.EqualFold(acc.AUTH_TYPE, "android") {
			provMap["musixmatch_android_email"] = mustMarshalJSON(acc.EMAIL)
			provMap["musixmatch_android_password"] = mustMarshalJSON(acc.PASSWORD)
		} else {
			provMap["musixmatch_cookie"] = mustMarshalJSON(acc.COOKIE)
			provMap["musixmatch_user_agent"] = mustMarshalJSON(acc.USER_AGENT)
		}
	case 0:
		delete(provMap, "musixmatch_android_email")
		delete(provMap, "musixmatch_android_password")
		delete(provMap, "musixmatch_cookie")
		delete(provMap, "musixmatch_user_agent")
	}

	// Sync Deezer
	provMap["deezer_accounts"] = mustMarshalJSON(s.DeezerAccounts)
	switch len(s.DeezerAccounts) {
	case 1:
		first := s.DeezerAccounts[0]
		provMap["deezer_arl"] = mustMarshalJSON(first.ARL)
		provMap["deezer_refresh_token"] = mustMarshalJSON(first.REFRESH_TOKEN)
	case 0:
		delete(provMap, "deezer_arl")
		delete(provMap, "deezer_refresh_token")
	}

	// Sync QQ Music
	if s.QQCookie != "" {
		provMap["qq_cookie"] = mustMarshalJSON(s.QQCookie)
	}

	s.rawDoc["provider"] = mustMarshalJSON(provMap)

	// 2. Prepare GDrive
	gdMap := make(map[string]json.RawMessage)
	if raw, ok := s.rawDoc["gdrive"]; ok {
		if err := json.Unmarshal(raw, &gdMap); err != nil || gdMap == nil {
			gdMap = make(map[string]json.RawMessage)
		}
	}
	if len(s.GDriveAccounts) > 0 {
		gdMap["accounts"] = mustMarshalJSON(s.GDriveAccounts)
	} else {
		delete(gdMap, "accounts")
	}
	if len(gdMap) > 0 {
		s.rawDoc["gdrive"] = mustMarshalJSON(gdMap)
	}
	switch len(s.GDriveAccounts) {
	case 1:
		first := s.GDriveAccounts[0]
		s.rawDoc["client_id"] = mustMarshalJSON(first.ClientID)
		s.rawDoc["client_secret"] = mustMarshalJSON(first.ClientSecret)
		s.rawDoc["refresh_token"] = mustMarshalJSON(first.RefreshToken)
		s.rawDoc["root"] = mustMarshalJSON(first.Root)
	case 0:
		delete(s.rawDoc, "client_id")
		delete(s.rawDoc, "client_secret")
		delete(s.rawDoc, "refresh_token")
		delete(s.rawDoc, "root")
	}

	// 3. Write formatted JSON to file
	dir := filepath.Dir(s.filePath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	formatted, err := json.MarshalIndent(s.rawDoc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	formatted = append(formatted, '\n')

	if err := os.WriteFile(s.filePath, formatted, 0644); err != nil {
		return fmt.Errorf("write %s: %w", s.filePath, err)
	}

	return nil
}

// ============================================================================
// .env support
// ============================================================================

// loadFromEnv parses the dotenv content into ordered lines and fills the
// provider / GDrive accounts from the credential keys it understands.
func (s *Store) loadFromEnv(content string) {
	vals := map[string]string{}
	s.envLines = s.envLines[:0]
	for _, raw := range strings.Split(content, "\n") {
		key, val, ok := parseEnvLine(raw)
		s.envLines = append(s.envLines, envLine{raw: raw, key: key, val: val})
		if ok {
			vals[key] = val
		}
	}
	s.loadGDriveEnv(vals)
	s.loadProvidersEnv(vals)
}

func (s *Store) loadGDriveEnv(vals map[string]string) {
	if accs, ok := parseAccounts[config.GDriveAccount](vals["GDRIVE_ACCOUNTS"]); ok {
		s.GDriveAccounts = accs
		return
	}
	acc := config.GDriveAccount{
		ClientID:     vals["AUTH_KEY_CLIENT_ID"],
		ClientSecret: vals["AUTH_KEY_CLIENT_SECRET"],
		RefreshToken: vals["AUTH_KEY_REFRESH_TOKEN"],
		Root:         vals["AUTH_KEY_ROOT"],
	}
	if acc.ClientID != "" || acc.RefreshToken != "" {
		s.GDriveAccounts = append(s.GDriveAccounts, acc)
	}
}

func (s *Store) loadProvidersEnv(vals map[string]string) {
	// Spotify
	if accs, ok := parseAccounts[config.SpotifyAccount](vals["SPOTIFY_ACCOUNTS"]); ok {
		s.SpotifyAccounts = accs
	} else if vals["SPOTIFY_COOKIE"] != "" || vals["SPOTIFY_CLIENT_ID"] != "" {
		s.SpotifyAccounts = append(s.SpotifyAccounts, config.SpotifyAccount{
			NAMEID:        "spotify-default",
			COOKIE:        vals["SPOTIFY_COOKIE"],
			CLIENT_ID:     vals["SPOTIFY_CLIENT_ID"],
			CLIENT_SECRET: vals["SPOTIFY_CLIENT_SECRET"],
		})
	}

	// Apple Music
	if accs, ok := parseAccounts[config.AppleAccount](vals["APPLE_MUSIC_ACCOUNTS"]); ok {
		s.AppleAccounts = accs
	} else {
		if tok := vals["APPLE_MUSIC_ANDROID_AUTH_TOKEN"]; tok != "" {
			s.AppleAccounts = append(s.AppleAccounts, config.AppleAccount{
				NAMEID:             "apple-android",
				AUTH_TYPE:          "android",
				ANDROID_AUTH_TOKEN: tok,
				ANDROID_DSID:       vals["APPLE_MUSIC_ANDROID_DSID"],
				ANDROID_USER_AGENT: vals["APPLE_MUSIC_ANDROID_USER_AGENT"],
				ANDROID_COOKIE:     vals["APPLE_MUSIC_ANDROID_COOKIE"],
				STOREFRONT:         vals["APPLE_MUSIC_STOREFRONT"],
			})
		}
		if tok := vals["APPLE_MUSIC_AUTH_TOKEN"]; tok != "" {
			s.AppleAccounts = append(s.AppleAccounts, config.AppleAccount{
				NAMEID:           "apple-web",
				AUTH_TYPE:        "web",
				MUSIC_AUTH_TOKEN: tok,
				STOREFRONT:       vals["APPLE_MUSIC_STOREFRONT"],
			})
		}
	}

	// Musixmatch
	if accs, ok := parseAccounts[config.MusixmatchAccount](vals["MUSIXMATCH_ACCOUNTS"]); ok {
		s.MusixmatchAccounts = accs
	} else {
		if vals["MUSIXMATCH_COOKIE"] != "" || vals["MUSIXMATCH_USER_AGENT"] != "" {
			s.MusixmatchAccounts = append(s.MusixmatchAccounts, config.MusixmatchAccount{
				NAMEID:     "mxm-web",
				AUTH_TYPE:  "web",
				COOKIE:     vals["MUSIXMATCH_COOKIE"],
				USER_AGENT: vals["MUSIXMATCH_USER_AGENT"],
			})
		}
		if vals["MUSIXMATCH_ANDROID_EMAIL"] != "" && vals["MUSIXMATCH_ANDROID_PASSWORD"] != "" {
			s.MusixmatchAccounts = append(s.MusixmatchAccounts, config.MusixmatchAccount{
				NAMEID:    "mxm-android",
				AUTH_TYPE: "android",
				EMAIL:     vals["MUSIXMATCH_ANDROID_EMAIL"],
				PASSWORD:  vals["MUSIXMATCH_ANDROID_PASSWORD"],
			})
		}
	}

	// Deezer
	if accs, ok := parseAccounts[config.DeezerAccount](vals["DEEZER_ACCOUNTS"]); ok {
		s.DeezerAccounts = accs
	} else if vals["DEEZER_ARL"] != "" || vals["DEEZER_REFRESH_TOKEN"] != "" {
		s.DeezerAccounts = append(s.DeezerAccounts, config.DeezerAccount{
			NAMEID:        "deezer-default",
			ARL:           vals["DEEZER_ARL"],
			REFRESH_TOKEN: vals["DEEZER_REFRESH_TOKEN"],
		})
	}

	s.QQCookie = vals["QQ_COOKIE"]
}

// envArray marshals an account slice for a .env value, normalizing nil slices
// to "[]" instead of "null" so the line reads cleanly.
func envArray(v any) string {
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return "[]"
	}
	return string(b)
}

// saveEnv writes the provider and GDrive credential keys back into the .env
// file in place, preserving comments and unrelated keys.
func (s *Store) saveEnv() error {
	// Spotify
	s.envSet("SPOTIFY_ACCOUNTS", envArray(s.SpotifyAccounts))
	switch len(s.SpotifyAccounts) {
	case 1:
		s.envSet("SPOTIFY_COOKIE", s.SpotifyAccounts[0].COOKIE)
		s.envSet("SPOTIFY_CLIENT_ID", s.SpotifyAccounts[0].CLIENT_ID)
		s.envSet("SPOTIFY_CLIENT_SECRET", s.SpotifyAccounts[0].CLIENT_SECRET)
	case 0:
		s.envSet("SPOTIFY_COOKIE", "")
		s.envSet("SPOTIFY_CLIENT_ID", "")
		s.envSet("SPOTIFY_CLIENT_SECRET", "")
	}

	// Apple Music
	s.envSet("APPLE_MUSIC_ACCOUNTS", envArray(s.AppleAccounts))
	switch len(s.AppleAccounts) {
	case 1:
		acc := s.AppleAccounts[0]
		if strings.EqualFold(acc.AUTH_TYPE, "web") {
			s.envSet("APPLE_MUSIC_AUTH_TOKEN", acc.MUSIC_AUTH_TOKEN)
			s.envSet("APPLE_MUSIC_STOREFRONT", acc.STOREFRONT)
		} else {
			s.envSet("APPLE_MUSIC_ANDROID_AUTH_TOKEN", acc.ANDROID_AUTH_TOKEN)
			s.envSet("APPLE_MUSIC_ANDROID_DSID", acc.ANDROID_DSID)
			s.envSet("APPLE_MUSIC_ANDROID_USER_AGENT", acc.ANDROID_USER_AGENT)
			s.envSet("APPLE_MUSIC_ANDROID_COOKIE", acc.ANDROID_COOKIE)
			s.envSet("APPLE_MUSIC_STOREFRONT", acc.STOREFRONT)
		}
	case 0:
		s.envSet("APPLE_MUSIC_AUTH_TOKEN", "")
		s.envSet("APPLE_MUSIC_ANDROID_AUTH_TOKEN", "")
		s.envSet("APPLE_MUSIC_ANDROID_DSID", "")
		s.envSet("APPLE_MUSIC_ANDROID_USER_AGENT", "")
		s.envSet("APPLE_MUSIC_ANDROID_COOKIE", "")
		s.envSet("APPLE_MUSIC_STOREFRONT", "")
	}

	// Musixmatch
	s.envSet("MUSIXMATCH_ACCOUNTS", envArray(s.MusixmatchAccounts))
	switch len(s.MusixmatchAccounts) {
	case 1:
		acc := s.MusixmatchAccounts[0]
		if strings.EqualFold(acc.AUTH_TYPE, "android") {
			s.envSet("MUSIXMATCH_ANDROID_EMAIL", acc.EMAIL)
			s.envSet("MUSIXMATCH_ANDROID_PASSWORD", acc.PASSWORD)
		} else {
			s.envSet("MUSIXMATCH_COOKIE", acc.COOKIE)
			s.envSet("MUSIXMATCH_USER_AGENT", acc.USER_AGENT)
		}
	case 0:
		s.envSet("MUSIXMATCH_COOKIE", "")
		s.envSet("MUSIXMATCH_USER_AGENT", "")
		s.envSet("MUSIXMATCH_ANDROID_EMAIL", "")
		s.envSet("MUSIXMATCH_ANDROID_PASSWORD", "")
	}

	// Deezer
	s.envSet("DEEZER_ACCOUNTS", envArray(s.DeezerAccounts))
	switch len(s.DeezerAccounts) {
	case 1:
		s.envSet("DEEZER_ARL", s.DeezerAccounts[0].ARL)
		s.envSet("DEEZER_REFRESH_TOKEN", s.DeezerAccounts[0].REFRESH_TOKEN)
	case 0:
		s.envSet("DEEZER_ARL", "")
		s.envSet("DEEZER_REFRESH_TOKEN", "")
	}

	// QQ Music
	s.envSet("QQ_COOKIE", s.QQCookie)

	// Google Drive
	s.envSet("GDRIVE_ACCOUNTS", envArray(s.GDriveAccounts))
	switch len(s.GDriveAccounts) {
	case 1:
		s.envSet("AUTH_KEY_CLIENT_ID", s.GDriveAccounts[0].ClientID)
		s.envSet("AUTH_KEY_CLIENT_SECRET", s.GDriveAccounts[0].ClientSecret)
		s.envSet("AUTH_KEY_REFRESH_TOKEN", s.GDriveAccounts[0].RefreshToken)
		s.envSet("AUTH_KEY_ROOT", s.GDriveAccounts[0].Root)
	case 0:
		s.envSet("AUTH_KEY_CLIENT_ID", "")
		s.envSet("AUTH_KEY_CLIENT_SECRET", "")
		s.envSet("AUTH_KEY_REFRESH_TOKEN", "")
		s.envSet("AUTH_KEY_ROOT", "")
	}

	dir := filepath.Dir(s.filePath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	var b strings.Builder
	for _, l := range s.envLines {
		b.WriteString(l.raw)
		b.WriteString("\n")
	}
	if err := os.WriteFile(s.filePath, []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("write %s: %w", s.filePath, err)
	}
	return nil
}

// envSet updates an existing key in place, or appends a new line at the end.
// It always rewrites the line, so values that contain spaces / # / quotes get
// safely double-quoted for reliable re-parsing.
func (s *Store) envSet(key, val string) {
	for i := range s.envLines {
		if strings.EqualFold(s.envLines[i].key, key) {
			s.envLines[i].key = key
			s.envLines[i].val = val
			s.envLines[i].raw = key + "=" + encodeEnvValue(val)
			return
		}
	}
	s.envLines = append(s.envLines, envLine{key: key, val: val, raw: key + "=" + encodeEnvValue(val)})
}

// parseEnvLine parses a single .env line into key/value using the same rules as
// the server's dotenv loader: export prefix, optional quotes, inline comments.
func parseEnvLine(raw string) (key, val string, ok bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}
	eq := strings.Index(line, "=")
	if eq < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:eq])
	if key == "" {
		return "", "", false
	}
	val = strings.TrimSpace(line[eq+1:])

	if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
		quote := val[0]
		val = val[1 : len(val)-1]
		if quote == '"' {
			val = unescapeEnvValue(val)
		}
	} else if cIdx := strings.Index(val, " #"); cIdx >= 0 {
		val = strings.TrimSpace(val[:cIdx])
	}
	return key, val, true
}

func unescapeEnvValue(s string) string {
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\t`, "\t")
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

func encodeEnvValue(v string) string {
	if strings.ContainsAny(v, " #\"'\t\n\r\\") {
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range v {
			switch r {
			case '\\':
				b.WriteString(`\\`)
			case '"':
				b.WriteString(`\"`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
		return b.String()
	}
	return v
}

// parseAccounts decodes a JSON array of provider accounts, normalizing keys to
// UPPERCASE so lowercase / JS-style credentials still load reliably.
func parseAccounts[T any](raw string) ([]T, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || len(entries) == 0 {
		return nil, false
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
		return nil, false
	}
	return out, true
}

// FilePath returns the file path of the store.
func (s *Store) FilePath() string {
	return s.filePath
}

// SetFilePath updates the target file path.
func (s *Store) SetFilePath(p string) {
	s.filePath = p
}

// MaskCredential returns a visually masked version of a token/cookie.
func MaskCredential(val string) string {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return "<empty>"
	}
	if len(trimmed) <= 12 {
		return "****"
	}
	prefix := trimmed[:6]
	suffix := trimmed[len(trimmed)-4:]
	return fmt.Sprintf("%s...%s (%d chars)", prefix, suffix, len(trimmed))
}

// CleanCookie removes surrounding quotes, extra whitespace, or standardizes cookie strings.
func CleanCookie(val string) string {
	s := strings.TrimSpace(val)
	s = strings.TrimPrefix(s, "Cookie: ")
	s = strings.TrimPrefix(s, "cookie: ")
	s = strings.Trim(s, "\"'`")
	return strings.TrimSpace(s)
}

// ExtractSpDc extracts the sp_dc value from a full Cookie header if present.
func ExtractSpDc(cookieHeader string) string {
	cleaned := CleanCookie(cookieHeader)
	for _, part := range strings.Split(cleaned, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "sp_dc=") {
			return strings.TrimPrefix(part, "sp_dc=")
		}
	}
	return cleaned
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func mustMarshalJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ConvertEnvToJSON reads configuration from a .env file and writes a structured JSON configuration file.
func ConvertEnvToJSON(envPath, jsonPath string) error {
	if envPath == "" {
		envPath = ".env"
	}
	if jsonPath == "" {
		jsonPath = "config.json"
	}

	cfg := config.Load(envPath)

	type ServerConfig struct {
		Port              string   `json:"port"`
		MaxConcurrency    int      `json:"max_concurrency,omitempty"`
		MaxURLBytes       int      `json:"max_url_bytes,omitempty"`
		MaxQueryParams    int      `json:"max_query_params,omitempty"`
		MaxQueryValueLen  int      `json:"max_query_value_len,omitempty"`
		ProxyEnabled      bool     `json:"proxy_enabled"`
		ProxyURL          string   `json:"proxy_url,omitempty"`
		ProxyURLs         []string `json:"proxy_urls,omitempty"`
		ProxyTokenHeader  string   `json:"proxy_token_header,omitempty"`
		ProxyToken        string   `json:"proxy_token,omitempty"`
		ShutdownTimeoutMS int64    `json:"shutdown_timeout_ms,omitempty"`
	}

	type GDriveConfig struct {
		Enabled           bool                   `json:"enabled"`
		Accounts          []config.GDriveAccount `json:"accounts,omitempty"`
		Folders           map[string]string      `json:"folders,omitempty"`
		DailyDumpEnabled  bool                   `json:"daily_dump_enabled,omitempty"`
		DailyDumpInterval int                    `json:"daily_dump_interval_hours,omitempty"`
		DailyDumpDir      string                 `json:"daily_dump_dir,omitempty"`
	}

	type ProviderConfig struct {
		TimeoutMS          int64                      `json:"timeout_ms,omitempty"`
		SpotifySecretsURL  string                     `json:"spotify_secrets_url,omitempty"`
		SpotifyAccounts    []config.SpotifyAccount    `json:"spotify_accounts,omitempty"`
		AppleAccounts      []config.AppleAccount      `json:"apple_music_accounts,omitempty"`
		MusixmatchAccounts []config.MusixmatchAccount `json:"musixmatch_accounts,omitempty"`
		DeezerAccounts     []config.DeezerAccount     `json:"deezer_accounts,omitempty"`
		QQCookie           string                     `json:"qq_cookie,omitempty"`
	}

	type LyricsPlusConfig struct {
		JWTSecret        string `json:"jwt_secret"`
		ChallengeTTLMS   int64  `json:"challenge_ttl_ms,omitempty"`
		PoWDifficulty    int    `json:"pow_difficulty,omitempty"`
		MaxBodyBytes     int64  `json:"max_body_bytes,omitempty"`
		AcceptVandalism  bool   `json:"accept_vandalism"`
		AllowSubmissions bool   `json:"allow_submissions"`
	}

	type RateLimitConfig struct {
		Requests int   `json:"requests,omitempty"`
		WindowMS int64 `json:"window_ms,omitempty"`
	}

	type StorageConfig struct {
		SQLitePath              string `json:"sqlite_path,omitempty"`
		LRUSize                 int    `json:"lru_size,omitempty"`
		ContentCacheBytes       int64  `json:"content_cache_bytes,omitempty"`
		ExactTTLMS              int64  `json:"exact_ttl_ms,omitempty"`
		ExistingTTLMS           int64  `json:"existing_ttl_ms,omitempty"`
		NegativeTTLMS           int64  `json:"negative_ttl_ms,omitempty"`
		CircuitBreakerThreshold int    `json:"circuit_breaker_threshold,omitempty"`
		CircuitBreakerCooldown  int64  `json:"circuit_breaker_cooldown_ms,omitempty"`
	}

	type CacheConfig struct {
		MaxEntries         int   `json:"max_entries,omitempty"`
		MaxBytes           int64 `json:"max_bytes,omitempty"`
		MaxBodyBytes       int64 `json:"max_body_bytes,omitempty"`
		MemoryWatchdogByte int64 `json:"memory_watchdog_bytes,omitempty"`
	}

	type LoggerConfig struct {
		Enabled bool   `json:"enabled"`
		Level   string `json:"level,omitempty"`
		Format  string `json:"format,omitempty"`
	}

	folders := make(map[string]string)
	if len(cfg.GDrive.FolderUserTML) > 0 {
		folders["user_tml"] = strings.Join(cfg.GDrive.FolderUserTML, ",")
	}
	if len(cfg.GDrive.FolderTTML) > 0 {
		folders["ttml"] = strings.Join(cfg.GDrive.FolderTTML, ",")
	}
	if len(cfg.GDrive.FolderSpotify) > 0 {
		folders["spotify"] = strings.Join(cfg.GDrive.FolderSpotify, ",")
	}
	if len(cfg.GDrive.FolderMusixmatch) > 0 {
		folders["musixmatch"] = strings.Join(cfg.GDrive.FolderMusixmatch, ",")
	}
	if len(cfg.GDrive.FolderQQ) > 0 {
		folders["qq"] = strings.Join(cfg.GDrive.FolderQQ, ",")
	}
	if len(cfg.GDrive.FolderDeezer) > 0 {
		folders["deezer"] = strings.Join(cfg.GDrive.FolderDeezer, ",")
	}
	if len(cfg.GDrive.FolderBackup) > 0 {
		folders["backup"] = strings.Join(cfg.GDrive.FolderBackup, ",")
	}

	doc := struct {
		Server     ServerConfig     `json:"server"`
		GDrive     GDriveConfig     `json:"gdrive"`
		Provider   ProviderConfig   `json:"provider"`
		LyricsPlus LyricsPlusConfig `json:"lyricsplus"`
		RateLimit  RateLimitConfig  `json:"rate_limit"`
		Storage    StorageConfig    `json:"storage"`
		Cache      CacheConfig      `json:"cache"`
		Logger     LoggerConfig     `json:"logger"`
	}{
		Server: ServerConfig{
			Port:              cfg.Server.Addr,
			MaxConcurrency:    cfg.Server.MaxConcurrency,
			MaxURLBytes:       cfg.Server.MaxURLBytes,
			MaxQueryParams:    cfg.Server.MaxQueryParams,
			MaxQueryValueLen:  cfg.Server.MaxQueryValueLen,
			ProxyEnabled:      cfg.Server.Proxy.Enabled,
			ProxyURL:          cfg.Server.Proxy.URL,
			ProxyURLs:         cfg.Server.Proxy.URLs,
			ProxyTokenHeader:  cfg.Server.Proxy.TokenHeader,
			ProxyToken:        cfg.Server.Proxy.Token,
			ShutdownTimeoutMS: cfg.Server.ShutdownTimeout.Milliseconds(),
		},
		GDrive: GDriveConfig{
			Enabled:           cfg.GDrive.Enabled,
			Accounts:          cfg.GDrive.Accounts,
			Folders:           folders,
			DailyDumpEnabled:  cfg.GDrive.DailyDumpEnabled,
			DailyDumpInterval: int(cfg.GDrive.DailyDumpInterval / time.Hour),
			DailyDumpDir:      cfg.GDrive.DailyDumpDir,
		},
		Provider: ProviderConfig{
			TimeoutMS:          cfg.Provider.Timeout.Milliseconds(),
			SpotifySecretsURL:  cfg.Provider.SpotifySecretsURL,
			SpotifyAccounts:    cfg.Provider.SpotifyAccounts,
			AppleAccounts:      cfg.Provider.AppleAccounts,
			MusixmatchAccounts: cfg.Provider.MusixmatchAccounts,
			DeezerAccounts:     cfg.Provider.DeezerAccounts,
			QQCookie:           cfg.Provider.QQCookie,
		},
		LyricsPlus: LyricsPlusConfig{
			JWTSecret:        cfg.LyricsPlus.JWTSecret,
			ChallengeTTLMS:   cfg.LyricsPlus.ChallengeTTL.Milliseconds(),
			PoWDifficulty:    cfg.LyricsPlus.PoWDifficulty,
			MaxBodyBytes:     cfg.LyricsPlus.MaxBodyBytes,
			AcceptVandalism:  cfg.LyricsPlus.AcceptVandalism,
			AllowSubmissions: cfg.LyricsPlus.AllowSubmissions,
		},
		RateLimit: RateLimitConfig{
			Requests: cfg.RateLimit.Requests,
			WindowMS: cfg.RateLimit.Window.Milliseconds(),
		},
		Storage: StorageConfig{
			SQLitePath:              cfg.Storage.DBPath,
			LRUSize:                 cfg.Storage.LRUSize,
			ContentCacheBytes:       cfg.Storage.ContentCacheBytes,
			ExactTTLMS:              cfg.Storage.ExactTTL.Milliseconds(),
			ExistingTTLMS:           cfg.Storage.ExistingTTL.Milliseconds(),
			NegativeTTLMS:           cfg.Storage.NegativeTTL.Milliseconds(),
			CircuitBreakerThreshold: cfg.Storage.CircuitBreakerThreshold,
			CircuitBreakerCooldown:  cfg.Storage.CircuitBreakerCooldown.Milliseconds(),
		},
		Cache: CacheConfig{
			MaxEntries:         cfg.Cache.MaxEntries,
			MaxBytes:           cfg.Cache.MaxBytes,
			MaxBodyBytes:       cfg.Cache.MaxBodyBytes,
			MemoryWatchdogByte: cfg.Cache.MemoryWatchdogByte,
		},
		Logger: LoggerConfig{
			Enabled: cfg.Logger.Enabled,
			Level:   cfg.Logger.Level,
			Format:  cfg.Logger.Format,
		},
	}

	dir := filepath.Dir(jsonPath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	formatted, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	formatted = append(formatted, '\n')

	return os.WriteFile(jsonPath, formatted, 0644)
}
