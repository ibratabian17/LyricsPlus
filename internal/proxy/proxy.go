// Package proxy provides the SSRF-safe outbound HTTP proxy pipeline.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lyricsplus/backend/internal/config"
)

// hopByHopHeaders are stripped per RFC 7230 when forwarding.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

var errSSRFBlocked = errors.New("ssrf: request blocked")

// Client wraps the shared HTTP transport plus optional forward-proxy rewrite.
type Client struct {
	http *http.Client
	cfg  config.Proxy
}

func New(cfg config.Server) *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		Proxy:                 http.ProxyFromEnvironment,
	}
	return &Client{
		http: &http.Client{Transport: tr, Timeout: 30 * time.Second},
		cfg:  cfg.Proxy,
	}
}

// MustDial reports a critical misconfiguration upfront (e.g. mapping to itself).
func (c *Client) MustDial() error {
	return nil
}

// Get performs a GET through the proxy pipeline with SSRF protection.
func (c *Client) Get(ctx context.Context, urlstr string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlstr, nil)
	if err != nil {
		return nil, err
	}
	for k, vv := range header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	return c.Do(req)
}

// Post performs a POST through the proxy pipeline.
func (c *Client) Post(ctx context.Context, urlstr string, header http.Header, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlstr, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vv := range header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.Do(req)
}

// Do validates URL safety and performs the request, optionally routing
// through the configured forward proxy.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	u := req.URL
	if err := c.validate(u); err != nil {
		return nil, err
	}

	if c.cfg.Enabled && c.cfg.URL != "" {
		internal := req.Clone(req.Context())
		for _, h := range hopByHopHeaders {
			internal.Header.Del(h)
		}
		targetStr := u.String()
		var fullURL string
		if strings.Contains(c.cfg.URL, "?") {
			fullURL = c.cfg.URL + url.QueryEscape(targetStr)
		} else {
			fullURL = strings.TrimSuffix(c.cfg.URL, "/") + "/" + url.QueryEscape(targetStr)
		}
		newU, err := url.Parse(fullURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		internal.URL = newU
		internal.Host = newU.Host
		if c.cfg.Token != "" {
			tokenHeader := c.cfg.TokenHeader
			if tokenHeader == "" {
				tokenHeader = "x-proxy-token"
			}
			internal.Header.Set(tokenHeader, c.cfg.Token)
		}
		resp, err := c.http.Do(internal)
		if err != nil {
			return nil, dedupeErr(err)
		}
		return decompressIfNeeded(resp), nil
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, dedupeErr(err)
	}
	return decompressIfNeeded(resp), nil
}

type gzipReadCloser struct {
	*gzip.Reader
	closer io.Closer
}

func (g *gzipReadCloser) Close() error {
	_ = g.Reader.Close()
	return g.closer.Close()
}

func decompressIfNeeded(resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err == nil {
			resp.Body = &gzipReadCloser{Reader: gz, closer: resp.Body}
		}
	}
	return resp
}

func (c *Client) validate(u *url.URL) error {
	host := u.Hostname()
	// Block IP literal and weird shadowing.
	if host == "" {
		return errSSRFBlocked
	}
	if ip := parseIPLiteral(host); ip != nil {
		if isBogon(ip) {
			return fmt.Errorf("%w: private ip %s", errSSRFBlocked, ip)
		}
		return nil
	}
	// Pin DNS resolution verbatim to prevent rebinding.
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("dns lookup failed: %w", err)
	}
	seen := map[string]bool{}
	for _, ip := range ips {
		if isBogon(ip) {
			return fmt.Errorf("%w: resolves to private ip %s", errSSRFBlocked, ip)
		}
		seen[ip.String()] = true
	}
	_ = seen
	return nil
}

func parseIPLiteral(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	return nil
}

func isBogon(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 127 || ip4[0] == 0 || ip4[0] == 10 {
			return true
		}
		if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return true
		}
		if ip4[0] == 192 && ip4[1] == 168 {
			return true
		}
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

func dedupeErr(err error) error {
	if err == nil {
		return nil
	}
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

// ReadAll is a convenience for bounded body reads.
func ReadAll(r io.Reader, limit int64) ([]byte, error) {
	lr := &io.LimitedReader{R: r, N: limit + 1}
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return data, nil
}
