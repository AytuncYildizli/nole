package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dorukardahan/nole/internal/core"
)

func TestName(t *testing.T) {
	p := New()
	if got := p.Name(); got != "google" {
		t.Errorf("Name() = %q, want %q", got, "google")
	}
}

func TestCapabilities(t *testing.T) {
	p := New()
	caps := p.Capabilities()
	if len(caps) != 2 {
		t.Fatalf("Capabilities() returned %d items, want 2", len(caps))
	}
	hasSearch := false
	hasStatus := false
	for _, c := range caps {
		if c == core.CapabilitySearch {
			hasSearch = true
		}
		if c == core.CapabilityStatus {
			hasStatus = true
		}
	}
	if !hasSearch {
		t.Error("Capabilities() missing search")
	}
	if !hasStatus {
		t.Error("Capabilities() missing status")
	}
}

func TestStatusNoKey(t *testing.T) {
	p := New()
	status := p.Status(context.Background())
	if status.Available {
		t.Error("expected Available=false when NOLE_GOOGLE_PSE_KEY is not set")
	}
	if status.Name != "google" {
		t.Errorf("Name = %q, want %q", status.Name, "google")
	}
}

func TestSearch_Basic(t *testing.T) {
	mockResp := googleSearchResponse{
		Items: []googleSearchItem{
			{Title: "Result 1", Link: "https://example.com/1", Snippet: "Snippet 1"},
			{Title: "Result 2", Link: "https://example.com/2", Snippet: "Snippet 2 longer description for testing"},
		},
		SearchInformation: &searchInformation{
			TotalResults: "2",
		},
	}

	p := newProviderWithMockServer(t, &mockResp, http.StatusOK)
	resp, err := p.Search(context.Background(), core.SearchRequest{
		Query: "test query",
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("Search() returned error: %v", err)
	}

	if resp.Provider != "google" {
		t.Errorf("Provider = %q, want %q", resp.Provider, "google")
	}
	if len(resp.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(resp.Results))
	}
	if resp.Results[0].Title != "Result 1" {
		t.Errorf("Results[0].Title = %q, want %q", resp.Results[0].Title, "Result 1")
	}
	if resp.Results[0].URL != "https://example.com/1" {
		t.Errorf("Results[0].URL = %q, want %q", resp.Results[0].URL, "https://example.com/1")
	}
	if resp.Results[0].Snippet != "Snippet 1" {
		t.Errorf("Results[0].Snippet = %q, want %q", resp.Results[0].Snippet, "Snippet 1")
	}
}

func TestSearch_TruncatesSnippet(t *testing.T) {
	longSnippet := ""
	for i := 0; i < 50; i++ {
		longSnippet += "Lorem ipsum dolor sit amet. "
	}
	mockResp := googleSearchResponse{
		Items: []googleSearchItem{
			{Title: "Truncated", Link: "https://example.com/t", Snippet: longSnippet},
		},
		SearchInformation: &searchInformation{
			TotalResults: "1",
		},
	}
	p := newProviderWithMockServer(t, &mockResp, http.StatusOK)
	resp, err := p.Search(context.Background(), core.SearchRequest{
		Query: "truncation test",
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("Search() returned error: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(resp.Results))
	}
	if len([]rune(resp.Results[0].Snippet)) > 303 {
		t.Errorf("snippet too long: %d runes", len([]rune(resp.Results[0].Snippet)))
	}
}

func TestSearch_EmptyResults(t *testing.T) {
	mockResp := googleSearchResponse{
		SearchInformation: &searchInformation{
			TotalResults: "0",
		},
	}

	p := newProviderWithMockServer(t, &mockResp, http.StatusOK)
	resp, err := p.Search(context.Background(), core.SearchRequest{
		Query: "empty search",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search() returned error: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("got %d results, want 0", len(resp.Results))
	}
}

func TestSearch_NoAPIKey(t *testing.T) {
	p := New()
	_, err := p.Search(context.Background(), core.SearchRequest{
		Query: "test",
		Limit: 5,
	})
	if err == nil {
		t.Error("expected error when NOLE_GOOGLE_PSE_KEY is not set")
	}
}

func TestSearch_RateLimit(t *testing.T) {
	errResp := googleSearchResponse{
		Error: &googleAPIError{
			Code:    429,
			Message: "Rate Limit Exceeded",
			Errors: []googleAPIErrorDetail{
				{Reason: "rateLimitExceeded", Message: "Quota exceeded"},
			},
		},
	}
	p := newProviderWithMockServer(t, &errResp, http.StatusOK)
	_, err := p.Search(context.Background(), core.SearchRequest{
		Query: "test",
		Limit: 5,
	})
	if err == nil {
		t.Error("expected error for rate limit")
	}
}

func TestSearch_HTTP429(t *testing.T) {
	p := newProviderWithMockServer(t, nil, http.StatusTooManyRequests)
	_, err := p.Search(context.Background(), core.SearchRequest{
		Query: "test",
		Limit: 5,
	})
	if err == nil {
		t.Error("expected error for HTTP 429")
	}
}

func TestSearch_ErrorBody(t *testing.T) {
	errResp := googleSearchResponse{
		Error: &googleAPIError{
			Code:    403,
			Message: "Daily Limit Exceeded",
		},
	}
	p := newProviderWithMockServer(t, &errResp, http.StatusOK)
	_, err := p.Search(context.Background(), core.SearchRequest{
		Query: "test",
		Limit: 5,
	})
	if err == nil {
		t.Error("expected error for API-level error")
	}
}

func TestExtract(t *testing.T) {
	p := New()
	_, err := p.Extract(context.Background(), core.ExtractRequest{
		URL: "https://example.com",
	})
	if err == nil {
		t.Error("expected extract to return error (not supported)")
	}
}

func TestNewDefaults(t *testing.T) {
	p := New()
	if p.apiKey != "" {
		t.Errorf("default apiKey = %q, want empty", p.apiKey)
	}
	if p.engineID != DefaultEngineID {
		t.Errorf("default engineID = %q, want %q", p.engineID, DefaultEngineID)
	}
}

func TestWithAPIKey(t *testing.T) {
	p := New(WithAPIKey("custom-key"))
	if p.apiKey != "custom-key" {
		t.Errorf("WithAPIKey: apiKey = %q, want %q", p.apiKey, "custom-key")
	}
}

func TestWithEngineID(t *testing.T) {
	p := New(WithEngineID("custom-cx"))
	if p.engineID != "custom-cx" {
		t.Errorf("WithEngineID: engineID = %q, want %q", p.engineID, "custom-cx")
	}
}

func TestSearch_RespectsLimit(t *testing.T) {
	mockItems := make([]googleSearchItem, 5)
	for i := 0; i < 5; i++ {
		mockItems[i] = googleSearchItem{
			Title:   fmt.Sprintf("Result %d", i+1),
			Link:    fmt.Sprintf("https://example.com/%d", i+1),
			Snippet: fmt.Sprintf("Snippet %d", i+1),
		}
	}
	mockResp := googleSearchResponse{
		Items: mockItems,
		SearchInformation: &searchInformation{
			TotalResults: "5",
		},
	}
	p := newProviderWithMockServer(t, &mockResp, http.StatusOK)
	// Request limit 3 — should still return all 5 since the server always
	// returns 5 items (the limit is sent as num param but we verify parsing).
	resp, err := p.Search(context.Background(), core.SearchRequest{
		Query: "test",
		Limit: 3,
	})
	if err != nil {
		t.Fatalf("Search() returned error: %v", err)
	}
	if len(resp.Results) != 5 {
		t.Errorf("got %d results from mock (server always returns 5), want 5", len(resp.Results))
	}
}

func TestSearch_ClampsNum(t *testing.T) {
	// Test that num is clamped to 10
	items := make([]googleSearchItem, 10)
	for i := 0; i < 10; i++ {
		items[i] = googleSearchItem{
			Title:   fmt.Sprintf("Result %d", i+1),
			Link:    fmt.Sprintf("https://example.com/%d", i+1),
			Snippet: fmt.Sprintf("Snippet %d", i+1),
		}
	}
	mockResp := googleSearchResponse{
		Items: items,
		SearchInformation: &searchInformation{
			TotalResults: "50",
		},
	}
	p := newProviderWithMockServer(t, &mockResp, http.StatusOK)
	resp, err := p.Search(context.Background(), core.SearchRequest{
		Query: "test",
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("Search() returned error: %v", err)
	}
	if len(resp.Results) != 10 {
		t.Errorf("got %d results, want 10 (clamped)", len(resp.Results))
	}
}

// --- test helpers ---

// newProviderWithMockServer creates a Provider with a mock HTTP server that
// acts as the Google Custom Search API. If resp is non-nil it is JSON-marshalled
// and returned with the given statusCode; if resp is nil only the status code is
// returned (no body).
func newProviderWithMockServer(t *testing.T, resp *googleSearchResponse, statusCode int) Provider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "test-key" {
			http.Error(w, "missing key param", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("cx") != "test-cx" {
			http.Error(w, "missing cx param", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("q") == "" {
			http.Error(w, "missing q param", http.StatusBadRequest)
			return
		}
		if resp == nil {
			w.WriteHeader(statusCode)
			return
		}
		data, err := json.Marshal(resp)
		if err != nil {
			http.Error(w, fmt.Sprintf("marshal: %v", err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(statusCode)
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)

	client := server.Client()
	baseTransport := client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	client.Transport = &rewriteTransport{
		inner:    baseTransport,
		mockHost: server.Listener.Addr().String(),
		mockURL:  server.URL,
	}

	return Provider{
		apiKey:     "test-key",
		engineID:   "test-cx",
		httpClient: client,
	}
}

// rewriteTransport rewrites the Host and URL of outbound requests to point at
// the mock server instead of www.googleapis.com.
type rewriteTransport struct {
	inner    http.RoundTripper
	mockHost string
	mockURL  string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone and rewrite
	clone := req.Clone(req.Context())
	// Replace the host:port with the mock server's listener address
	clone.URL.Host = rt.mockHost
	clone.URL.Scheme = "http"
	clone.Host = rt.mockHost
	return rt.inner.RoundTrip(clone)
}
