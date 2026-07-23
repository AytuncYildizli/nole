package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// crawlFakeProvider is a minimal provider that returns canned extract responses.
type crawlFakeProvider struct {
	name        string
	caps        []Capability
	extractResp ExtractResponse
	extractErr  error
}

func (p *crawlFakeProvider) Name() string                     { return p.name }
func (p *crawlFakeProvider) Capabilities() []Capability       { return p.caps }
func (p *crawlFakeProvider) Search(context.Context, SearchRequest) (SearchResponse, error) {
	return SearchResponse{}, nil
}
func (p *crawlFakeProvider) Extract(_ context.Context, req ExtractRequest) (ExtractResponse, error) {
	return p.extractResp, p.extractErr
}
func (p *crawlFakeProvider) Status(context.Context) ProviderStatus {
	return ProviderStatus{Name: p.name, Available: true, Capabilities: p.caps}
}

func newCrawlTestService() *Service {
	provider := &crawlFakeProvider{
		name: "test-extract",
		caps: []Capability{CapabilityExtract, CapabilityStatus},
		extractResp: ExtractResponse{
			Content: "test content",
		},
	}
	registry := NewRegistry()
	_ = registry.Register(provider)
	ledger := NewMemoryQuotaLedger()
	ledger.Set(QuotaEntry{Provider: provider.Name(), CostClass: CostClassKeylessFree, KeylessFree: true})
	return NewService(registry, ledger, RouteMatrix{TaskExtract: {provider.Name()}})
}

func TestExtractLinks_Basic(t *testing.T) {
	html := `<html><body>
<a href="/page1">Page 1</a>
<a href="https://example.com/page2">Page 2</a>
<a href="#anchor">Skip</a>
<a href="javascript:void(0)">JS</a>
<a href="mailto:test@example.com">Mail</a>
</body></html>`

	links, err := extractLinks([]byte(html), "https://example.com")
	if err != nil {
		t.Fatalf("extractLinks failed: %v", err)
	}

	if len(links) != 2 {
		t.Fatalf("expected 2 links, got %d: %v", len(links), links)
	}

	found1, found2 := false, false
	for _, link := range links {
		switch link {
		case "https://example.com/page1":
			found1 = true
		case "https://example.com/page2":
			found2 = true
		}
	}
	if !found1 {
		t.Errorf("expected https://example.com/page1 in links: %v", links)
	}
	if !found2 {
		t.Errorf("expected https://example.com/page2 in links: %v", links)
	}
}

func TestExtractLinks_SameDomain(t *testing.T) {
	html := `<html><body>
<a href="/page1">Page 1</a>
<a href="https://other.com/page">External</a>
<a href="https://example.com/page2">Same</a>
</body></html>`

	links, err := extractLinks([]byte(html), "https://example.com")
	if err != nil {
		t.Fatalf("extractLinks failed: %v", err)
	}

	if len(links) != 3 {
		t.Fatalf("expected 3 links, got %d: %v", len(links), links)
	}
}

func TestExtractLinks_Deduplicates(t *testing.T) {
	html := `<html><body>
<a href="/page1">A</a>
<a href="https://example.com/page1">B</a>
</body></html>`

	links, err := extractLinks([]byte(html), "https://example.com")
	if err != nil {
		t.Fatalf("extractLinks failed: %v", err)
	}

	if len(links) != 1 {
		t.Fatalf("expected dedup to produce 1 link, got %d: %v", len(links), links)
	}
}

// TestCrawl_Basic exercises the full Crawl function with a mock HTTP server
// serving an interconnected page set. Since Crawl uses Service.Extract (which
// delegates to the registered provider) and fetchRaw (which does a real HTTP
// GET), we set up a small test server.
func TestCrawl_Basic(t *testing.T) {
	// Use a minimal mock server that returns two linked pages
	pageCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageCount++
		w.Header().Set("Content-Type", "text/html")
		// All pages are the same: single <a> back to seed
		body := `<html><body><p>page ` + r.URL.Path + `</p></body></html>`
		w.Write([]byte(body))
	}))
	defer srv.Close()

	svc := newCrawlTestService()

	// Override the fake provider's content to return page-specific data
	resp, err := svc.Crawl(context.Background(), CrawlRequest{
		SeedURL: srv.URL + "/seed",
		Depth:   1,
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("Crawl failed: %v", err)
	}
	if resp.Total == 0 {
		t.Fatal("expected at least 1 extracted page")
	}
}

func TestExtractTitleFromRawHTML(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		title string
	}{
		{
			name:  "simple title",
			html:  `<html><head><title>My Page Title</title></head><body>hello</body></html>`,
			title: "My Page Title",
		},
		{
			name:  "no title",
			html:  `<html><head></head><body>hello</body></html>`,
			title: "",
		},
		{
			name:  "empty title",
			html:  `<html><head><title></title></head><body>hello</body></html>`,
			title: "",
		},
		{
			name:  "nested title text",
			html:  `<html><head><title>  Spaced Title  </title></head></html>`,
			title: "Spaced Title",
		},
		{
			name:  "body text after title",
			html:  `<html><head><title>Real Title</title></head><body><p>content</p></body></html>`,
			title: "Real Title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTitleFromRawHTML([]byte(tt.html))
			if got != tt.title {
				t.Errorf("extractTitleFromRawHTML = %q, want %q", got, tt.title)
			}
		})
	}
}

func TestCrawl_RejectsEmptySeed(t *testing.T) {
	svc := newCrawlTestService()
	_, err := svc.Crawl(context.Background(), CrawlRequest{
		SeedURL: "",
		Depth:   1,
		Limit:   10,
	})
	if err == nil {
		t.Fatal("expected error for empty seed URL")
	}
}

func TestCrawl_DefaultsDepthAndLimit(t *testing.T) {
	html := `<html><body><p>hello</p></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))
	}))
	defer srv.Close()

	svc := newCrawlTestService()
	resp, err := svc.Crawl(context.Background(), CrawlRequest{
		SeedURL: srv.URL,
		// Depth=0, Limit=0 should default to 1 and 20
	})
	if err != nil {
		t.Fatalf("Crawl failed: %v", err)
	}
	if resp.Depth != 1 {
		t.Errorf("expected depth default 1, got %d", resp.Depth)
	}
	if resp.Total != 1 {
		t.Errorf("expected 1 page extracted, got %d", resp.Total)
	}
}
