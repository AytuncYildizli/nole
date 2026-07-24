// Package ahmia provides the Ahmia.fi hidden-service search provider for Nólë.
// Ahmia indexes .onion hidden services on the Tor network — unlike DDGS onion
// endpoint (which is just DDG's clearnet index proxied through Tor), Ahmia
// actually crawls Tor hidden services and indexes .onion content directly.
//
// Ahmia's .onion search page requires JavaScript rendering. This provider
// uses a Scrapling Python helper script for JS render + DOM extraction,
// communicating via a JSON stdin/stdout contract.
//
// Proxy: requires NOLE_PROXY_URL (SOCKS5 through Tor).
// Clearnet fallback: disabled by design (Tor required).
package ahmia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dorukardahan/nole/internal/core"
)

const DefaultTorHost = "127.0.0.1"
const DefaultTorPort = 9050

const nonJSBanner = "non-JavaScript"
const helperTimeout = 45 * time.Second

// Provider uses Scrapling Python helper for Ahmia search.
type Provider struct {
	helperPath string
}

// helperRequest is sent via stdin to the Python helper.
type helperRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
	Proxy string `json:"proxy"`
}

// helperResponse comes via stdout from the Python helper.
type helperResponse struct {
	Results []helperResult `json:"results"`
	Error   string         `json:"error,omitempty"`
	Detail  string         `json:"detail,omitempty"`
}

type helperResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// New creates an Ahmia provider.
func New() Provider {
	hp := findHelper()
	return Provider{helperPath: hp}
}

func findHelper() string {
	// Relative to the provider dir (development)
	candidates := []string{
		"/tmp/nole-doruk/internal/providers/ahmia/ahmia_search.py",
		"internal/providers/ahmia/ahmia_search.py",
		"ahmia_search.py",
	}
	// Try NOLE_AHMIA_HELPER env var first
	if env := os.Getenv("NOLE_AHMIA_HELPER"); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	// Search from CWD
	cwd, _ := os.Getwd()
	for _, c := range candidates {
		full := filepath.Join(cwd, c)
		if _, err := os.Stat(full); err == nil {
			return full
		}
	}
	return candidates[0]
}

func (p Provider) Name() string { return "ahmia" }

func (p Provider) Capabilities() []core.Capability {
	return []core.Capability{core.CapabilitySearch, core.CapabilityStatus}
}

func (p Provider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	// Check Tor
	proxyURL := resolveProxy()
	if proxyURL == "" {
		return core.SearchResponse{}, fmt.Errorf("ahmia: Tor not available — set NOLE_PROXY_URL or start Tor on port 9050")
	}

	// Check helper exists
	if _, err := os.Stat(p.helperPath); os.IsNotExist(err) {
		return core.SearchResponse{}, fmt.Errorf("ahmia: helper not found at %s", p.helperPath)
	}

	limit := req.Limit
	if limit <= 0 || limit > 20 {
		limit = 10
	}

	// Prepare JSON request
	hreq := helperRequest{
		Query: req.Query,
		Limit: limit,
		Proxy: proxyURL,
	}
	hreqBody, err := json.Marshal(hreq)
	if err != nil {
		return core.SearchResponse{}, fmt.Errorf("ahmia: marshal request: %w", err)
	}

	// Execute helper
	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", p.helperPath)
	cmd.Stdin = bytes.NewReader(hreqBody)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Check if context deadline caused the exit
		if ctx.Err() != nil {
			return core.SearchResponse{}, fmt.Errorf("ahmia: helper timeout (%s)", helperTimeout)
		}
		stderrStr := strings.TrimSpace(stderr.String())
		return core.SearchResponse{}, fmt.Errorf("ahmia: helper error: %v (stderr: %s)", err, stderrStr)
	}

	// Parse JSON response
	var hresp helperResponse
	if err := json.Unmarshal(stdout.Bytes(), &hresp); err != nil {
		return core.SearchResponse{}, fmt.Errorf("ahmia: helper JSON parse: %w (stdout: %s)", err, stdout.String()[:min(500, stdout.Len())])
	}

	// Extract
	results := make([]core.SearchResult, 0, len(hresp.Results))
	for _, hr := range hresp.Results {
		title := strings.TrimSpace(hr.Title)
		url := strings.TrimSpace(hr.URL)
		if title == "" && url == "" {
			continue
		}
		results = append(results, core.SearchResult{
			Title:    title,
			URL:      url,
			Snippet:  strings.TrimSpace(hr.Snippet),
			Provider: "ahmia",
		})
	}

	// Check for render failure
	if hresp.Error != "" {
		if len(results) == 0 {
			if strings.Contains(hresp.Error, "render_failed") {
				return core.SearchResponse{}, fmt.Errorf("ahmia: render failed: %s", hresp.Detail)
			}
			return core.SearchResponse{}, fmt.Errorf("ahmia: %s: %s", hresp.Error, hresp.Detail)
		}
		// Partial results with error warning
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
	available := true
	var issues []string

	if resolveProxy() == "" {
		available = false
		issues = append(issues, "Tor not available")
	}
	if _, err := os.Stat(p.helperPath); os.IsNotExist(err) {
		available = false
		issues = append(issues, fmt.Sprintf("helper not found at %s", p.helperPath))
	}

	return core.ProviderStatus{
		Name:         p.Name(),
		Available:    available,
		Capabilities: p.Capabilities(),
	}
}

// resolveProxy returns the SOCKS5 proxy URL to use.
func resolveProxy() string {
	if pu := os.Getenv("NOLE_PROXY_URL"); pu != "" {
		return pu
	}
	// Auto-detect Tor
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(DefaultTorHost, strconv.Itoa(DefaultTorPort)), 2*time.Second)
	if err == nil {
		conn.Close()
		return fmt.Sprintf("socks5://%s:%d", DefaultTorHost, DefaultTorPort)
	}
	return ""
}

// searchEndpoint returns the Ahmia onion endpoint with proxy.
func (p Provider) searchEndpoint() string {
	if resolveProxy() != "" {
		return "http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion/search/"
	}
	return "https://ahmia.fi/search/"
}
