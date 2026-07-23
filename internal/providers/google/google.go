// Package google provides the Google Programmable Search Engine (gPSE) JSON API
// search provider for Nólë. It uses the free-tier Google Custom Search JSON API
// (100 queries/day free), reading the API key from NOLE_GOOGLE_PSE_KEY and the
// engine ID from NOLE_GOOGLE_PSE_CX (a default generic web search engine is
// included as a fallback).
//
// Proxy support: NOLE_PROXY_URL env var (socks5://host:port or http://host:port)
// enables proxied crawling. When set, the HTTP client routes through the proxy.
package google

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/dorukardahan/nole/internal/core"
	"github.com/dorukardahan/nole/internal/providers/providerhttp"
)

// DefaultEngineID is the default Google Programmable Search Engine ID for
// generic web search. Created under the dorukardahan GCP project.
const DefaultEngineID = "013479563781538683077:z0yh_l_pvyi"

// GoogleSearchEndpoint is the Google Custom Search JSON API endpoint.
const GoogleSearchEndpoint = "https://www.googleapis.com/customsearch/v1"

// Provider implements the core.Provider interface for Google Programmable Search.
type Provider struct {
	apiKey     string
	engineID   string
	httpClient *http.Client
	breaker    *providerhttp.Breaker
}

// Option is a functional option for configuring the Google provider.
type Option func(*Provider)

// WithAPIKey sets the Google PSE API key.
func WithAPIKey(key string) Option {
	return func(p *Provider) { p.apiKey = key }
}

// WithEngineID sets the custom search engine ID.
func WithEngineID(id string) Option {
	return func(p *Provider) { p.engineID = id }
}

// WithBreaker attaches a circuit breaker so persistent upstream failures
// short-circuit fast instead of burning the per-call timeout + retry budget. A
// nil breaker (the default) leaves behaviour unchanged.
func WithBreaker(b *providerhttp.Breaker) Option {
	return func(p *Provider) { p.breaker = b }
}

// New creates a Google PSE provider with settings from env vars.
func New(opts ...Option) Provider {
	p := Provider{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(&p)
	}
	if p.apiKey == "" {
		p.apiKey = os.Getenv("NOLE_GOOGLE_PSE_KEY")
	}
	if p.engineID == "" {
		p.engineID = os.Getenv("NOLE_GOOGLE_PSE_CX")
	}
	if p.engineID == "" {
		p.engineID = DefaultEngineID
	}

	// Build transport with optional proxy support
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

func (p Provider) Name() string { return "google" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilitySearch, core.CapabilityStatus}
}

// --- Google Custom Search JSON API response types ---

type googleSearchResponse struct {
	Items            []googleSearchItem `json:"items,omitempty"`
	SearchInformation *searchInformation `json:"searchInformation,omitempty"`
	Error            *googleAPIError    `json:"error,omitempty"`
}

type googleSearchItem struct {
	Title   string `json:"title"`
	Link    string `json:"link"`
	Snippet string `json:"snippet"`
}

type searchInformation struct {
	TotalResults string `json:"totalResults"`
}

type googleAPIError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Errors  []googleAPIErrorDetail `json:"errors,omitempty"`
}

type googleAPIErrorDetail struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	if p.apiKey == "" {
		return core.SearchResponse{}, fmt.Errorf("google: NOLE_GOOGLE_PSE_KEY not set")
	}

	params := url.Values{}
	params.Set("key", p.apiKey)
	params.Set("cx", p.engineID)
	params.Set("q", req.Query)

	// Google CSE returns up to 10 results per query. Set num to the caller's
	// limit, clamped to [1, 10].
	num := req.Limit
	if num <= 0 {
		num = 5
	}
	if num > 10 {
		num = 10
	}
	params.Set("num", strconv.Itoa(num))

	// Optional: country/region biasing through Google's gl parameter
	if req.Options.Country != "" {
		params.Set("gl", req.Options.Country)
	}
	// Interface language for the snippet text (hl)
	if req.Options.UILang != "" {
		params.Set("hl", req.Options.UILang)
	}
	// SafeSearch level: active, moderate (default), or off
	if req.Options.SafeSearch != "" {
		params.Set("safe", req.Options.SafeSearch)
	}

	u := fmt.Sprintf("%s?%s", GoogleSearchEndpoint, params.Encode())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("google: create request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := providerhttp.DoWithRetryBreaker(ctx, p.httpClient, httpReq, providerhttp.DefaultRetryOptions(), p.breaker)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("google: search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := providerhttp.ReadAllLimited(resp.Body, providerhttp.MaxSearchResponseBytes)
		return core.SearchResponse{}, providerhttp.NewHTTPStatusError("google", "search", resp.StatusCode, respBody)
	}

	var gresp googleSearchResponse
	if err := providerhttp.DecodeJSONLimited(resp.Body, providerhttp.MaxSearchResponseBytes, &gresp); err != nil {
		return core.SearchResponse{}, fmt.Errorf("google: decode response: %w", err)
	}

	// Check for API-level error in the response body (Google returns HTTP 200
	// with an error field for some failures, e.g. daily quota exceeded)
	if gresp.Error != nil {
		errMsg := gresp.Error.Message
		// Surface 429 (rate limit exceeded) clearly
		if gresp.Error.Code == 429 || len(gresp.Error.Errors) > 0 && gresp.Error.Errors[0].Reason == "rateLimitExceeded" {
			return core.SearchResponse{}, fmt.Errorf("google: rate limited: %s", errMsg)
		}
		return core.SearchResponse{}, fmt.Errorf("google: API error (HTTP %d): %s", gresp.Error.Code, errMsg)
	}

	results := make([]core.SearchResult, 0, len(gresp.Items))
	for _, item := range gresp.Items {
		results = append(results, core.SearchResult{
			Title:    item.Title,
			URL:      item.Link,
			Snippet:  core.TruncateRunes(item.Snippet, 300),
			Provider: "google",
		})
	}

	return core.SearchResponse{
		Query:    req.Query,
		Task:     req.Task,
		Provider: "google",
		Results:  results,
	}, nil
}

func (p Provider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	return core.ExtractResponse{}, fmt.Errorf("google: extract not supported; use tavily or firecrawl")
}

func (p Provider) Status(ctx context.Context) core.ProviderStatus {
	if p.apiKey == "" {
		return core.ProviderStatus{
			Name:         p.Name(),
			Available:    false,
			Capabilities: p.Capabilities(),
			Reason:       "NOLE_GOOGLE_PSE_KEY not set",
		}
	}
	state, consecFails, openedAt := providerhttp.BreakerStatusFields(p.breaker)
	status := core.ProviderStatus{
		Name:               p.Name(),
		Available:          true,
		Capabilities:       p.Capabilities(),
		BreakerState:       state,
		BreakerConsecFails: consecFails,
		BreakerOpenedAt:    openedAt,
	}
	if p.breaker != nil && p.breaker.IsOpen() {
		status.Available = false
		status.Reason = "circuit_open"
	}
	return status
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
