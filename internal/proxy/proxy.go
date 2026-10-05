// Package proxy provides the SSRF-safe outbound HTTP proxy pipeline.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"lyricsplus/backend/internal/config"
)

// dnsCache caches successful IP lookups to avoid per-request blocking DNS on
// the validate() hot path. Entries expire after dnsCacheTTL.
const dnsCacheTTL = 60 * time.Second

type dnsCacheEntry struct {
	ips    []net.IP
	expiry time.Time
}

var (
	dnsMu    sync.Mutex
	dnsStore = make(map[string]dnsCacheEntry, 64)
)

func lookupIPCached(host string) ([]net.IP, error) {
	now := time.Now()
	dnsMu.Lock()
	if e, ok := dnsStore[host]; ok && now.Before(e.expiry) {
		ips := e.ips
		dnsMu.Unlock()
		return ips, nil
	}
	dnsMu.Unlock()

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}

	dnsMu.Lock()
	// Prune stale entries if the map grows too large.
	if len(dnsStore) > 512 {
		for k, v := range dnsStore {
			if now.After(v.expiry) {
				delete(dnsStore, k)
			}
		}
	}
	dnsStore[host] = dnsCacheEntry{ips: ips, expiry: now.Add(dnsCacheTTL)}
	dnsMu.Unlock()
	return ips, nil
}

// hopByHopHeaders are stripped per RFC 7230 when forwarding.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

var errSSRFBlocked = errors.New("ssrf: request blocked")

// defaultRequestTimeout is applied to any outbound request that does not
// already carry a context deadline.
const defaultRequestTimeout = 15 * time.Second

// Client wraps the shared HTTP transport plus optional forward-proxy rewrite.
type Client struct {
	http *http.Client
	cfg  config.Proxy
}

func New(cfg config.Server) *Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2000,
		MaxIdleConnsPerHost:   500,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
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
// through the configured forward proxy (picking randomly from the URL list).
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	u := req.URL
	if err := c.validate(u); err != nil {
		return nil, err
	}

	req, cancel := withDefaultDeadline(req)

	if c.cfg.Enabled {
		if proxyURL := c.pickProxyURL(); proxyURL != "" && shouldProxy(u) {
			internal := req.Clone(req.Context())
			for _, h := range hopByHopHeaders {
				internal.Header.Del(h)
			}
			targetStr := u.String()
			var fullURL string
			if strings.Contains(proxyURL, "?") {
				fullURL = proxyURL + url.QueryEscape(targetStr)
			} else {
				fullURL = strings.TrimSuffix(proxyURL, "/") + "/" + url.QueryEscape(targetStr)
			}
			newU, err := url.Parse(fullURL)
			if err != nil {
				cancel()
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
			// Advertise Connection: keep-alive to the proxy so the upstream
			// TCP socket is reused across worker requests.
			if internal.Header.Get("Connection") == "" || internal.Header.Get("Connection") == "close" {
				internal.Header.Set("Connection", "keep-alive")
			}
			resp, err := c.http.Do(internal)
			if err != nil {
				cancel()
				return nil, dedupeErr(err)
			}
			return wrapCancelBody(decompressIfNeeded(resp), cancel), nil
		}
	}

	if req.Header.Get("Connection") == "" || req.Header.Get("Connection") == "close" {
		req.Header.Set("Connection", "keep-alive")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, dedupeErr(err)
	}
	return wrapCancelBody(decompressIfNeeded(resp), cancel), nil
}

// wrapCancelBody attaches the cancel func to the response body so it is called
// when the caller closes the body, releasing context timer resources promptly.
func wrapCancelBody(resp *http.Response, cancel context.CancelFunc) *http.Response {
	if resp == nil || resp.Body == nil {
		cancel()
		return resp
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp
}

// pickProxyURL returns a randomly selected proxy URL from the configured list.
// Falls back to the legacy single SERVER_PROXY_URL value.
func (c *Client) pickProxyURL() string {
	raw := c.cfg.URLs
	if len(raw) == 0 && c.cfg.URL != "" {
		raw = []string{c.cfg.URL}
	}
	if len(raw) == 0 {
		return ""
	}
	return raw[rand.Intn(len(raw))]
}

// shouldProxy never routes localhost / loopback / mDNS (.local) hosts through
// the forward proxy.
func shouldProxy(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "127.0.0.1" || strings.HasSuffix(host, ".local") ||
		host == "::1" || host == "0.0.0.0" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return false
	}
	return true
}

// cancelBody wraps a ReadCloser and calls cancel when closed, releasing the
// context resources as soon as the response body is drained or discarded.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// withDefaultDeadline applies the JS-equivalent 15s per-request timeout when
// the caller did not provide a context deadline of its own.
// The returned cancel is wired to the response body so it fires on Close().
func withDefaultDeadline(req *http.Request) (*http.Request, context.CancelFunc) {
	if _, ok := req.Context().Deadline(); ok {
		return req, func() {}
	}
	ctx, cancel := context.WithTimeout(req.Context(), defaultRequestTimeout)
	return req.WithContext(ctx), cancel
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
	ips, err := lookupIPCached(host)
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
