// Package haystack provides the Haystak.com darkweb and clearnet search
// provider for Nólë. Haystak indexes both clearnet web content and .onion
// hidden services, making it a general-purpose search engine that also covers
// the dark web.
//
// This provider uses GET requests to Haystak's search endpoint and parses
// the HTML response to extract result titles, URLs, and snippets. No API
// key is needed — the free web search tier is available to everyone.
//
// Proxy support: NOLE_PROXY_URL env var (socks5://host:port) enables Tor
// routing. When set, the HTTP request goes through the SOCKS5 proxy. When
// no proxy is set but Tor is detected on 127.0.0.1:9050, the provider
// auto-configures the proxy.
//
// Clearnet fallback: when Tor is not available, the provider falls back to
// https://haystak.com directly.
package haystack

import (
	"context"
	"fmt"
	"io"
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

// clearnetSearchURL is the Haystak clearnet search endpoint.
const clearnetSearchURL = "https://haystak.com/search/index.php"

// Provider implements the core.Provider interface for Haystak search.
type Provider struct {
	httpClient *http.Client
}

// New creates a Haystak provider with optional proxy support.
func New() Provider {
	p := Provider{}

	transport := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if pu := proxyFromEnv(); pu != nil {
		transport.Proxy = http.ProxyURL(pu)
	}

	p.httpClient = &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	return p
}

func (p Provider) Name() string { return "haystack" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilitySearch, core.CapabilityStatus}
}

// HTML regex patterns for parsing Haystak search results.
var (
	reResultItem     = regexp.MustCompile(`(?s)<div\s+class="result"[^>]*>(.*?)</div>\s*(?:</div>)?`)
	reResultTitle    = regexp.MustCompile(`<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reResultAltTitle = regexp.MustCompile(`class="search_title"[^>]*>(.*?)</div>`)
	reResultURL      = regexp.MustCompile(`class="search_url"[^>]*>(.*?)</div>`)
	reResultSnippet  = regexp.MustCompile(`class="search_result"[^>]*>(.*?)</div>`)
	reLinkInResult   = regexp.MustCompile(`<(?:a|A)\s+[^>]*href="([^"]+)"`)
	reStripTags      = regexp.MustCompile(`<[^>]+>`)
	reHTMLEntity     = regexp.MustCompile(`&amp;`)
)

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	// Build search URL
	u, err := url.Parse(clearnetSearchURL)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("haystack: parse endpoint: %w", err)
	}
	q := u.Query()
	q.Set("q", req.Query)
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("haystack: create request: %w", err)
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := providerhttp.DoWithRetry(ctx, p.httpClient, httpReq, providerhttp.DefaultRetryOptions())
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("haystack: search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
		return core.SearchResponse{}, providerhttp.NewHTTPStatusError("haystack", "search", resp.StatusCode, body)
	}

	bodyBytes, err := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
	if err != nil && err != io.EOF {
		return core.SearchResponse{}, fmt.Errorf("haystack: read response: %w", err)
	}
	html := string(bodyBytes)

	results := parseResults(html, req.Limit)

	return core.SearchResponse{
		Query:    req.Query,
		Task:     req.Task,
		Provider: "haystack",
		Results:  results,
	}, nil
}

func (p Provider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	return core.ExtractResponse{}, fmt.Errorf("haystack: extract not supported; use tavily or firecrawl")
}

func (p Provider) Status(ctx context.Context) core.ProviderStatus {
	return core.ProviderStatus{
		Name:         p.Name(),
		Available:    true,
		Capabilities: p.Capabilities(),
	}
}

// parseResults extracts search results from Haystak HTML.
func parseResults(html string, limit int) []core.SearchResult {
	var results []core.SearchResult

	// Try multiple parsing strategies since Haystak HTML structure can vary.

	// Strategy 1: Find result blocks by div class="result"
	resultBlocks := reResultItem.FindAllStringSubmatch(html, -1)
	for _, block := range resultBlocks {
		inner := block[1]
		title, urlStr, snippet := extractFromBlock(inner)

		// If class="result" parsing didn't yield a URL, try strategy 2
		if urlStr == "" {
			title, urlStr, snippet = extractFromSearchResult(inner)
		}

		if title == "" && urlStr == "" {
			continue
		}
		title = cleanHTML(title)
		urlStr = cleanURL(urlStr)
		snippet = cleanHTML(snippet)

		if title == "" && urlStr == "" {
			continue
		}

		results = append(results, core.SearchResult{
			Title:    title,
			URL:      urlStr,
			Snippet:  core.TruncateRunes(snippet, 300),
			Provider: "haystack",
		})

		if limit > 0 && len(results) >= limit {
			break
		}
	}

	// Strategy 3: If no results found via div blocks, try direct link harvesting
	if len(results) == 0 {
		results = append(results, parseLinkBlocks(html, limit)...)
	}

	return results
}

// extractFromBlock tries to parse a search result from a div block.
func extractFromBlock(inner string) (title, urlStr, snippet string) {
	// Try finding an anchor with href
	if m := reLinkInResult.FindStringSubmatch(inner); len(m) > 1 {
		urlStr = m[1]
	}

	// Try finding title text
	if m := reResultTitle.FindStringSubmatch(inner); len(m) > 2 {
		title = m[2]
	}

	// Try finding snippet from search_result class
	if m := reResultSnippet.FindStringSubmatch(inner); len(m) > 1 {
		snippet = m[1]
	}

	return
}

// extractFromSearchResult tries to parse using search_title/search_url/search_result classes.
func extractFromSearchResult(inner string) (title, urlStr, snippet string) {
	if m := reResultAltTitle.FindStringSubmatch(inner); len(m) > 1 {
		title = m[1]
	}
	if m := reResultURL.FindStringSubmatch(inner); len(m) > 1 {
		urlStr = m[1]
	}
	if m := reResultSnippet.FindStringSubmatch(inner); len(m) > 1 {
		snippet = m[1]
	}
	return
}

// parseLinkBlocks finds result-like link blocks when div.result parsing fails.
func parseLinkBlocks(html string, limit int) []core.SearchResult {
	var results []core.SearchResult

	// Find any anchor with href and nearby text
	linkBlocks := regexp.MustCompile(`(?s)<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`).FindAllStringSubmatch(html, -1)
	for _, m := range linkBlocks {
		href := m[1]
		text := cleanHTML(m[2])
		if text == "" || href == "" {
			continue
		}
		// Skip navigation/same-page links
		if strings.HasPrefix(href, "#") || strings.HasPrefix(href, "/") {
			continue
		}
		if !strings.HasPrefix(href, "http") {
			continue
		}

		results = append(results, core.SearchResult{
			Title:    text,
			URL:      href,
			Provider: "haystack",
		})

		if limit > 0 && len(results) >= limit {
			break
		}
	}

	return results
}

// cleanHTML removes HTML tags and decodes common entities.
func cleanHTML(s string) string {
	s = reStripTags.ReplaceAllString(s, "")
	s = reHTMLEntity.ReplaceAllString(s, "&")
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&#x27;", "'")
	s = strings.ReplaceAll(s, "&#x2F;", "/")
	s = strings.ReplaceAll(s, "\n", " ")
	// Collapse multiple spaces
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// cleanURL normalizes a URL extracted from HTML.
func cleanURL(u string) string {
	u = cleanHTML(u)
	u = strings.TrimSpace(u)
	return u
}

// proxyFromEnv reads NOLE_PROXY_URL and returns the parsed proxy URL, or nil.
func proxyFromEnv() *url.URL {
	raw := os.Getenv("NOLE_PROXY_URL")
	if raw == "" {
		// Auto-detect Tor
		addr := net.JoinHostPort(DefaultTorHost, strconv.Itoa(DefaultTorPort))
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			proxyURL, _ := url.Parse(fmt.Sprintf("socks5://%s", addr))
			return proxyURL
		}
		return nil
	}
	proxyURL, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return proxyURL
}
