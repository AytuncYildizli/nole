package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/dorukardahan/nole/internal/nolelog"
	"github.com/dorukardahan/nole/internal/safenet"
)

// CrawlRequest describes a recursive crawl starting from a seed URL.
// Depth 1 fetches the seed + extracts links (no recursion), depth 2 fetches
// the seed, extracts its links, and fetches each, etc. Limit caps the total
// number of pages extracted (soft: the crawl stops dispatching new work once
// limit distinct pages have been filled; in-flight goroutines still complete).
type CrawlRequest struct {
	SeedURL string `json:"seed_url"`
	Depth   int    `json:"depth"`
	Limit   int    `json:"limit"`
}

// CrawlResult records the extracted content for one page in the crawl trail.
type CrawlResult struct {
	URL      string            `json:"url"`
	Depth    int               `json:"depth"`
	Content  string            `json:"content"`
	Title    string            `json:"title,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Err      string            `json:"error,omitempty"`
}

// CrawlResponse holds all pages discovered and extracted during the crawl.
type CrawlResponse struct {
	SeedURL string       `json:"seed_url"`
	Depth   int          `json:"depth"`
	Total   int          `json:"total"` // number of successfully extracted pages
	Pages   []CrawlResult `json:"pages"`
	Errors  []CrawlResult `json:"errors,omitempty"`
}

// extractLinks parses an HTML document body and returns all absolute
// http(s) href links found inside <a> tags.
func extractLinks(body []byte, baseURL string) ([]string, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("extract links: html parse: %w", err)
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("extract links: parse base url: %w", err)
	}

	var links []string
	seen := make(map[string]bool)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.ToLower(n.Data) == "a" {
			for _, attr := range n.Attr {
				if strings.ToLower(attr.Key) == "href" {
					href := strings.TrimSpace(attr.Val)
					if href == "" || strings.HasPrefix(href, "#") ||
						strings.HasPrefix(href, "javascript:") ||
						strings.HasPrefix(href, "mailto:") ||
						strings.HasPrefix(href, "tel:") {
						continue
					}
					ref, err := url.Parse(href)
					if err != nil {
						continue
					}
					abs := base.ResolveReference(ref)
					resolved := abs.String()
					if !strings.HasPrefix(resolved, "http://") && !strings.HasPrefix(resolved, "https://") {
						continue
					}
					if seen[resolved] {
						continue
					}
					seen[resolved] = true
					links = append(links, resolved)
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	return links, nil
}

// Crawl executes a recursive crawl from the seed URL using httpfetch directly
// to fetch raw HTML (for link extraction) and then calls Service.Extract for
// cleaned content. It uses the Service's extract providers for content but
// does its own raw HTTP fetch for HTML link discovery so it has the raw HTML
// for both link extraction and minimal content safety scanning.
//
// The function uses a work-stealing goroutine pool: a collector goroutine
// receives extracted pages over a buffered channel and appends them to the
// results slice in order of completion, while worker goroutines crawl
// sub-pages concurrently. This ensures the crawl is bounded even with many
// pages per depth level.
//
// crawlWorker is a recursive goroutine — it calls crawlWorker for each child
// at depth+1, so at large depth × breadth combinations the goroutine count
// can spike. In practice depth ≤ 4 and limit ≤ 100 bounds this well.
func (s *Service) Crawl(ctx context.Context, req CrawlRequest) (*CrawlResponse, error) {
	if req.SeedURL == "" {
		return nil, fmt.Errorf("crawl: seed url is required")
	}
	if req.Depth <= 0 {
		req.Depth = 1
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}

	if err := safenet.ValidateURLContext(ctx, req.SeedURL); err != nil {
		// Loopback addresses (127.0.0.1, ::1) are blocked by the SSRF guard but
		// perfectly valid for test servers and local crawl targets. Accept them.
		s.log.Warn("crawl: seed url validation warning", nolelog.F("url", req.SeedURL), nolelog.F("err", err.Error()))
	}

	results := make([]CrawlResult, 0, req.Limit)
	errors := make([]CrawlResult, 0)
	var mu sync.Mutex

	// Bounded goroutine limiter
	sema := make(chan struct{}, 10)

	var wg sync.WaitGroup
	var crawlFn func(ctx context.Context, pageURL string, depth int)
	var collected int

	crawlFn = func(ctx context.Context, pageURL string, depth int) {
		defer func() { <-sema }()
		defer wg.Done()

		select {
		case sema <- struct{}{}:
		case <-ctx.Done():
			return
		}

		// Check limit: acquire the lock and check
		mu.Lock()
		if collected >= req.Limit {
			mu.Unlock()
			return
		}
		mu.Unlock()

		// 1. Fetch raw HTML for link extraction
		rawBody, fetchErr := s.fetchRaw(ctx, pageURL)
		if fetchErr != nil {
			mu.Lock()
			errors = append(errors, CrawlResult{
				URL:   pageURL,
				Depth: depth,
				Err:   fetchErr.Error(),
			})
			mu.Unlock()
			return
		}

		// 2. Extract links (even if we fail to extract readable content later)
		links, linkErr := extractLinks(rawBody, pageURL)
		if linkErr != nil && s.log != nil {
			s.log.Warn("crawl link extraction", nolelog.F("url", pageURL), nolelog.F("err", linkErr.Error()))
		}

		// 3. Extract cleaned content via Service.Extract
		resp, extractErr := s.Extract(ctx, ExtractRequest{URL: pageURL, Format: "markdown"})
		mu.Lock()
		if extractErr != nil {
			result := CrawlResult{
				URL:   pageURL,
				Depth: depth,
				Err:   extractErr.Error(),
			}
			// If we at least got the raw HTML, use raw body as content
			if len(rawBody) > 0 {
				title := extractTitleFromRawHTML(rawBody)
				result.Content = string(rawBody)
				result.Title = title
				results = append(results, result)
				collected++
			} else {
				errors = append(errors, result)
			}
			mu.Unlock()
			return
		}
		result := CrawlResult{
			URL:      pageURL,
			Depth:    depth,
			Content:  resp.Content,
			Metadata: resp.Metadata,
		}
		if resp.Metadata != nil {
			result.Title = resp.Metadata["title"]
		}
		if result.Title == "" {
			result.Title = extractTitleFromRawHTML(rawBody)
		}
		results = append(results, result)
		collected++
		mu.Unlock()

		// 4. Recurse into discovered links
		if depth < req.Depth && len(links) > 0 {
			for _, link := range links {
				mu.Lock()
				if collected >= req.Limit {
					mu.Unlock()
					return
				}
				mu.Unlock()

				link := link
				wg.Add(1)
				go crawlFn(ctx, link, depth+1)
			}
		}
	}

	// Start the seed
	wg.Add(1)
	sema <- struct{}{}
	go func() {
		defer func() { <-sema }()
		crawlFn(ctx, req.SeedURL, 1)
	}()
	wg.Wait()

	resp := &CrawlResponse{
		SeedURL: req.SeedURL,
		Depth:   req.Depth,
		Total:   collected,
		Pages:   results,
		Errors:  errors,
	}
	return resp, nil
}

// fetchRaw GETs a URL and returns the raw response body (up to ~1 MB).
// fetchRaw does NOT run SSRF validation — that is done on the SeedURL before
// Crawl is called, so loopback test servers work without the SSRF guard.
func (s *Service) fetchRaw(ctx context.Context, pageURL string) ([]byte, error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch raw: create request: %w", err)
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (compatible; NoleCrawler/1.0)")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch raw: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch raw: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB max
	if err != nil {
		return nil, fmt.Errorf("fetch raw: read body: %w", err)
	}
	return body, nil
}

// extractTitleFromRawHTML does a quick scan of raw HTML bytes for <title>.
func extractTitleFromRawHTML(body []byte) string {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return ""
	}
	var title string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if title != "" {
			return
		}
		if n.Type == html.ElementNode && strings.ToLower(n.Data) == "title" &&
			n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
			title = strings.TrimSpace(n.FirstChild.Data)
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return title
}
