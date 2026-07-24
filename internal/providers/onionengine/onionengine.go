// Package onionengine provides the OnionEngine.com search provider for Nólë.
// OnionEngine indexes enterprise threat intelligence and public web content,
// including .onion hidden services. The free tier provides public web search
// without requiring an API key.
//
// This provider uses GET requests to OnionEngine's search endpoint and parses
// the HTML response to extract result titles, URLs, and snippets.
//
// Proxy support: NOLE_PROXY_URL env var (socks5://host:port) enables Tor
// routing. When set, the HTTP request goes through the SOCKS5 proxy. When
// no proxy is set but Tor is detected on 127.0.0.1:9050, the provider
// auto-configures the proxy.
//
// Clearnet fallback: when Tor is not available, the provider falls back to
// https://onionengine.com directly.
package onionengine

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

// searchEndpoint is the OnionEngine clearnet search endpoint.
const searchEndpoint = "https://onionengine.com/search/"

// Provider implements the core.Provider interface for OnionEngine search.
type Provider struct {
	httpClient *http.Client
}

// New creates an OnionEngine provider with optional proxy support.
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

func (p Provider) Name() string { return "onionengine" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilitySearch, core.CapabilityStatus}
}

// HTML regex patterns for parsing OnionEngine search results.
var (
	// OnionEngine typically uses div.result blocks similar to other search engines.
	reResultItem     = regexp.MustCompile(`(?s)<div\s+class="result"[^>]*>(.*?)</div>\s*</div>`)
	reResultItemLax  = regexp.MustCompile(`(?s)<div\s+class="result"[^>]*>(.*?)(?:</div>\s*){1,2}`)
	reAnchor         = regexp.MustCompile(`<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reResultTitle    = regexp.MustCompile(`class="[^"]*title[^"]*"[^>]*>(.*?)</div>`)
	reResultTitleA   = regexp.MustCompile(`class="[^"]*title[^"]*"[^>]*><a[^>]*>(.*?)</a>`)
	reResultURL      = regexp.MustCompile(`class="[^"]*url[^"]*"[^>]*>(.*?)</div>`)
	reResultDomain   = regexp.MustCompile(`class="[^"]*domain[^"]*"[^>]*>(.*?)</div>`)
	reResultSnippet  = regexp.MustCompile(`class="[^"]*(?:snippet|description|desc)[^"]*"[^>]*>(.*?)</div>`)
	reResultDesc     = regexp.MustCompile(`class="[^"]*desc[^"]*"[^>]*>(.*?)</div>`)
	reLinkInDiv      = regexp.MustCompile(`<(?:a|A)\s+[^>]*href="([^"]+)"`)
	reStripTags      = regexp.MustCompile(`<[^>]+>`)
	reHTMLEntity     = regexp.MustCompile(`&amp;`)
	reResultBlocks   = regexp.MustCompile(`(?s)(?:<div[^>]*class="[^"]*result[^"]*"[^>]*>.*?</div>\s*</div>)`)
)

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	// Build search URL
	u, err := url.Parse(searchEndpoint)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("onionengine: parse endpoint: %w", err)
	}
	q := u.Query()
	q.Set("q", req.Query)
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("onionengine: create request: %w", err)
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := providerhttp.DoWithRetry(ctx, p.httpClient, httpReq, providerhttp.DefaultRetryOptions())
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("onionengine: search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
		return core.SearchResponse{}, providerhttp.NewHTTPStatusError("onionengine", "search", resp.StatusCode, body)
	}

	bodyBytes, err := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("onionengine: read response: %w", err)
	}
	html := string(bodyBytes)

	results := parseResults(html, req.Limit)

	return core.SearchResponse{
		Query:    req.Query,
		Task:     req.Task,
		Provider: "onionengine",
		Results:  results,
	}, nil
}

func (p Provider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	return core.ExtractResponse{}, fmt.Errorf("onionengine: extract not supported; use tavily or firecrawl")
}

func (p Provider) Status(ctx context.Context) core.ProviderStatus {
	return core.ProviderStatus{
		Name:         p.Name(),
		Available:    true,
		Capabilities: p.Capabilities(),
	}
}

// parseResults extracts search results from OnionEngine HTML.
func parseResults(html string, limit int) []core.SearchResult {
	var results []core.SearchResult

	// Strategy 1: Find result blocks by div class="result"
	blocks := findResultBlocks(html)

	for _, block := range blocks {
		title, urlStr, snippet := extractResultFromBlock(block)
		if title == "" && urlStr == "" {
			continue
		}

		results = append(results, core.SearchResult{
			Title:    cleanHTML(title),
			URL:      cleanURL(urlStr),
			Snippet:  core.TruncateRunes(cleanHTML(snippet), 300),
			Provider: "onionengine",
		})

		if limit > 0 && len(results) >= limit {
			break
		}
	}

	// Strategy 2: Direct anchor extraction from the document
	if len(results) == 0 {
		results = append(results, extractDirectAnchors(html, limit)...)
	}

	return results
}

// findResultBlocks finds search result div blocks in the HTML.
func findResultBlocks(html string) []string {
	// Try strict div.result pattern first
	if blocks := reResultBlocks.FindAllString(html, -1); len(blocks) > 0 {
		return blocks
	}

	// Fall back to inner content matches
	var blocks []string
	for _, m := range reResultItem.FindAllStringSubmatch(html, -1) {
		if len(m) > 1 {
			blocks = append(blocks, m[1])
		}
	}
	if len(blocks) > 0 {
		return blocks
	}

	// Lax match
	for _, m := range reResultItemLax.FindAllStringSubmatch(html, -1) {
		if len(m) > 1 {
			blocks = append(blocks, m[1])
		}
	}
	return blocks
}

// extractResultFromBlock extracts title, URL, and snippet from a result block.
func extractResultFromBlock(block string) (title, urlStr, snippet string) {
	// Look for anchor with href inside the block
	if m := reLinkInDiv.FindStringSubmatch(block); len(m) > 1 {
		urlStr = m[1]
	}

	// Try title class
	if m := reResultTitle.FindStringSubmatch(block); len(m) > 1 {
		title = m[1]
	}

	// Try anchor inside title
	if m := reResultTitleA.FindStringSubmatch(block); len(m) > 1 {
		title = m[1]
	}

	// If no title from class, try anchor text
	if title == "" {
		if m := reAnchor.FindStringSubmatch(block); len(m) > 2 {
			if title == "" {
				title = m[2]
			}
		}
	}

	// Try URL/domain classes
	if urlStr == "" {
		if m := reResultURL.FindStringSubmatch(block); len(m) > 1 {
			urlStr = m[1]
		}
	}
	if urlStr == "" {
		if m := reResultDomain.FindStringSubmatch(block); len(m) > 1 {
			urlStr = m[1]
		}
	}

	// Try snippet/description classes
	if m := reResultSnippet.FindStringSubmatch(block); len(m) > 1 {
		snippet = m[1]
	}
	if snippet == "" {
		if m := reResultDesc.FindStringSubmatch(block); len(m) > 1 {
			snippet = m[1]
		}
	}

	return
}

// extractDirectAnchors falls back to extracting links directly from the HTML.
func extractDirectAnchors(html string, limit int) []core.SearchResult {
	var results []core.SearchResult

	anchors := reAnchor.FindAllStringSubmatch(html, -1)
	for _, m := range anchors {
		if len(m) < 3 {
			continue
		}
		href := cleanURL(m[1])
		text := cleanHTML(m[2])

		if text == "" || href == "" {
			continue
		}
		if strings.HasPrefix(href, "#") {
			continue
		}
		if strings.HasPrefix(href, "/") {
			// Could be a relative link — qualify it
			href = "https://onionengine.com" + href
		}
		if !strings.HasPrefix(href, "http") {
			continue
		}

		results = append(results, core.SearchResult{
			Title:    text,
			URL:      href,
			Provider: "onionengine",
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
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// cleanURL normalizes a URL extracted from HTML.
func cleanURL(u string) string {
	u = cleanHTML(u)
	return strings.TrimSpace(u)
}

// proxyFromEnv reads NOLE_PROXY_URL and returns the parsed proxy URL, or nil.
func proxyFromEnv() *url.URL {
	raw := os.Getenv("NOLE_PROXY_URL")
	if raw == "" {
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
