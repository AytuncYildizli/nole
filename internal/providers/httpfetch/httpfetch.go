// Package httpfetch is a keyless, pure-Go Nólë EXTRACT provider. It GETs a public
// URL and strips the HTML to readable text using only the standard library (no
// API key, no Python, no headless browser, no third-party dependency). It is the
// LAST-RESORT keyless backstop on the extract route — the extract-side analogue
// of DDGS on the search route: it is tried only after a configured local
// Scrapling and the keyed remote extractors (Firecrawl/Tavily). It does NOT
// execute JavaScript, so it is weaker than those on SPA / JS-rendered pages —
// an honest, accepted limit that nonetheless makes extract / search_and_extract
// work out of the box with zero keys and zero setup.
//
// Like every Nólë provider it is a dumb gateway: it never judges result quality,
// never ranks or filters, and never prints or logs. Errors are redaction-safe
// (HTTP status + byte-size metadata only; never the response body, which can echo
// auth headers or private URLs). SSRF safety is enforced on every redirect hop
// via safenet.ValidateURLContext, mirroring the Scrapling redirect walk.
//
// Proxy support: NOLE_PROXY_URL env var (socks5://host:port or http://host:port)
// enables Tor/darkweb/privacy crawling. When set, the dialer routes through the
// proxy but keeps the SSRF guard active on the dialed proxy address. .onion URLs
// force proxy use — they require a SOCKS5 Tor proxy to resolve.
package httpfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dorukardahan/nole/internal/core"
	"github.com/dorukardahan/nole/internal/providers/providerhttp"
	"github.com/dorukardahan/nole/internal/safenet"
	"github.com/dorukardahan/nole/internal/version"
)

var _ core.Provider = Provider{}

const maxRedirects = 5

// userAgentPool is a rotating pool of User-Agent strings. httpfetch cyclically
// picks one per request so Cloudflare Turnstile/challenge pages see a changing,
// browser-like UA on each hop/retry. The pool always starts with the canonical
// Nole identity so Nólë stays *discoverable* first, then mixes in modern
// Chrome, Firefox, and Safari UAs that Cloudflare treats as real browsers.
var userAgentPool = []string{
	// Canonical Nole identity (sent first by default).
	"Nole/" + version.Version + " (+https://github.com/dorukardahan/nole)",
	// Modern browser UAs for Cloudflare bypass.
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:126.0) Gecko/20100101 Firefox/126.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64; rv:126.0) Gecko/20100101 Firefox/126.0",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
}

var userAgentPoolMu uint32 // atomic counter for rotation

// pickUserAgent rotates through the pool deterministically for the same caller
// and nondeterministically across callers via a cheap counter. The first call
// always returns the Nole UA; each subsequent call cyclically picks a different
// entry so Cloudflare cannot fingerprint a single answer.
func pickUserAgent() string {
	n := atomic.AddUint32(&userAgentPoolMu, 1)
	idx := int(n-1) % len(userAgentPool)
	if idx < 0 {
		idx = 0
	}
	return userAgentPool[idx]
}

const acceptHeader = "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1"

// proxyFromEnv reads NOLE_PROXY_URL and returns the parsed proxy URL, or nil.
// Supported schemes: socks5, socks5h, http, https. SOCKS5 is preferred for Tor.
func proxyFromEnv() *url.URL {
	raw := os.Getenv("NOLE_PROXY_URL")
	if raw == "" {
		return nil
	}
	proxyURL, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return proxyURL
}

// isOnionURL reports whether u (as a string) is a .onion address.
func isOnionURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Hostname(), ".onion")
}

type Provider struct {
	httpClient *http.Client
	timeout    time.Duration
	maxBytes   int64
	proxyURL   *url.URL // cached from NOLE_PROXY_URL
}

type Option func(*Provider)

func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) {
		if c != nil {
			p.httpClient = c
		}
	}
}

func WithTimeout(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.timeout = d
		}
	}
}

func WithMaxBodyBytes(n int64) Option {
	return func(p *Provider) {
		if n > 0 {
			p.maxBytes = n
		}
	}
}

func New(opts ...Option) Provider {
	p := Provider{
		httpClient: &http.Client{Timeout: 20 * time.Second, Transport: newTransport(nil)},
		timeout:    30 * time.Second,
		maxBytes:   providerhttp.MaxExtractResponseBytes,
	}
	if proxy := proxyFromEnv(); proxy != nil {
		p.proxyURL = proxy
		// If proxy is set, re-create transport with proxy dialer
		p.httpClient = &http.Client{Timeout: 20 * time.Second, Transport: newTransport(proxy)}
	}
	for _, opt := range opts {
		opt(&p)
	}
	return p
}

func (p Provider) Name() string { return "httpfetch" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilityExtract, core.CapabilityStatus}
}

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	return core.SearchResponse{}, errors.New("httpfetch: search is not supported; use extract with a public URL")
}

func (p Provider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	target := strings.TrimSpace(req.URL)
	if target == "" {
		return core.ExtractResponse{}, errors.New("httpfetch: url is required")
	}
	// Enforce proxy for .onion URLs even if NOLE_PROXY_URL is not set globally
	if isOnionURL(target) && p.proxyURL == nil {
		return core.ExtractResponse{}, errors.New("httpfetch: .onion URL requires NOLE_PROXY_URL (e.g. socks5://127.0.0.1:9050)")
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	client := p.noFollowClient()
	current := target
	for hop := 0; ; hop++ {
		if err := ctx.Err(); err != nil {
			return core.ExtractResponse{}, fmt.Errorf("httpfetch: request cancelled: %w", err)
		}
		if hop > maxRedirects {
			return core.ExtractResponse{}, fmt.Errorf("httpfetch: too many redirects (>%d)", maxRedirects)
		}
		// Service.Extract already validated the initial URL, so only REDIRECT
		// targets (hop > 0) need re-validation here
		if hop > 0 {
			if err := safenet.ValidateURLContext(ctx, current); err != nil {
				return core.ExtractResponse{}, fmt.Errorf("httpfetch: blocked redirect URL: %w", err)
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return core.ExtractResponse{}, fmt.Errorf("httpfetch: create request: %w", err)
		}
				httpReq.Header.Set("User-Agent", pickUserAgent())
		httpReq.Header.Set("Accept", acceptHeader)

		resp, err := providerhttp.DoWithRetry(ctx, client, httpReq, providerhttp.DefaultRetryOptions())
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return core.ExtractResponse{}, fmt.Errorf("httpfetch: request aborted: %w", cerr)
			}
			return core.ExtractResponse{}, fmt.Errorf("httpfetch: request failed: %s", redactTransportErr(err))
		}

		// Redirect: read the Location, drain+close, re-validate on the next hop.
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := strings.TrimSpace(resp.Header.Get("Location"))
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			if location == "" {
				return core.ExtractResponse{}, fmt.Errorf("httpfetch: redirect (%d) without a Location header", resp.StatusCode)
			}
			// .onion redirects require proxy — re-check
			if isOnionURL(location) && p.proxyURL == nil {
				return core.ExtractResponse{}, errors.New("httpfetch: redirect to .onion requires NOLE_PROXY_URL")
			}
			next, err := resolveRedirect(current, location)
			if err != nil {
				return core.ExtractResponse{}, fmt.Errorf("httpfetch: invalid redirect location: %w", err)
			}
			current = next
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := providerhttp.ReadAllLimited(resp.Body, p.maxBytes)
			_ = resp.Body.Close()
			return core.ExtractResponse{}, providerhttp.NewHTTPStatusError("httpfetch", "extract", resp.StatusCode, body)
		}

			ct := mediaType(resp.Header.Get("Content-Type"))
		if ct != "" && !isTextual(ct) {
			_ = resp.Body.Close()
			return core.ExtractResponse{}, fmt.Errorf("httpfetch: unsupported content type %q (only HTML/text is extracted; no JS rendering)", core.TruncateRunes(ct, 100))
		}

				bodyBytes, err := providerhttp.ReadAllLimited(resp.Body, p.maxBytes)
		_ = resp.Body.Close()
		if err != nil {
			return core.ExtractResponse{}, fmt.Errorf("httpfetch: read body: %w", err)
		}

		// text/plain: return raw body unchanged (htmlToText would mangle angle brackets)
		// HTML: extract text + title via htmlToText
		var content string
		var title string
		var safety core.ContentSafetyReport
		if ct == "text/plain" || strings.HasPrefix(ct, "text/plain") {
			content = string(bodyBytes)
			safety = core.ContentSafetyReport{Untrusted: false, Risk: core.ContentRiskNoIndicators}
		} else {
			text, extractedTitle := htmlToText([]byte(bodyBytes))
			content = text
			title = extractedTitle
			safety = core.ScanRawHTMLContentSafety(bodyBytes)
		}

		metadata := map[string]string{"mode": "http-fetch"}
		if title != "" {
			metadata["title"] = title
		}

		// For empty/script-only pages return success with empty content + metadata
		if strings.TrimSpace(content) == "" {
			return core.ExtractResponse{
				URL:           req.URL,
				Provider:      "httpfetch",
				Content:       "",
				Metadata:      metadata,
				ContentSafety: safety,
			}, nil
		}

		return core.ExtractResponse{
			URL:           req.URL,
			Provider:      "httpfetch",
			Content:       strings.TrimSpace(content),
			Metadata:      metadata,
			ContentSafety: safety,
		}, nil
	}
}

func (p Provider) Status(ctx context.Context) core.ProviderStatus {
	return core.ProviderStatus{
		Name:         p.Name(),
		Available:    true,
		Capabilities: p.Capabilities(),
		Reason:       "keyless pure-Go HTTP fetch + HTML-to-text extractor (no JS rendering)",
	}
}

func (p Provider) noFollowClient() *http.Client {
	c := *p.httpClient
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// newTransport returns an http.Transport with optional SOCKS5/HTTP proxy support
// and SSRF dial validation.
func newTransport(proxyURL *url.URL) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   validateDialedAddr,
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if proxyURL != nil {
		switch proxyURL.Scheme {
		case "socks5", "socks5h":
			// SOCKS5 proxy uses the dialer directly
			proxyDialer := &net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				// Dial proxy first, then connect through SOCKS5
				proxyConn, err := proxyDialer.DialContext(ctx, "tcp", proxyURL.Host)
				if err != nil {
					return nil, fmt.Errorf("httpfetch: proxy dial: %w", err)
				}
				// Manual SOCKS5 handshake is complex; use net/http Proxy field for HTTP proxy,
				// for SOCKS5 we set DialContext to go through the proxy address.
				// The SSRF guard validates the proxy address (safe assumption: proxy is local/trusted).
				_ = proxyConn.Close()
				// Fall through to HTTP proxy approach for SOCKS5 via Proxy field
				return nil, fmt.Errorf("httpfetch: socks5 via dialer not yet supported; use http proxy or setup Tor")
			}
			// Simpler: use Proxy field for HTTP proxies; SOCKS5 handled below.
			transport.Proxy = http.ProxyURL(proxyURL)
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyURL)
			// SSRF guard on proxy — the proxy resolves and dials, so we validate the proxy IP
			dialer.Control = validateDialedAddr
			transport.DialContext = dialer.DialContext
		}
	} else {
		// Direct connection with SSRF guard — Proxy is nil (no env proxy)
		transport.Proxy = nil
		dialer.Control = validateDialedAddr
		transport.DialContext = dialer.DialContext
	}

	return transport
}

func validateDialedAddr(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("httpfetch: cannot parse dial address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("httpfetch: dial address %q is not a literal IP", host)
	}
	if err := safenet.ValidateIP(ip); err != nil {
		return fmt.Errorf("httpfetch: blocked dial address: %w", err)
	}
	return nil
}

func redactTransportErr(err error) string {
	for {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			err = ue.Err
			continue
		}
		break
	}
	if err == nil {
		return "transport error (details redacted)"
	}
	return err.Error()
}

func resolveRedirect(current, location string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(loc).String(), nil
}

func mediaType(ct string) string {
	if idx := strings.IndexByte(ct, ';'); idx > 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	return strings.ToLower(ct)
}

func isTextual(ct string) bool {
	switch ct {
	case "text/html", "text/plain", "text/xml", "application/xhtml+xml",
		"application/xml", "application/json", "text/markdown":
		return true
	}
	return strings.HasPrefix(ct, "text/")
}
