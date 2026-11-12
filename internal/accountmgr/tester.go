package accountmgr

import (
	"context"
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

type TestResult struct {
	Provider string
	NameID   string
	Status   string
	Message  string
	Duration time.Duration
}

type CredentialTester struct {
	client *http.Client
}

func NewTester() *CredentialTester {
	return &CredentialTester{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

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
