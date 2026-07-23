// Package ddgs provides the DuckDuckGo Lite / HTML search provider for Nólë.
// It is the last-resort keyless search backstop and, when routed through Tor,
// also serves .onion search results via DuckDuckGo's onion service.
//
// Proxy support: NOLE_PROXY_URL env var (socks5://host:port or http://host:port)
// enables Tor/darkweb/privacy crawling. When set, the HTTP client routes through
// the proxy. When no proxy is set but Tor is detected on 127.0.0.1:9050, the
// provider will automatically proxy through it.
package ddgs

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dorukardahan/nole/internal/core"
	"github.com/dorukardahan/nole/internal/providers/providerhttp"
)

// DefaultTorHost is the default Tor SOCKS5 host.
const DefaultTorHost = "127.0.0.1"

// DefaultTorPort is the default Tor SOCKS5 port.
const DefaultTorPort = 9050

// ddgHTMLEndpoint is the standard DDG HTML search endpoint.
const ddgHTMLEndpoint = "https://html.duckduckgo.com/html/"

// ddgOnionEndpoint is the DuckDuckGo .onion search endpoint, reachable only
// via Tor. It performs identically to the clearnet html endpoint but keeps
// the search entirely inside the Tor network — no clearnet exit, end-to-end
// encryption between the client and DDG's hidden service.
const ddgOnionEndpoint = "http://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion/html/"

type Provider struct {
	httpClient *http.Client
	proxyURL   *url.URL
	proxySet   bool
	// useOnionEndpoint forces the .onion endpoint. Set when proxy is configured
	// or Tor is detected, so the search stays entirely inside the Tor network.
	useOnionEndpoint bool
}

// Option is a functional option for configuring the DDGS provider.
type Option func(*Provider)

// WithProxy sets a SOCKS5 or HTTP proxy URL on the provider.
func WithProxy(proxyURL *url.URL) Option {
	return func(p *Provider) {
		p.proxyURL = proxyURL
		p.proxySet = true
	}
}

// WithOnionEndpoint forces the .onion endpoint. Set automatically when proxy
// is configured, but can be force-disabled with the zero value.
func WithOnionEndpoint(enabled bool) Option {
	return func(p *Provider) {
		p.useOnionEndpoint = enabled
	}
}

// proxyFromEnv reads NOLE_PROXY_URL and returns the parsed proxy URL, or nil.
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

// detectTor checks whether a Tor SOCKS5 proxy is listening on the default port.
func detectTor() bool {
	addr := net.JoinHostPort(DefaultTorHost, strconv.Itoa(DefaultTorPort))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// New creates a DDGS provider with default settings.
func New(opts ...Option) Provider {
	p := Provider{}

	// Apply user options first (may set proxy explicitly)
	for _, opt := range opts {
		opt(&p)
	}

	// Auto-detect: check NOLE_PROXY_URL env, then Tor port
	if !p.proxySet {
		if pu := proxyFromEnv(); pu != nil {
			p.proxyURL = pu
			p.proxySet = true
		}
	}
	if !p.proxySet && detectTor() {
		p.proxyURL = &url.URL{
			Scheme: "socks5",
			Host:   net.JoinHostPort(DefaultTorHost, strconv.Itoa(DefaultTorPort)),
		}
		p.proxySet = true
	}

	// When proxied, use the .onion endpoint
	if p.proxySet {
		p.useOnionEndpoint = true
	}

	// Build transport
	transport := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if p.proxyURL != nil {
		transport.Proxy = http.ProxyURL(p.proxyURL)
	}

	p.httpClient = &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	return p
}

func (p Provider) Name() string { return "ddgs" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilitySearch, core.CapabilityStatus}
}

var (
	reResultLink    = regexp.MustCompile(`class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reResultSnippet = regexp.MustCompile(`class="result__snippet"[^>]*>(.*?)</a>`)
	reStripTags     = regexp.MustCompile(`<[^>]+>`)
	reHTMLEntity    = regexp.MustCompile(`&amp;`)
)

// searchEndpoint returns the appropriate search endpoint based on whether the
// .onion internal endpoint should be used.
func (p Provider) searchEndpoint() string {
	if p.useOnionEndpoint {
		return ddgOnionEndpoint
	}
	return ddgHTMLEndpoint
}

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	form := url.Values{}
	form.Set("q", req.Query)
	// DDG no-JS HTML endpoint expects b="" on the first page. Setting b="Web Search"
	// (the visible submit-button label) trips the anti-bot heuristic — SearXNG's
	// canonical implementation explicitly sends an empty string.
	form.Set("b", "")

	endpoint := p.searchEndpoint()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("ddgs: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	// Browser-parity headers required by DDG's bot blocker (Q3/Q4 2025 tightening).
	// Missing Sec-Fetch-* + Referer triggers immediate 202 Ratelimit on most queries.
	httpReq.Header.Set("Referer", "https://html.duckduckgo.com/")
	httpReq.Header.Set("Sec-Fetch-Dest", "document")
	httpReq.Header.Set("Sec-Fetch-Mode", "navigate")
	httpReq.Header.Set("Sec-Fetch-Site", "same-origin")
	httpReq.Header.Set("Sec-Fetch-User", "?1")

	resp, err := providerhttp.DoWithRetry(ctx, p.httpClient, httpReq, providerhttp.DefaultRetryOptions())
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("ddgs: search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		// DDG signals rate-limit / bot-block with HTTP 202
		body, _ := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
		return core.SearchResponse{}, fmt.Errorf("ddgs: rate limited (HTTP 202; response body redacted, %d bytes)", len(body))
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
		return core.SearchResponse{}, providerhttp.NewHTTPStatusError("ddgs", "search", resp.StatusCode, body)
	}

	bodyBytes, err := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("ddgs: read response: %w", err)
	}
	html := string(bodyBytes)

	linkMatches := reResultLink.FindAllStringSubmatchIndex(html, -1)
	snippetMatches := reResultSnippet.FindAllStringSubmatchIndex(html, -1)

	results := make([]core.SearchResult, 0)

	for i, lm := range linkMatches {
		href := reHTMLEntity.ReplaceAllString(html[lm[2]:lm[3]], "&")
		title := cleanHTML(html[lm[4]:lm[5]])

		// Skip ad redirects
		if strings.Contains(href, "duckduckgo.com/y.js") || strings.Contains(href, "bing.com/aclick") {
			continue
		}

		nextLinkStart := len(html)
		if i+1 < len(linkMatches) {
			nextLinkStart = linkMatches[i+1][0]
		}
		snippet := ""
		for _, sm := range snippetMatches {
			if sm[0] >= lm[0] && sm[0] < nextLinkStart {
				snippet = cleanHTML(html[sm[2]:sm[3]])
				break
			}
		}
		snippet = core.TruncateRunes(snippet, 300)

		results = append(results, core.SearchResult{
			Title:    title,
			URL:      href,
			Snippet:  snippet,
			Provider: "ddgs",
		})

		if req.Limit > 0 && len(results) >= req.Limit {
			break
		}
	}

	return core.SearchResponse{
		Query:    req.Query,
		Task:     req.Task,
		Provider: "ddgs",
		Results:  results,
	}, nil
}

func (p Provider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	return core.ExtractResponse{}, fmt.Errorf("ddgs: extract not supported; use tavily or firecrawl")
}

func (p Provider) Status(ctx context.Context) core.ProviderStatus {
	return core.ProviderStatus{
		Name:         p.Name(),
		Available:    true,
		Capabilities: p.Capabilities(),
	}
}

func cleanHTML(s string) string {
	s = reStripTags.ReplaceAllString(s, "")
	s = reHTMLEntity.ReplaceAllString(s, "&")
	s = strings.TrimSpace(s)
	// Decode common HTML entities
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "  ", " ")
	return strings.TrimSpace(s)
}
