package core

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// SmartRouter wraps the base Router and adds dynamic query-aware provider
// selection. It reads the query context and connection environment to choose
// the best provider automatically.
type SmartRouter struct {
	base     *Router
	registry *Registry
}

// NewSmartRouter creates a SmartRouter wrapping an existing Router.
func NewSmartRouter(base *Router, registry *Registry) *SmartRouter {
	return &SmartRouter{base: base, registry: registry}
}

// torDetect checks if Tor SOCKS5 is available on port 9050.
func torDetect() bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(9050)), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// proxyFromEnv returns the current NOLE_PROXY_URL value.
func proxyFromEnv() string {
	pu := os.Getenv("NOLE_PROXY_URL")
	if pu != "" {
		return pu
	}
	// Auto-detect Tor
	if torDetect() {
		return "socks5://127.0.0.1:9050"
	}
	return ""
}

// hasOnionQuery checks if the query targets .onion content.
func hasOnionQuery(query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(q, ".onion") ||
		strings.Contains(q, "onion ") ||
		strings.Contains(q, "darkweb") ||
		strings.Contains(q, "dark web") ||
		strings.Contains(q, "tor ")
}

// SelectProvider returns the optimal provider name and proxy URL for a query.
func (sr *SmartRouter) SelectProvider(query string, task TaskType) (string, string, error) {
	proxyURL := proxyFromEnv()

	// Query contains .onion or darkweb references — must route through Tor/ddgs
	if hasOnionQuery(query) {
		if proxyURL == "" {
			return "", "", fmt.Errorf("Tor not running: start Tor or set NOLE_PROXY_URL")
		}
		// Check ddgs is registered
		if _, ok := sr.registry.Get("ddgs"); ok {
			return "ddgs", proxyURL, nil
		}
		// Fallback: try Ahmia (real .onion index)
		if _, ok := sr.registry.Get("ahmia"); ok {
			return "ahmia", proxyURL, nil
		}
		return "", proxyURL, fmt.Errorf("ddgs provider not registered for onion search")
	}

	// Proxy is set but query is normal — use ddgs through Tor for privacy
	if proxyURL != "" {
		if _, ok := sr.registry.Get("ddgs"); ok {
			return "ddgs", proxyURL, nil
		}
	}

	// Default task routing: let the base Router decide
	return "", proxyURL, nil
}

// Search performs a smart-routed search. If the base Router's default
// candidates fail, it tries fallback providers in order.
func (sr *SmartRouter) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	// Save original query for context
	query := req.Query

	// Phase 1: Smart provider selection based on query
	smartProvider, smartProxy, err := sr.SelectProvider(query, req.Task)
	if err != nil {
		// No smart route possible, fall through to default
		smartProvider = ""
	}

	if smartProvider != "" {
		// Set proxy env for the provider
		if smartProxy != "" {
			os.Setenv("NOLE_PROXY_URL", smartProxy)
		}

		// Try the smart-chosen provider
		if provider, ok := sr.registry.Get(smartProvider); ok {
			// Set task to match route if ddgs
			req.Task = TaskGeneral
			resp, perr := provider.Search(ctx, req)
			if perr == nil && len(resp.Results) > 0 {
				return resp, nil
			}

			// If smart provider fails (rate limit, 202), try Jina fallback
			if perr != nil {
				if jina, jok := sr.registry.Get("jinareader"); jok {
					req.Query = query
					jresp, jerr := jina.Search(ctx, req)
					if jerr == nil && len(jresp.Results) > 0 {
						return jresp, nil
					}
				}
			}
		}
	}

	// Phase 2: Try Jina Reader as general fallback if direct search fails
	if jina, ok := sr.registry.Get("jinareader"); ok {
		req.Query = query
		resp, err := jina.Search(ctx, req)
		if err == nil && len(resp.Results) > 0 {
			return resp, nil
		}
	}

	// Phase 3: Return error if nothing worked
	return SearchResponse{}, fmt.Errorf("smartrouter: all providers failed for query: %s", query)
}
