// Package ahmia provides the Ahmia.fi hidden-service search provider for Nólë.
// Ahmia indexes .onion hidden services on the Tor network — unlike DDGS onion
// endpoint (which is just DDG's clearnet index proxied through Tor), Ahmia
// actually crawls Tor hidden services and indexes .onion content directly.
//
// Proxy support: requires NOLE_PROXY_URL (SOCKS5 through Tor) for .onion
// discovery. Without a proxy, Ahmia clearnet endpoint returns the same HTML
// but the onion links may not resolve.
package ahmia

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/dorukardahan/nole/internal/core"
	"github.com/dorukardahan/nole/internal/providers/providerhttp"
)

const ahmiaEndpoint = "https://ahmia.fi/search/"

type Provider struct {
	httpClient *http.Client
}

var (
	reResult    = regexp.MustCompile(`<li class="search-result"[^>]*>(.*?)</li>`)
	reResultURL = regexp.MustCompile(`href="([^"]+)"`)
	reTitle     = regexp.MustCompile(`<a[^>]*>(.*?)</a>`)
	reSnippet   = regexp.MustCompile(`<p[^>]*>(.*?)</p>`)
	reStripTags = regexp.MustCompile(`<[^>]+>`)
	reHTMLEnt   = regexp.MustCompile(`&amp;|&#39;|&quot;|&lt;|&gt;`)
)

func New() Provider {
	transport := &http.Transport{
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	// Proxy support via NOLE_PROXY_URL
	if pu := os.Getenv("NOLE_PROXY_URL"); pu != "" {
		if proxyURL, err := url.Parse(pu); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	return Provider{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

func (p Provider) Name() string { return "ahmia" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilitySearch, core.CapabilityStatus}
}

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	form := url.Values{}
	form.Set("q", req.Query)

	endpoint := ahmiaEndpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+form.Encode(), nil)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("ahmia: create request: %w", err)
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "text/html")

	resp, err := providerhttp.DoWithRetry(ctx, p.httpClient, httpReq, providerhttp.DefaultRetryOptions())
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("ahmia: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
		return core.SearchResponse{}, providerhttp.NewHTTPStatusError("ahmia", "search", resp.StatusCode, body)
	}

	body, err := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("ahmia: read response: %w", err)
	}
	html := string(body)

	results := make([]core.SearchResult, 0)
	liMatches := reResult.FindAllStringSubmatch(html, -1)

	for _, li := range liMatches {
		liHTML := li[1]
		urlMatch := reResultURL.FindStringSubmatch(liHTML)
		titleMatch := reTitle.FindStringSubmatch(liHTML)
		snippetMatch := reSnippet.FindStringSubmatch(liHTML)

		if len(urlMatch) < 2 || len(titleMatch) < 2 {
			continue
		}

		link := urlMatch[1]
		title := cleanHTML(titleMatch[1])
		snippet := ""
		if len(snippetMatch) >= 2 {
			snippet = cleanHTML(snippetMatch[1])
		}
		snippet = core.TruncateRunes(snippet, 300)

		results = append(results, core.SearchResult{
			Title:    title,
			URL:      link,
			Snippet:  snippet,
			Provider: "ahmia",
		})

		if req.Limit > 0 && len(results) >= req.Limit {
			break
		}
	}

	return core.SearchResponse{
		Query:    req.Query,
		Task:     req.Task,
		Provider: "ahmia",
		Results:  results,
	}, nil
}

func (p Provider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	return core.ExtractResponse{}, fmt.Errorf("ahmia: extract not supported")
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
	s = reHTMLEnt.ReplaceAllStringFunc(s, func(match string) string {
		switch match {
		case "&amp;":
			return "&"
		case "&#39;":
			return "'"
		case "&quot;":
			return `"`
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		}
		return match
	})
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}
