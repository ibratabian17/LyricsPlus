package accountmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyricsplus/backend/internal/config"
)

func TestAccountStore_LoadSaveRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "auth.json")

	initialJSON := `{
  "jwt_secret": "my-jwt-secret-xyz",
  "server_proxy_urls": "https://proxy.example.com",
  "gdrive": {
    "enabled": true,
    "folders": {
      "user_tml": "folder-123"
    }
  },
  "provider": {
    "spotify_cookie": "sp_dc=initial-cookie-value",
    "spotify_client_id": "initial-client-id",
    "spotify_client_secret": "initial-client-secret",
    "deezer_arl": "initial-arl-token"
  }
}`

	if err := os.WriteFile(confPath, []byte(initialJSON), 0644); err != nil {
		t.Fatalf("failed to write test auth.json: %v", err)
	}

	st, err := LoadStore(confPath)
	if err != nil {
		t.Fatalf("LoadStore failed: %v", err)
	}

	// Verify initial loading from scalar fields
	if len(st.SpotifyAccounts) != 1 {
		t.Fatalf("expected 1 spotify account, got %d", len(st.SpotifyAccounts))
	}
	if st.SpotifyAccounts[0].CLIENT_ID != "initial-client-id" {
		t.Errorf("expected client_id initial-client-id, got %s", st.SpotifyAccounts[0].CLIENT_ID)
	}
	if len(st.DeezerAccounts) != 1 || st.DeezerAccounts[0].ARL != "initial-arl-token" {
		t.Errorf("expected deezer arl initial-arl-token, got %+v", st.DeezerAccounts)
	}

	// Add new accounts
	st.SpotifyAccounts = append(st.SpotifyAccounts, config.SpotifyAccount{
		NAMEID: "spotify-secondary",
		COOKIE: "sp_dc=second-cookie-value",
	})
	st.AppleAccounts = append(st.AppleAccounts, config.AppleAccount{
		NAMEID:             "apple-1",
		AUTH_TYPE:          "android",
		ANDROID_AUTH_TOKEN: "sample-token",
		STOREFRONT:         "us",
	})
	st.MusixmatchAccounts = append(st.MusixmatchAccounts, config.MusixmatchAccount{
		NAMEID:    "mxm-1",
		AUTH_TYPE: "android",
		EMAIL:     "test@example.com",
		PASSWORD:  "secret-pass",
	})
	st.GDriveAccounts = append(st.GDriveAccounts, config.GDriveAccount{
		ClientID:     "gdrive-client",
		ClientSecret: "gdrive-secret",
		RefreshToken: "gdrive-refresh",
		Root:         "gdrive-root",
	})

	if err := st.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Reload from file and verify
	reloaded, err := LoadStore(confPath)
	if err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	if len(reloaded.SpotifyAccounts) != 2 {
		t.Errorf("expected 2 spotify accounts, got %d", len(reloaded.SpotifyAccounts))
	}
	if len(reloaded.AppleAccounts) != 1 {
		t.Errorf("expected 1 apple account, got %d", len(reloaded.AppleAccounts))
	}
	if len(reloaded.MusixmatchAccounts) != 1 {
		t.Errorf("expected 1 musixmatch account, got %d", len(reloaded.MusixmatchAccounts))
	}
	if len(reloaded.GDriveAccounts) != 1 {
		t.Errorf("expected 1 gdrive account, got %d", len(reloaded.GDriveAccounts))
	}

	// Verify unmanaged fields were preserved!
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("failed to read reloaded file: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse reloaded file as json: %v", err)
	}

	var jwtSecret string
	if err := json.Unmarshal(raw["jwt_secret"], &jwtSecret); err != nil || jwtSecret != "my-jwt-secret-xyz" {
		t.Errorf("jwt_secret lost or altered, got %q", jwtSecret)
	}

	var gd struct {
		Folders map[string]string `json:"folders"`
	}
	if err := json.Unmarshal(raw["gdrive"], &gd); err != nil || gd.Folders["user_tml"] != "folder-123" {
		t.Errorf("gdrive.folders lost or altered: %+v", gd)
	}
}

func TestAccountStore_LoadSaveEnv(t *testing.T) {
	tmpDir := t.TempDir()
	envPath := filepath.Join(tmpDir, ".env")

	initial := `# server tuning (must survive round-trip)
GDRIVE_ENABLED=true

# gdrive oauth
AUTH_KEY_CLIENT_ID=env-client-id
AUTH_KEY_CLIENT_SECRET=env-client-secret
AUTH_KEY_REFRESH_TOKEN=env-refresh

# providers
SPOTIFY_COOKIE="sp_dc=env-spotify-cookie"
SPOTIFY_CLIENT_ID=env-spot-cid
SPOTIFY_CLIENT_SECRET=env-spot-csec
DEEZER_ARL=env-arl
MUSIXMATCH_ANDROID_EMAIL="env@example.com"
MUSIXMATCH_ANDROID_PASSWORD=env-pass
QQ_COOKIE="qq=cookie value"
`
	if err := os.WriteFile(envPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	st, err := LoadStore(envPath)
	if err != nil {
		t.Fatalf("LoadStore(%s): %v", envPath, err)
	}
	if !IsEnvPath(envPath) {
		t.Fatal("expected IsEnvPath to be true")
	}

	if len(st.SpotifyAccounts) != 1 || st.SpotifyAccounts[0].COOKIE != "sp_dc=env-spotify-cookie" {
		t.Errorf("spotify env load wrong: %+v", st.SpotifyAccounts)
	}
	if len(st.DeezerAccounts) != 1 || st.DeezerAccounts[0].ARL != "env-arl" {
		t.Errorf("deezer env load wrong: %+v", st.DeezerAccounts)
	}
	if len(st.MusixmatchAccounts) != 1 || st.MusixmatchAccounts[0].EMAIL != "env@example.com" {
		t.Errorf("musixmatch env load wrong: %+v", st.MusixmatchAccounts)
	}
	if st.QQCookie != "qq=cookie value" {
		t.Errorf("qq env load wrong: %q", st.QQCookie)
	}
	if len(st.GDriveAccounts) != 1 || st.GDriveAccounts[0].ClientID != "env-client-id" {
		t.Errorf("gdrive env load wrong: %+v", st.GDriveAccounts)
	}

	// Add a second spotify account and a web apple account, then save.
	st.SpotifyAccounts = append(st.SpotifyAccounts, config.SpotifyAccount{
		NAMEID: "spotify-2",
		COOKIE: "another cookie with = and spaces",
	})
	st.AppleAccounts = append(st.AppleAccounts, config.AppleAccount{
		NAMEID:           "apple-web-1",
		AUTH_TYPE:        "web",
		MUSIC_AUTH_TOKEN: "web-token",
		STOREFRONT:       "us",
	})
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadStore(envPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.SpotifyAccounts) != 2 {
		t.Errorf("expected 2 spotify accounts after save, got %d", len(reloaded.SpotifyAccounts))
	}
	if got := reloaded.SpotifyAccounts[0].COOKIE; got != "sp_dc=env-spotify-cookie" {
		t.Errorf("first spotify cookie altered: %q", got)
	}
	if got := reloaded.SpotifyAccounts[1].COOKIE; got != "another cookie with = and spaces" {
		t.Errorf("second spotify cookie wrong (quoting broken): %q", got)
	}
	if len(reloaded.AppleAccounts) != 1 || reloaded.AppleAccounts[0].MUSIC_AUTH_TOKEN != "web-token" {
		t.Errorf("apple web account lost: %+v", reloaded.AppleAccounts)
	}

	// Unrelated keys + comments must be preserved.
	data, _ := os.ReadFile(envPath)
	content := string(data)
	for _, wanted := range []string{
		"# server tuning (must survive round-trip)",
		"GDRIVE_ENABLED=true",
		"# gdrive oauth",
		"# providers",
		"MUSIXMATCH_ANDROID_EMAIL=env@example.com",
	} {
		if !strings.Contains(content, wanted) {
			t.Errorf(".env lost line %q after save:\n%s", wanted, content)
		}
	}
	// Array line should be present and quoted (contains spaces/quotes).
	if !strings.Contains(content, "SPOTIFY_ACCOUNTS=") {
		t.Errorf(".env missing SPOTIFY_ACCOUNTS after save:\n%s", content)
	}

	// Removing the last spotify account must not resurrect a scalar-created one.
	st.SpotifyAccounts = nil
	if err := st.Save(); err != nil {
		t.Fatalf("save after removal: %v", err)
	}
	reloaded2, err := LoadStore(envPath)
	if err != nil {
		t.Fatalf("reload after removal: %v", err)
	}
	if len(reloaded2.SpotifyAccounts) != 0 {
		t.Errorf("expected 0 spotify accounts after removal, got %d (%+v)", len(reloaded2.SpotifyAccounts), reloaded2.SpotifyAccounts)
	}
}

func TestMaskCredential(t *testing.T) {
	if got := MaskCredential(""); got != "<empty>" {
		t.Errorf("expected <empty>, got %q", got)
	}
	if got := MaskCredential("short"); got != "****" {
		t.Errorf("expected ****, got %q", got)
	}
	long := "123456abcdefghijklmn7890"
	masked := MaskCredential(long)
	if !filepath.HasPrefix(masked, "123456...") && !filepath.HasPrefix(masked, "123456...") {
		// check prefix and suffix
		if masked != "123456...7890 (24 chars)" {
			t.Errorf("expected '123456...7890 (24 chars)', got %q", masked)
		}
	}
}

func TestExtractSpDc(t *testing.T) {
	raw := `sp_t=abc; sp_dc=AQA12345XYZ==; sp_key=def`
	extracted := ExtractSpDc(raw)
	if extracted != "AQA12345XYZ==" {
		t.Errorf("expected AQA12345XYZ==, got %q", extracted)
	}

	single := "AQA12345XYZ=="
	if ExtractSpDc(single) != "AQA12345XYZ==" {
		t.Errorf("expected single token unchanged, got %q", ExtractSpDc(single))
	}
}
