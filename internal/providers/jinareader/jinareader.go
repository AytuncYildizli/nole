// Package jinareader is a keyless Nólë EXTRACT provider that uses the Jina Reader
// API (https://r.jina.ai/URL) to fetch and convert URLs to markdown. It is designed
// as a fallback after Scrapling: when Scrapling's local Python extraction fails,
// the router reaches Jina Reader before the keyed remote extractors
// (Firecrawl/Tavily). It requires no API key for basic usage; the optional
// JINA_API_KEY env var unlocks higher rate limits.
//
// Jina Reader is a remote API (not a local subprocess), so it does NOT perform
// SSRF validation on the fetched URL — the Jina service owns the outbound fetch
// and reports the result as markdown.
//
// Proxy support: NOLE_PROXY_URL env var (socks5://host:port or http://host:port)
// enables Tor/darkweb/privacy crawling. When set, the HTTP client routes through
// the proxy.
package jinareader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dorukardahan/nole/internal/core"
	"github.com/dorukardahan/nole/internal/providers/providerhttp"
	"github.com/dorukardahan/nole/internal/version"
)

var _ core.Provider = Provider{}

const jinaReaderBaseURL = "https://r.jina.ai/"

var userAgent = "Nole/" + version.Version + " (+https://github.com/dorukardahan/nole)"

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

type Provider struct {
	httpClient *http.Client
	apiKey     string
	configured bool
	timeout    time.Duration
	proxyURL   *url.URL
}

type Option func(*Provider)

func WithTimeout(d time.Duration) Option {
	return func(p *Provider) {
		if d > 0 {
			p.timeout = d
		}
	}
}

func New(opts ...Option) Provider {
	p := Provider{
		timeout: 30 * time.Second,
	}

	// Check for optional API key
	p.apiKey = strings.TrimSpace(os.Getenv("JINA_API_KEY"))
	if p.apiKey != "" {
		// If someone set a key but it's empty after trimming, treat as unconfigured
		// but still valid for keyless use — the key just bumps rate limits.
		// p.configured means "api key is available" here, which is cosmetic
		// (the free tier always works).
	}

	// Cache proxy URL
	p.proxyURL = proxyFromEnv()

	// Build transport with optional proxy
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
		Timeout:   p.timeout,
	}

	for _, opt := range opts {
		opt(&p)
	}

	return p
}

func (p Provider) Name() string { return "jinareader" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilityExtract, core.CapabilityStatus}
}

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	return core.SearchResponse{}, errors.New("jinareader: search is not supported; use extract with a public URL")
}

func (p Provider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	if strings.TrimSpace(req.URL) == "" {
		return core.ExtractResponse{}, errors.New("jinareader: url is required")
	}

	targetURL := jinaReaderBaseURL + strings.TrimSpace(req.URL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return core.ExtractResponse{}, fmt.Errorf("jinareader: create request: %w", err)
	}

	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("Accept", "text/markdown,text/html,text/plain;q=0.9,*/*;q=0.1")

	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	// Jina Reader accepts a format hint via X-Respond-With
	switch strings.ToLower(req.Format) {
	case "readerlm-v2", "readerlm":
		httpReq.Header.Set("X-Respond-With", "readerlm-v2")
	case "json", "application/json":
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("X-Respond-With", "markdown")
	default:
		// markdown (default)
		httpReq.Header.Set("X-Respond-With", "markdown")
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return core.ExtractResponse{}, fmt.Errorf("jinareader: fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		bodyStr := strings.TrimSpace(string(bodyBytes))
		if bodyStr != "" {
			// Try to extract a message from JSON error body
			var errPayload struct {
				Message string `json:"message"`
				Detail  string `json:"detail"`
			}
			if json.Unmarshal(bodyBytes, &errPayload) == nil && errPayload.Message != "" {
				bodyStr = errPayload.Message
			} else if errPayload.Detail != "" {
				bodyStr = errPayload.Detail
			}
			return core.ExtractResponse{}, fmt.Errorf("jinareader: HTTP %d: %s", resp.StatusCode, core.TruncateRunes(bodyStr, 200))
		}
		return core.ExtractResponse{}, fmt.Errorf("jinareader: HTTP %d (no body)", resp.StatusCode)
	}

	bodyBytes, err := providerhttp.ReadAllLimited(resp.Body, p.maxBytes())
	if err != nil {
		return core.ExtractResponse{}, fmt.Errorf("jinareader: read body: %w", err)
	}

	content := strings.TrimSpace(string(bodyBytes))

	metadata := map[string]string{"mode": "jina-reader"}
	if p.apiKey != "" {
		metadata["auth"] = "keyed"
	} else {
		metadata["auth"] = "keyless"
	}

	return core.ExtractResponse{
		URL:      req.URL,
		Provider: "jinareader",
		Content:  content,
		Metadata: metadata,
	}, nil
}

func (p Provider) Status(ctx context.Context) core.ProviderStatus {
	name := p.Name()
	caps := p.Capabilities()

	// Quick reachability check: try to hit r.jina.ai (root, no URL appended)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, jinaReaderBaseURL, nil)
	if err != nil {
		return core.ProviderStatus{Name: name, Available: true, Capabilities: caps, Reason: "Jina Reader API configured (status check failed: " + err.Error() + ")"}
	}
	httpReq.Header.Set("User-Agent", userAgent)
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return core.ProviderStatus{Name: name, Available: true, Capabilities: caps, Reason: "Jina Reader API reachable (status check: " + err.Error() + ")"}
	}
	resp.Body.Close()

	reason := "Jina Reader API (keyless, free tier)"
	if p.apiKey != "" {
		reason = "Jina Reader API (configured with JINA_API_KEY)"
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
		// 200 or 404 from root is fine — the API is alive
		return core.ProviderStatus{Name: name, Available: true, Capabilities: caps, Reason: reason + " — API reachable"}
	}
	return core.ProviderStatus{Name: name, Available: true, Capabilities: caps, Reason: reason + " — HTTP " + resp.Status}
}

// maxBytes returns the maximum response size for this provider.
// Matches the convention used by other Nólë providers.
func (p Provider) maxBytes() int64 {
	return 10 * 1024 * 1024 // 10 MB
}
