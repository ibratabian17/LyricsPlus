package accountmgr

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lyricsplus/backend/internal/config"
)

// TestResult holds the outcome of testing a single account credential.
type TestResult struct {
	Provider string
	NameID   string
	Status   string // "OK", "ERROR", "EXPIRED", "UNAUTHORIZED", "SKIPPED"
	Message  string
	Duration time.Duration
}

// CredentialTester handles live verification of provider credentials.
type CredentialTester struct {
	client *http.Client
}

// NewTester creates a new CredentialTester.
func NewTester() *CredentialTester {
	return &CredentialTester{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// TestSpotify verifies a Spotify account's cookie and/or client credentials.
func (t *CredentialTester) TestSpotify(ctx context.Context, acc config.SpotifyAccount) TestResult {
	start := time.Now()
	res := TestResult{
		Provider: "Spotify",
		NameID:   acc.NAMEID,
	}

	cookie := CleanCookie(acc.COOKIE)
	if cookie != "" && !strings.Contains(cookie, "sp_dc=") {
		cookie = "sp_dc=" + cookie
	}

	// 1. Test Web Player cookie
	if cookie != "" {
		reqURL := "https://open.spotify.com/get_access_token?reason=transport&productType=web_player"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			res.Status = "ERROR"
			res.Message = err.Error()
			res.Duration = time.Since(start)
			return res
		}

		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
		req.Header.Set("Cookie", cookie)
		req.Header.Set("app-platform", "WebPlayer")
		req.Header.Set("Referer", "https://open.spotify.com/")

		resp, err := t.client.Do(req)
		if err != nil {
			res.Status = "ERROR"
			res.Message = fmt.Sprintf("network error: %v", err)
			res.Duration = time.Since(start)
			return res
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK {
			var data struct {
				IsAnonymous bool   `json:"isAnonymous"`
				AccessToken string `json:"accessToken"`
				ClientID    string `json:"clientId"`
			}
			if err := json.Unmarshal(body, &data); err == nil && data.AccessToken != "" {
				if data.IsAnonymous {
					res.Status = "EXPIRED"
					res.Message = "Cookie rejected (returned anonymous guest token, re-login needed)"
				} else {
					res.Status = "OK"
					res.Message = "Cookie valid (WebPlayer access token acquired)"
				}
				res.Duration = time.Since(start)
				return res
			}
		}

		res.Status = "UNAUTHORIZED"
		res.Message = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		res.Duration = time.Since(start)
		return res
	}

	// 2. Test Client ID & Secret
	if acc.CLIENT_ID != "" && acc.CLIENT_SECRET != "" {
		data := url.Values{"grant_type": {"client_credentials"}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://accounts.spotify.com/api/token", strings.NewReader(data.Encode()))
		if err != nil {
			res.Status = "ERROR"
			res.Message = err.Error()
			res.Duration = time.Since(start)
			return res
		}
		authBasic := base64.StdEncoding.EncodeToString([]byte(acc.CLIENT_ID + ":" + acc.CLIENT_SECRET))
		req.Header.Set("Authorization", "Basic "+authBasic)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := t.client.Do(req)
		if err != nil {
			res.Status = "ERROR"
			res.Message = fmt.Sprintf("network error: %v", err)
			res.Duration = time.Since(start)
			return res
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK {
			res.Status = "OK"
			res.Message = "Client credentials valid (API token acquired)"
		} else {
			res.Status = "UNAUTHORIZED"
			res.Message = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		res.Duration = time.Since(start)
		return res
	}

	res.Status = "SKIPPED"
	res.Message = "No credentials provided"
	res.Duration = time.Since(start)
	return res
}

// TestApple verifies an Apple Music account.
func (t *CredentialTester) TestApple(ctx context.Context, acc config.AppleAccount) TestResult {
	start := time.Now()
	res := TestResult{
		Provider: "Apple Music",
		NameID:   acc.NAMEID,
	}

	sf := acc.STOREFRONT
	if sf == "" {
		sf = "us"
	}

	testURL := fmt.Sprintf("https://amp-api.music.apple.com/v1/catalog/%s/search?term=Taylor+Swift&types=songs&limit=1", sf)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		res.Status = "ERROR"
		res.Message = err.Error()
		res.Duration = time.Since(start)
		return res
	}

	if strings.EqualFold(acc.AUTH_TYPE, "web") {
		if acc.MUSIC_AUTH_TOKEN == "" {
			res.Status = "SKIPPED"
			res.Message = "MUSIC_AUTH_TOKEN is empty"
			res.Duration = time.Since(start)
			return res
		}
		req.Header.Set("Authorization", "Bearer "+acc.MUSIC_AUTH_TOKEN)
		req.Header.Set("Origin", "https://music.apple.com")
		req.Header.Set("Referer", "https://music.apple.com")
	} else {
		if acc.ANDROID_AUTH_TOKEN == "" {
			res.Status = "SKIPPED"
			res.Message = "ANDROID_AUTH_TOKEN is empty"
			res.Duration = time.Since(start)
			return res
		}
		req.Header.Set("Authorization", "Bearer "+acc.ANDROID_AUTH_TOKEN)
		if acc.ANDROID_DSID != "" {
			req.Header.Set("x-dsid", acc.ANDROID_DSID)
		}
		ua := acc.ANDROID_USER_AGENT
		if ua == "" {
			ua = "Music/6.1 Android/16 model/RealmeGT2Pro build/1472 (dt:66)"
		}
		req.Header.Set("User-Agent", ua)
		if acc.ANDROID_COOKIE != "" {
			req.Header.Set("Cookie", acc.ANDROID_COOKIE)
		}
	}

	resp, err := t.client.Do(req)
	if err != nil {
		res.Status = "ERROR"
		res.Message = fmt.Sprintf("network error: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		res.Status = "OK"
		res.Message = fmt.Sprintf("Apple token valid (catalog search succeeded in %s)", sf)
	} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		res.Status = "UNAUTHORIZED"
		res.Message = fmt.Sprintf("HTTP %d: token invalid or expired", resp.StatusCode)
	} else {
		res.Status = "ERROR"
		res.Message = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	res.Duration = time.Since(start)
	return res
}

// TestMusixmatch verifies a Musixmatch account.
func (t *CredentialTester) TestMusixmatch(ctx context.Context, acc config.MusixmatchAccount) TestResult {
	start := time.Now()
	res := TestResult{
		Provider: "Musixmatch",
		NameID:   acc.NAMEID,
	}

	if strings.EqualFold(acc.AUTH_TYPE, "android") {
		if acc.EMAIL == "" || acc.PASSWORD == "" {
			res.Status = "SKIPPED"
			res.Message = "EMAIL or PASSWORD missing"
			res.Duration = time.Since(start)
			return res
		}

		// 1. Get token
		tokenURL := "https://apic-desktop.musixmatch.com/ws/1.1/token.get?app_id=android-player-v1.0&format=json"
		tokenReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
		tokenReq.Header.Set("User-Agent", "Dalvik/2.1.0 (Linux; U; Android 16; Pixel 8 Pro)")
		tokenResp, err := t.client.Do(tokenReq)
		if err != nil {
			res.Status = "ERROR"
			res.Message = fmt.Sprintf("token.get error: %v", err)
			res.Duration = time.Since(start)
			return res
		}
		defer func() { _ = tokenResp.Body.Close() }()

		var tokData struct {
			Message struct {
				Header struct {
					StatusCode int `json:"status_code"`
				} `json:"header"`
				Body struct {
					UserToken string `json:"user_token"`
				} `json:"body"`
			} `json:"message"`
		}
		if err := json.NewDecoder(tokenResp.Body).Decode(&tokData); err != nil || tokData.Message.Body.UserToken == "" {
			res.Status = "ERROR"
			res.Message = "failed to get initial android token"
			res.Duration = time.Since(start)
			return res
		}

		// 2. credential.post login
		now := time.Now().UTC()
		sigTarget := "credential.post" + fmt.Sprintf("%04d%02d%02d", now.Year(), int(now.Month()), now.Day())
		mac := hmac.New(sha1.New, []byte("MusixmatchSecretKey2015"))
		mac.Write([]byte(sigTarget))
		sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

		loginURL := fmt.Sprintf("https://apic-desktop.musixmatch.com/ws/1.1/credential.post?app_id=android-player-v1.0&usertoken=%s&signature=%s&signature_protocol=sha1&format=json",
			tokData.Message.Body.UserToken, sig)

		loginPayload := map[string]interface{}{
			"credential_list": []map[string]interface{}{
				{
					"credential": map[string]string{
						"type":     "mxm",
						"action":   "login",
						"email":    acc.EMAIL,
						"password": acc.PASSWORD,
					},
				},
			},
		}
		b, _ := json.Marshal(loginPayload)
		loginReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, bytes.NewReader(b))
		loginReq.Header.Set("Content-Type", "application/json")
		loginReq.Header.Set("User-Agent", "Dalvik/2.1.0 (Linux; U; Android 16; Pixel 8 Pro)")

		loginResp, err := t.client.Do(loginReq)
		if err != nil {
			res.Status = "ERROR"
			res.Message = fmt.Sprintf("login error: %v", err)
			res.Duration = time.Since(start)
			return res
		}
		defer func() { _ = loginResp.Body.Close() }()

		var loginData struct {
			Message struct {
				Header struct {
					StatusCode int    `json:"status_code"`
					Hint       string `json:"hint"`
				} `json:"header"`
			} `json:"message"`
		}
		if err := json.NewDecoder(loginResp.Body).Decode(&loginData); err == nil && loginData.Message.Header.StatusCode == 200 {
			res.Status = "OK"
			res.Message = fmt.Sprintf("Android account valid (logged in as %s)", acc.EMAIL)
		} else {
			res.Status = "UNAUTHORIZED"
			res.Message = fmt.Sprintf("Login failed (%d): %s", loginData.Message.Header.StatusCode, loginData.Message.Header.Hint)
		}
		res.Duration = time.Since(start)
		return res
	}

	// Web token/cookie test
	reqURL := "https://apic-desktop.musixmatch.com/ws/1.1/token.get?app_id=web-desktop-app-v1.0"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		res.Status = "ERROR"
		res.Message = err.Error()
		res.Duration = time.Since(start)
		return res
	}

	ua := acc.USER_AGENT
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)
	if acc.COOKIE != "" {
		req.Header.Set("Cookie", CleanCookie(acc.COOKIE))
	}
	req.Header.Set("Origin", "https://musixmatch.com")

	resp, err := t.client.Do(req)
	if err != nil {
		res.Status = "ERROR"
		res.Message = fmt.Sprintf("network error: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	var tokRes struct {
		Message struct {
			Header struct {
				StatusCode int `json:"status_code"`
			} `json:"header"`
			Body struct {
				UserToken string `json:"user_token"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokRes); err == nil && tokRes.Message.Header.StatusCode == 200 && tokRes.Message.Body.UserToken != "" {
		res.Status = "OK"
		res.Message = "Web credential valid (token acquired)"
	} else {
		res.Status = "ERROR"
		res.Message = fmt.Sprintf("token.get returned status %d", tokRes.Message.Header.StatusCode)
	}

	res.Duration = time.Since(start)
	return res
}

// TestDeezer verifies a Deezer account's ARL or refresh token.
func (t *CredentialTester) TestDeezer(ctx context.Context, acc config.DeezerAccount) TestResult {
	start := time.Now()
	res := TestResult{
		Provider: "Deezer",
		NameID:   acc.NAMEID,
	}

	rawToken := acc.REFRESH_TOKEN
	isARL := false
	if rawToken == "" {
		rawToken = acc.ARL
		isARL = true
	}

	if rawToken == "" {
		res.Status = "SKIPPED"
		res.Message = "No ARL or REFRESH_TOKEN provided"
		res.Duration = time.Since(start)
		return res
	}

	cleanToken := strings.TrimSpace(rawToken)
	cookieString := "refresh-token=" + cleanToken
	if isARL {
		cookieString = "arl=" + cleanToken
	}

	reqURL := "https://auth.deezer.com/login/anonymous?jwtToken="
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		res.Status = "ERROR"
		res.Message = err.Error()
		res.Duration = time.Since(start)
		return res
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", cookieString)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		res.Status = "ERROR"
		res.Message = fmt.Sprintf("network error: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	var authResp struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err == nil && authResp.JWT != "" {
		res.Status = "OK"
		res.Message = "Deezer token valid (JWT authentication succeeded)"
	} else {
		res.Status = "UNAUTHORIZED"
		res.Message = fmt.Sprintf("Authentication failed (HTTP %d, empty JWT)", resp.StatusCode)
	}

	res.Duration = time.Since(start)
	return res
}

// TestGDrive verifies a Google Drive OAuth2 credential.
func (t *CredentialTester) TestGDrive(ctx context.Context, acc config.GDriveAccount) TestResult {
	start := time.Now()
	res := TestResult{
		Provider: "Google Drive",
		NameID:   acc.ClientID,
	}

	if acc.RefreshToken == "" || acc.ClientID == "" || acc.ClientSecret == "" {
		res.Status = "SKIPPED"
		res.Message = "CLIENT_ID, CLIENT_SECRET, or REFRESH_TOKEN missing"
		res.Duration = time.Since(start)
		return res
	}

	params := url.Values{
		"client_id":     {acc.ClientID},
		"client_secret": {acc.ClientSecret},
		"refresh_token": {acc.RefreshToken},
		"grant_type":    {"refresh_token"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(params.Encode()))
	if err != nil {
		res.Status = "ERROR"
		res.Message = err.Error()
		res.Duration = time.Since(start)
		return res
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.client.Do(req)
	if err != nil {
		res.Status = "ERROR"
		res.Message = fmt.Sprintf("network error: %v", err)
		res.Duration = time.Since(start)
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	var tokenData struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tokenData)

	if resp.StatusCode == http.StatusOK && tokenData.AccessToken != "" {
		res.Status = "OK"
		res.Message = "Google OAuth2 refresh token valid"
	} else {
		res.Status = "UNAUTHORIZED"
		res.Message = fmt.Sprintf("OAuth error: %s (%s)", tokenData.Error, tokenData.ErrorDesc)
	}

	res.Duration = time.Since(start)
	return res
}
