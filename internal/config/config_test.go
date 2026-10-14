package config

import (
	"os"
	"path/filepath"
	"testing"
)

func clearTestEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_ = os.Unsetenv(k)
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	content := `
# Comment line
PORT=4000
export SPOTIFY_CLIENT_ID=test-spotify-id
SPOTIFY_COOKIE="test-cookie\nwith-newline"
EMPTY_VAR=
WITH_INLINE_COMMENT=val123 # this is comment
EXISTING_VAR=new_val
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatalf("write temp .env: %v", err)
	}

	clearTestEnv(t, "PORT", "SPOTIFY_CLIENT_ID", "SPOTIFY_COOKIE", "EMPTY_VAR", "WITH_INLINE_COMMENT", "EXISTING_VAR")

	cfg := Default()
	if err := loadDotEnv(&cfg, envPath); err != nil {
		t.Fatalf("loadDotEnv failed: %v", err)
	}

	if cfg.Server.Addr != "4000" {
		t.Errorf("expected PORT=4000, got %q", cfg.Server.Addr)
	}
	if len(cfg.Provider.SpotifyAccounts) != 1 {
		t.Fatalf("expected 1 spotify account, got %d", len(cfg.Provider.SpotifyAccounts))
	}
	if cfg.Provider.SpotifyAccounts[0].CLIENT_ID != "test-spotify-id" {
		t.Errorf("expected CLIENT_ID=test-spotify-id, got %q", cfg.Provider.SpotifyAccounts[0].CLIENT_ID)
	}
	if cfg.Provider.SpotifyAccounts[0].COOKIE != "test-cookie\nwith-newline" {
		t.Errorf("expected COOKIE with newline, got %q", cfg.Provider.SpotifyAccounts[0].COOKIE)
	}
}

func TestLoadJSONConfig_OAuthRoot(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "auth.json")

	content := `{
		"client_id": "oauth-client-id",
		"client_secret": "oauth-client-secret",
		"refresh_token": "oauth-refresh-token",
		"root": "oauth-root-id",
		"jwt_secret": "custom-jwt-secret",
		"port": "5000"
	}`

	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	cfg := Default()
	if err := loadJSONConfig(&cfg, jsonPath); err != nil {
		t.Fatalf("loadJSONConfig failed: %v", err)
	}

	if len(cfg.GDrive.Accounts) != 1 {
		t.Fatalf("expected 1 gdrive account, got %d", len(cfg.GDrive.Accounts))
	}
	if cfg.GDrive.Accounts[0].ClientID != "oauth-client-id" {
		t.Errorf("expected ClientID=oauth-client-id, got %q", cfg.GDrive.Accounts[0].ClientID)
	}
	if cfg.GDrive.Accounts[0].ClientSecret != "oauth-client-secret" {
		t.Errorf("expected ClientSecret=oauth-client-secret, got %q", cfg.GDrive.Accounts[0].ClientSecret)
	}
	if cfg.GDrive.Accounts[0].RefreshToken != "oauth-refresh-token" {
		t.Errorf("expected RefreshToken=oauth-refresh-token, got %q", cfg.GDrive.Accounts[0].RefreshToken)
	}
	if cfg.GDrive.Accounts[0].Root != "oauth-root-id" {
		t.Errorf("expected Root=oauth-root-id, got %q", cfg.GDrive.Accounts[0].Root)
	}
	if cfg.LyricsPlus.JWTSecret != "custom-jwt-secret" {
		t.Errorf("expected JWTSecret=custom-jwt-secret, got %q", cfg.LyricsPlus.JWTSecret)
	}
	if cfg.Server.Addr != "5000" {
		t.Errorf("expected PORT=5000, got %q", cfg.Server.Addr)
	}
}

func TestLoadJSONConfig_Structured(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "auth.json")

	content := `{
		"gdrive": {
			"enabled": true,
			"folders": {
				"ttml": "folder-ttml-1,folder-ttml-2",
				"spotify": "folder-spotify"
			}
		},
		"provider": {
			"spotify_accounts": [
				{ "NAMEID": "sp-1", "CLIENT_ID": "sp-id-123", "COOKIE": "sp-cookie-abc" }
			],
			"musixmatch_accounts": [
				{ "NAMEID": "mxm-1", "AUTH_TYPE": "web", "COOKIE": "mxm-cookie-xyz" }
			],
			"apple_music_accounts": [
				{ "NAMEID": "apple-1", "AUTH_TYPE": "android", "ANDROID_AUTH_TOKEN": "apple-token-123" }
			]
		}
	}`

	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	cfg := Default()
	if err := loadJSONConfig(&cfg, jsonPath); err != nil {
		t.Fatalf("loadJSONConfig failed: %v", err)
	}
	finalizeConfig(&cfg)

	if !cfg.GDrive.Enabled {
		t.Errorf("expected GDRIVE.Enabled=true, got false")
	}
	if len(cfg.GDrive.FolderTTML) != 2 || cfg.GDrive.FolderTTML[0] != "folder-ttml-1" {
		t.Errorf("expected FolderTTML=[folder-ttml-1 folder-ttml-2], got %+v", cfg.GDrive.FolderTTML)
	}
	if len(cfg.GDrive.FolderSpotify) != 1 || cfg.GDrive.FolderSpotify[0] != "folder-spotify" {
		t.Errorf("expected FolderSpotify=[folder-spotify], got %+v", cfg.GDrive.FolderSpotify)
	}
	if len(cfg.Provider.SpotifyAccounts) != 1 || cfg.Provider.SpotifyAccounts[0].CLIENT_ID != "sp-id-123" {
		t.Errorf("expected SpotifyAccount CLIENT_ID=sp-id-123, got %+v", cfg.Provider.SpotifyAccounts)
	}
	if len(cfg.Provider.MusixmatchAccounts) != 1 || cfg.Provider.MusixmatchAccounts[0].COOKIE != "mxm-cookie-xyz" {
		t.Errorf("expected MusixmatchAccount COOKIE=mxm-cookie-xyz, got %+v", cfg.Provider.MusixmatchAccounts)
	}
	if len(cfg.Provider.AppleAccounts) != 1 || cfg.Provider.AppleAccounts[0].ANDROID_AUTH_TOKEN != "apple-token-123" {
		t.Errorf("expected AppleAccount ANDROID_AUTH_TOKEN=apple-token-123, got %+v", cfg.Provider.AppleAccounts)
	}
}

func TestLoadJSONConfig_GCPInstalled(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "client_secret.json")

	content := `{
		"installed": {
			"client_id": "gcp-client-id",
			"client_secret": "gcp-client-secret",
			"refresh_token": "gcp-refresh-token"
		}
	}`

	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("write client_secret.json: %v", err)
	}

	cfg := Default()
	if err := loadJSONConfig(&cfg, jsonPath); err != nil {
		t.Fatalf("loadJSONConfig failed: %v", err)
	}

	if len(cfg.GDrive.Accounts) != 1 {
		t.Fatalf("expected 1 GDrive account, got %d", len(cfg.GDrive.Accounts))
	}
	if cfg.GDrive.Accounts[0].ClientID != "gcp-client-id" {
		t.Errorf("expected ClientID=gcp-client-id, got %q", cfg.GDrive.Accounts[0].ClientID)
	}
	if cfg.GDrive.Accounts[0].ClientSecret != "gcp-client-secret" {
		t.Errorf("expected ClientSecret=gcp-client-secret, got %q", cfg.GDrive.Accounts[0].ClientSecret)
	}
	if cfg.GDrive.Accounts[0].RefreshToken != "gcp-refresh-token" {
		t.Errorf("expected RefreshToken=gcp-refresh-token, got %q", cfg.GDrive.Accounts[0].RefreshToken)
	}
}

func TestLoad_WithCustomPath(t *testing.T) {
	dir := t.TempDir()
	customPath := filepath.Join(dir, "my_custom.env")

	if err := os.WriteFile(customPath, []byte("PORT=9999\nAUTH_KEY_CLIENT_ID=custom-client\nAUTH_KEY_REFRESH_TOKEN=custom-token\n"), 0644); err != nil {
		t.Fatalf("write custom env: %v", err)
	}

	clearTestEnv(t, "PORT", "AUTH_KEY_CLIENT_ID", "AUTH_KEY_CLIENT_SECRET", "AUTH_KEY_REFRESH_TOKEN", "GDRIVE_ACCOUNTS")

	cfg := Load(customPath)

	if cfg.Server.Addr != "9999" {
		t.Errorf("expected cfg.Server.Addr=9999, got %q", cfg.Server.Addr)
	}
	if len(cfg.GDrive.Accounts) != 1 || cfg.GDrive.Accounts[0].ClientID != "custom-client" {
		t.Errorf("expected account clientID=custom-client, got %+v", cfg.GDrive.Accounts)
	}
}

func TestLoad_DefaultsSafeAndClean(t *testing.T) {
	clearTestEnv(t,
		"PORT", "MAX_CONCURRENCY",
		"AUTH_KEY_CLIENT_ID", "AUTH_KEY_CLIENT_SECRET", "AUTH_KEY_REFRESH_TOKEN", "AUTH_KEY_ROOT", "GDRIVE_ACCOUNTS",
		"SPOTIFY_COOKIE", "SPOTIFY_CLIENT_ID", "SPOTIFY_CLIENT_SECRET",
		"MUSIXMATCH_COOKIE", "MUSIXMATCH_ANDROID_EMAIL", "MUSIXMATCH_ANDROID_PASSWORD",
		"APPLE_MUSIC_ANDROID_AUTH_TOKEN", "APPLE_MUSIC_AUTH_TOKEN",
	)

	cfg := Load(filepath.Join(t.TempDir(), "empty.env"))

	if cfg.Server.Addr != "3000" {
		t.Errorf("expected default PORT=3000, got %q", cfg.Server.Addr)
	}
	if cfg.Provider.SpotifySpotifyDCCookie != "" {
		t.Errorf("expected empty Spotify cookie, got %q", cfg.Provider.SpotifySpotifyDCCookie)
	}
	if cfg.Provider.SpotifyClientID != "" {
		t.Errorf("expected empty Spotify client id, got %q", cfg.Provider.SpotifyClientID)
	}
	if cfg.Provider.MusixmatchCookie != "" {
		t.Errorf("expected empty Musixmatch cookie, got %q", cfg.Provider.MusixmatchCookie)
	}
	if cfg.Provider.AppleAndroidToken != "" {
		t.Errorf("expected empty Apple Android token, got %q", cfg.Provider.AppleAndroidToken)
	}
	if len(cfg.GDrive.Accounts) != 0 {
		t.Errorf("expected no GDrive accounts, got %+v", cfg.GDrive.Accounts)
	}
}

func TestLoad_MultiAccountEnvArrays(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	content := `
SPOTIFY_ACCOUNTS='[{"NAMEID":"sp-primary","COOKIE":"cookie-1"},{"NAMEID":"sp-secondary","COOKIE":"cookie-2"}]'
APPLE_MUSIC_ACCOUNTS='[{"NAMEID":"apple-android","AUTH_TYPE":"android","ANDROID_AUTH_TOKEN":"tok1"},{"NAMEID":"apple-web","AUTH_TYPE":"web","MUSIC_AUTH_TOKEN":"tok2"}]'
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	clearTestEnv(t, "SPOTIFY_ACCOUNTS", "APPLE_MUSIC_ACCOUNTS")

	cfg := Load(envPath)
	if len(cfg.Provider.SpotifyAccounts) != 2 {
		t.Fatalf("expected 2 Spotify accounts, got %d", len(cfg.Provider.SpotifyAccounts))
	}
	if cfg.Provider.SpotifyAccounts[0].NAMEID != "sp-primary" || cfg.Provider.SpotifyAccounts[1].NAMEID != "sp-secondary" {
		t.Errorf("unexpected spotify accounts: %+v", cfg.Provider.SpotifyAccounts)
	}
	if len(cfg.Provider.AppleAccounts) != 2 {
		t.Fatalf("expected 2 Apple accounts, got %d", len(cfg.Provider.AppleAccounts))
	}
	if cfg.Provider.AppleAccounts[0].ANDROID_AUTH_TOKEN != "tok1" || cfg.Provider.AppleAccounts[1].MUSIC_AUTH_TOKEN != "tok2" {
		t.Errorf("unexpected apple accounts: %+v", cfg.Provider.AppleAccounts)
	}
}
