package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MultiSearchResult holds a single provider's search result.
type MultiSearchResult struct {
	Provider string
	Response SearchResponse
	Error    error
}

// MultiSearch sends the same query to multiple providers in parallel
// and returns the best result: the one with the most results (up to limit)
// that arrives first. If all fail, returns the last error.
func MultiSearch(ctx context.Context, registry *Registry, req SearchRequest) (SearchResponse, error) {
	// Candidate providers: try ddgs, brave, google, jina in priority order
	candidates := []string{"ddgs", "brave", "google", "jinareader", "tavily"}
	active := make([]string, 0, len(candidates))

	for _, name := range candidates {
		if _, ok := registry.Get(name); ok {
			active = append(active, name)
		}
	}

	if len(active) == 0 {
		return SearchResponse{}, fmt.Errorf("multisearch: no search providers registered")
	}

	// At most 3 parallel searches
	if len(active) > 3 {
		active = active[:3]
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	results := make(chan MultiSearchResult, len(active))
	var wg sync.WaitGroup

	for _, name := range active {
		wg.Add(1)
		name := name
		go func() {
			defer wg.Done()
			provider, ok := registry.Get(name)
			if !ok {
				results <- MultiSearchResult{Provider: name, Error: fmt.Errorf("provider %s not found", name)}
				return
			}
			resp, err := provider.Search(ctx, SearchRequest{
				Query:   req.Query,
				Task:    req.Task,
				Limit:   req.Limit,
				Options: req.Options,
			})
			results <- MultiSearchResult{Provider: name, Response: resp, Error: err}
		}()
	}

	// Wait and collect
	go func() {
		wg.Wait()
		close(results)
	}()

	var best MultiSearchResult
	bestCount := 0

	for res := range results {
		if res.Error != nil {
			continue
		}
		count := len(res.Response.Results)
		if count > bestCount {
			best = res
			bestCount = count
		}
	}

	if bestCount > 0 {
		best.Response.Provider = fmt.Sprintf("multisearch:%s", best.Provider)
		return best.Response, nil
	}

	return SearchResponse{}, fmt.Errorf("multisearch: all %d providers failed for query: %s", len(active), truncateQuery(req.Query, 50))
}

func truncateQuery(q string, max int) string {
	if len(q) <= max {
		return q
	}
	return strings.TrimSpace(q[:max]) + "..."
}
