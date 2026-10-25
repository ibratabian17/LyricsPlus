package middleware

import (
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/domain"
	"lyricsplus/backend/internal/logger"
)

type ipMatcher struct {
	raw   string
	exact net.IP
	cidr  *net.IPNet
}

func parseIPMatcher(s string) ipMatcher {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		_, ipNet, err := net.ParseCIDR(s)
		if err == nil {
			return ipMatcher{raw: s, cidr: ipNet}
		}
	}
	ip := net.ParseIP(s)
	return ipMatcher{raw: s, exact: ip}
}

func (m ipMatcher) matches(parsedIP net.IP, clientIP string) bool {
	if m.cidr != nil && parsedIP != nil {
		return m.cidr.Contains(parsedIP)
	}
	if m.exact != nil && parsedIP != nil {
		return m.exact.Equal(parsedIP)
	}
	return strings.EqualFold(clientIP, m.raw)
}

type uaMatcher struct {
	raw   string
	regex *regexp.Regexp
}

func parseUAMatcher(s string) uaMatcher {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "re:") {
		re, err := regexp.Compile(s[3:])
		if err == nil {
			return uaMatcher{raw: s, regex: re}
		}
	} else if strings.HasPrefix(s, "^") || strings.HasSuffix(s, "$") {
		re, err := regexp.Compile(s)
		if err == nil {
			return uaMatcher{raw: s, regex: re}
		}
	}
	return uaMatcher{raw: strings.ToLower(s)}
}

func (m uaMatcher) matches(ua string) bool {
	if m.regex != nil {
		return m.regex.MatchString(ua)
	}
	return strings.Contains(strings.ToLower(ua), m.raw)
}

func normalizeCountry(c string) string {
	c = strings.TrimSpace(c)
	if len(c) == 2 {
		return strings.ToUpper(c)
	}
	switch strings.ToLower(c) {
	case "india":
		return "IN"
	case "indonesia":
		return "ID"
	case "mexico":
		return "MX"
	case "philippines":
		return "PH"
	case "brazil":
		return "BR"
	case "united states", "usa", "us":
		return "US"
	case "peru":
		return "PE"
	case "south africa":
		return "ZA"
	case "colombia":
		return "CO"
	case "united kingdom", "uk", "great britain":
		return "GB"
	case "germany":
		return "DE"
	case "france":
		return "FR"
	case "canada":
		return "CA"
	case "russia":
		return "RU"
	case "japan":
		return "JP"
	case "china":
		return "CN"
	default:
		return strings.ToUpper(c)
	}
}

type compiledRule struct {
	name       string
	action     string
	matchMode  string
	ipMatchers []ipMatcher
	uaMatchers []uaMatcher
	countries  []string
	paths      []string
	throttle   *shardedRateLimiter
	statusCode int
	message    string
}

func (r *compiledRule) matches(parsedIP net.IP, clientIP, country, ua, path string) bool {
	if len(r.paths) > 0 {
		pathMatched := false
		for _, p := range r.paths {
			if strings.HasPrefix(path, p) {
				pathMatched = true
				break
			}
		}
		if !pathMatched {
			return false
		}
	}

	hasIPs := len(r.ipMatchers) > 0
	hasUAs := len(r.uaMatchers) > 0
	hasCCs := len(r.countries) > 0

	if !hasIPs && !hasUAs && !hasCCs {
		return true
	}

	ipMatch := false
	for _, m := range r.ipMatchers {
		if m.matches(parsedIP, clientIP) {
			ipMatch = true
			break
		}
	}

	uaMatch := false
	for _, m := range r.uaMatchers {
		if m.matches(ua) {
			uaMatch = true
			break
		}
	}

	ccMatch := false
	normCountry := normalizeCountry(country)
	for _, c := range r.countries {
		if strings.EqualFold(normCountry, c) {
			ccMatch = true
			break
		}
	}

	if strings.EqualFold(r.matchMode, "all") {
		if hasIPs && !ipMatch {
			return false
		}
		if hasUAs && !uaMatch {
			return false
		}
		if hasCCs && !ccMatch {
			return false
		}
		return true
	}

	return (hasIPs && ipMatch) || (hasUAs && uaMatch) || (hasCCs && ccMatch)
}

func matchesIPList(matchers []ipMatcher, parsedIP net.IP, clientIP string) bool {
	for _, m := range matchers {
		if m.matches(parsedIP, clientIP) {
			return true
		}
	}
	return false
}

func matchesUAList(matchers []uaMatcher, ua string) bool {
	for _, m := range matchers {
		if m.matches(ua) {
			return true
		}
	}
	return false
}

func matchesCountryList(countries []string, country string) bool {
	if len(countries) == 0 || country == "" {
		return false
	}
	norm := normalizeCountry(country)
	for _, c := range countries {
		if strings.EqualFold(norm, c) {
			return true
		}
	}
	return false
}

type ExternalScrapingEngine struct {
	enabled             bool
	blockEmptyUA        bool
	rules               []compiledRule
	shorthandBlockedIPs []ipMatcher
	shorthandBlockedUAs []uaMatcher
	shorthandBlockedCCs []string
	shorthandCacheIPs   []ipMatcher
	shorthandCacheUAs   []uaMatcher
	shorthandCacheCCs   []string
	logger              *logger.Logger
}

func NewExternalScraping(cfg config.ExternalScraping, lg *logger.Logger) *ExternalScrapingEngine {
	e := &ExternalScrapingEngine{
		enabled:      cfg.Enabled,
		blockEmptyUA: cfg.BlockEmptyUserAgent,
		logger:       lg,
	}

	for _, s := range cfg.BlockedIPs {
		if s = strings.TrimSpace(s); s != "" {
			e.shorthandBlockedIPs = append(e.shorthandBlockedIPs, parseIPMatcher(s))
		}
	}
	for _, s := range cfg.BlockedUserAgents {
		if s = strings.TrimSpace(s); s != "" {
			e.shorthandBlockedUAs = append(e.shorthandBlockedUAs, parseUAMatcher(s))
		}
	}
	for _, s := range cfg.BlockedCountries {
		if s = strings.TrimSpace(s); s != "" {
			e.shorthandBlockedCCs = append(e.shorthandBlockedCCs, normalizeCountry(s))
		}
	}

	for _, s := range cfg.CachedOnlyIPs {
		if s = strings.TrimSpace(s); s != "" {
			e.shorthandCacheIPs = append(e.shorthandCacheIPs, parseIPMatcher(s))
		}
	}
	for _, s := range cfg.CachedOnlyUserAgents {
		if s = strings.TrimSpace(s); s != "" {
			e.shorthandCacheUAs = append(e.shorthandCacheUAs, parseUAMatcher(s))
		}
	}
	for _, s := range cfg.CachedOnlyCountries {
		if s = strings.TrimSpace(s); s != "" {
			e.shorthandCacheCCs = append(e.shorthandCacheCCs, normalizeCountry(s))
		}
	}

	for _, r := range cfg.Rules {
		cr := compiledRule{
			name:       r.Name,
			action:     strings.ToLower(strings.TrimSpace(r.Action)),
			matchMode:  strings.ToLower(strings.TrimSpace(r.MatchMode)),
			statusCode: r.StatusCode,
			message:    r.Message,
		}
		if cr.action == "" {
			cr.action = "block"
		}
		if cr.matchMode == "" {
			cr.matchMode = "any"
		}

		for _, ip := range r.IPs {
			if ip = strings.TrimSpace(ip); ip != "" {
				cr.ipMatchers = append(cr.ipMatchers, parseIPMatcher(ip))
			}
		}
		for _, ua := range r.UserAgents {
			if ua = strings.TrimSpace(ua); ua != "" {
				cr.uaMatchers = append(cr.uaMatchers, parseUAMatcher(ua))
			}
		}
		for _, cc := range r.Countries {
			if cc = strings.TrimSpace(cc); cc != "" {
				cr.countries = append(cr.countries, normalizeCountry(cc))
			}
		}
		for _, p := range r.Paths {
			if p = strings.TrimSpace(p); p != "" {
				cr.paths = append(cr.paths, p)
			}
		}

		if r.Throttle != nil && r.Throttle.Requests > 0 {
			cr.throttle = newShardedRateLimiter(r.Throttle.Requests, r.Throttle.Window)
		} else if cr.action == "throttle" {
			cr.throttle = newShardedRateLimiter(60, 10*time.Second)
		}

		e.rules = append(e.rules, cr)
	}

	return e
}

func ExternalScraping(cfg config.ExternalScraping, lg *logger.Logger) func(http.Handler) http.Handler {
	if !cfg.Enabled {
		return func(next http.Handler) http.Handler {
			return next
		}
	}
	engine := NewExternalScraping(cfg, lg)
	return engine.Middleware
}

func (e *ExternalScrapingEngine) Middleware(next http.Handler) http.Handler {
	if !e.enabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsHealthPath(r) {
			next.ServeHTTP(w, r)
			return
		}

		ua := r.UserAgent()
		if e.blockEmptyUA && strings.TrimSpace(ua) == "" {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "Forbidden",
				"message": "User-Agent header is required",
			})
			return
		}

		clientIP := ClientIP(r)
		parsedIP := net.ParseIP(clientIP)
		country := ClientCountry(r)
		path := r.URL.Path

		// Check shorthand blocked
		if matchesIPList(e.shorthandBlockedIPs, parsedIP, clientIP) ||
			matchesUAList(e.shorthandBlockedUAs, ua) ||
			matchesCountryList(e.shorthandBlockedCCs, country) {
			if e.logger != nil {
				e.logger.Warnf("request blocked: ip=%s country=%s ua=%q path=%s rule=shorthand_blocked", clientIP, country, ua, path)
			}
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "Forbidden",
				"message": "Access restricted",
			})
			return
		}

		cacheOnly := false
		if matchesIPList(e.shorthandCacheIPs, parsedIP, clientIP) ||
			matchesUAList(e.shorthandCacheUAs, ua) ||
			matchesCountryList(e.shorthandCacheCCs, country) {
			cacheOnly = true
		}

		// Evaluate rules
		for _, rule := range e.rules {
			if !rule.matches(parsedIP, clientIP, country, ua, path) {
				continue
			}

			switch rule.action {
			case "block", "deny":
				if e.logger != nil {
					e.logger.Warnf("request blocked: ip=%s country=%s ua=%q path=%s rule=%q", clientIP, country, ua, path, rule.name)
				}
				status := rule.statusCode
				if status <= 0 {
					status = http.StatusForbidden
				}
				msg := rule.message
				if msg == "" {
					msg = "Access restricted"
				}
				w.Header().Set("Cache-Control", "no-store")
				writeJSON(w, status, map[string]string{
					"error":   http.StatusText(status),
					"message": msg,
				})
				return

			case "throttle", "rate_limit":
				if rule.throttle != nil {
					key := rule.name + ":" + clientIP
					rem, ok := rule.throttle.allow(key)
					if !ok {
						if e.logger != nil {
							e.logger.Warnf("request throttled: ip=%s country=%s ua=%q path=%s rule=%q retry=%s", clientIP, country, ua, path, rule.name, rem)
						}
						w.Header().Set("Retry-After", rem)
						w.Header().Set("Cache-Control", "no-store")
						msg := rule.message
						if msg == "" {
							msg = "Rate limit exceeded. Retry in " + rem + " seconds."
						}
						writeJSON(w, http.StatusTooManyRequests, map[string]string{
							"error":   "Too Many Requests",
							"message": msg,
						})
						return
					}
				}

			case "cached_only", "cache_only":
				if rule.throttle != nil {
					key := rule.name + ":" + clientIP
					rem, ok := rule.throttle.allow(key)
					if !ok {
						if e.logger != nil {
							e.logger.Warnf("request throttled: ip=%s country=%s ua=%q path=%s rule=%q retry=%s", clientIP, country, ua, path, rule.name, rem)
						}
						w.Header().Set("Retry-After", rem)
						w.Header().Set("Cache-Control", "no-store")
						writeJSON(w, http.StatusTooManyRequests, map[string]string{
							"error":   "Too Many Requests",
							"message": "Rate limit exceeded for cached queries. Retry in " + rem + " seconds.",
						})
						return
					}
				}
				cacheOnly = true
			}
		}

		ctx := r.Context()
		if cacheOnly {
			ctx = domain.WithCacheOnly(ctx, true)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
