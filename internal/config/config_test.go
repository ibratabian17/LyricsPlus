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
	_ = os.Setenv("EXISTING_VAR", "already_set")

	if err := loadDotEnv(envPath); err != nil {
		t.Fatalf("loadDotEnv failed: %v", err)
	}

	if val := os.Getenv("PORT"); val != "4000" {
		t.Errorf("expected PORT=4000, got %q", val)
	}
	if val := os.Getenv("SPOTIFY_CLIENT_ID"); val != "test-spotify-id" {
		t.Errorf("expected SPOTIFY_CLIENT_ID=test-spotify-id, got %q", val)
	}
	if val := os.Getenv("SPOTIFY_COOKIE"); val != "test-cookie\nwith-newline" {
		t.Errorf("expected SPOTIFY_COOKIE with newline, got %q", val)
	}
	if val := os.Getenv("WITH_INLINE_COMMENT"); val != "val123" {
		t.Errorf("expected WITH_INLINE_COMMENT=val123, got %q", val)
	}
	// Existing should NOT be overwritten
	if val := os.Getenv("EXISTING_VAR"); val != "already_set" {
		t.Errorf("expected EXISTING_VAR=already_set, got %q", val)
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

	clearTestEnv(t, "AUTH_KEY_CLIENT_ID", "AUTH_KEY_CLIENT_SECRET", "AUTH_KEY_REFRESH_TOKEN", "AUTH_KEY_ROOT", "JWT_SECRET", "PORT")

	if err := loadJSONConfig(jsonPath); err != nil {
		t.Fatalf("loadJSONConfig failed: %v", err)
	}

	if val := os.Getenv("AUTH_KEY_CLIENT_ID"); val != "oauth-client-id" {
		t.Errorf("expected AUTH_KEY_CLIENT_ID=oauth-client-id, got %q", val)
	}
	if val := os.Getenv("AUTH_KEY_CLIENT_SECRET"); val != "oauth-client-secret" {
		t.Errorf("expected AUTH_KEY_CLIENT_SECRET=oauth-client-secret, got %q", val)
	}
	if val := os.Getenv("AUTH_KEY_REFRESH_TOKEN"); val != "oauth-refresh-token" {
		t.Errorf("expected AUTH_KEY_REFRESH_TOKEN=oauth-refresh-token, got %q", val)
	}
	if val := os.Getenv("AUTH_KEY_ROOT"); val != "oauth-root-id" {
		t.Errorf("expected AUTH_KEY_ROOT=oauth-root-id, got %q", val)
	}
	if val := os.Getenv("JWT_SECRET"); val != "custom-jwt-secret" {
		t.Errorf("expected JWT_SECRET=custom-jwt-secret, got %q", val)
	}
	if val := os.Getenv("PORT"); val != "5000" {
		t.Errorf("expected PORT=5000, got %q", val)
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
			"spotify_client_id": "sp-id-123",
			"spotify_cookie": "sp-cookie-abc",
			"musixmatch_cookie": "mxm-cookie-xyz",
			"apple_music_auth_token": "apple-token-123"
		}
	}`

	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	clearTestEnv(t, "GDRIVE_ENABLED", "GDRIVE_CACHED_TTML", "GDRIVE_CACHED_SPOTIFY", "SPOTIFY_CLIENT_ID", "SPOTIFY_COOKIE", "MUSIXMATCH_COOKIE", "APPLE_MUSIC_AUTH_TOKEN")

	if err := loadJSONConfig(jsonPath); err != nil {
		t.Fatalf("loadJSONConfig failed: %v", err)
	}

	if val := os.Getenv("GDRIVE_ENABLED"); val != "true" {
		t.Errorf("expected GDRIVE_ENABLED=true, got %q", val)
	}
	if val := os.Getenv("GDRIVE_CACHED_TTML"); val != "folder-ttml-1,folder-ttml-2" {
		t.Errorf("expected GDRIVE_CACHED_TTML=folder-ttml-1,folder-ttml-2, got %q", val)
	}
	if val := os.Getenv("GDRIVE_CACHED_SPOTIFY"); val != "folder-spotify" {
		t.Errorf("expected GDRIVE_CACHED_SPOTIFY=folder-spotify, got %q", val)
	}
	if val := os.Getenv("SPOTIFY_CLIENT_ID"); val != "sp-id-123" {
		t.Errorf("expected SPOTIFY_CLIENT_ID=sp-id-123, got %q", val)
	}
	if val := os.Getenv("SPOTIFY_COOKIE"); val != "sp-cookie-abc" {
		t.Errorf("expected SPOTIFY_COOKIE=sp-cookie-abc, got %q", val)
	}
	if val := os.Getenv("APPLE_MUSIC_AUTH_TOKEN"); val != "apple-token-123" {
		t.Errorf("expected APPLE_MUSIC_AUTH_TOKEN=apple-token-123, got %q", val)
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

	clearTestEnv(t, "AUTH_KEY_CLIENT_ID", "AUTH_KEY_CLIENT_SECRET", "AUTH_KEY_REFRESH_TOKEN")

	if err := loadJSONConfig(jsonPath); err != nil {
		t.Fatalf("loadJSONConfig failed: %v", err)
	}

	if val := os.Getenv("AUTH_KEY_CLIENT_ID"); val != "gcp-client-id" {
		t.Errorf("expected AUTH_KEY_CLIENT_ID=gcp-client-id, got %q", val)
	}
	if val := os.Getenv("AUTH_KEY_CLIENT_SECRET"); val != "gcp-client-secret" {
		t.Errorf("expected AUTH_KEY_CLIENT_SECRET=gcp-client-secret, got %q", val)
	}
	if val := os.Getenv("AUTH_KEY_REFRESH_TOKEN"); val != "gcp-refresh-token" {
		t.Errorf("expected AUTH_KEY_REFRESH_TOKEN=gcp-refresh-token, got %q", val)
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

	// Load from non-existent file so neither .env nor auth.json in root is loaded
	cfg := Load(filepath.Join(t.TempDir(), "empty.env"))

	// Non-sensitive defaults
	if cfg.Server.Addr != "3000" {
		t.Errorf("expected default PORT=3000, got %q", cfg.Server.Addr)
	}
	// Sensitive tokens should be empty, not hardcoded!
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
