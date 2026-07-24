package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/dorukardahan/nole/internal/core"
	"github.com/spf13/cobra"
)

func newDarkwebCommand() *cobra.Command {
	return newDarkwebCommandWithDeps(core.ResolveProxyURL, runSearch, defaultService)
}

func newDarkwebCommandWithDeps(
	resolveProxy func() string,
	search darkwebSearchFunc,
	svcFn func() *core.Service,
) *cobra.Command {
	var limit int
	var jsonOut bool
	var insightRaw string

	cmd := &cobra.Command{
		Use:   "darkweb <query>",
		Short: "Search .onion darkweb via Ahmia/Tor (fail-closed, no clearnet)",
		Long: `Search the dark web using Ahmia.fi hidden-service index through Tor.

Ahmia indexes real .onion hidden services. Uses Scrapling/Playwright for JS
rendering through Tor SOCKS5. Fail-closed: never falls back to clearnet providers.

The command automatically detects Tor by:
  1. Checking NOLE_PROXY_URL environment variable
  2. Detecting Tor on port 127.0.0.1:9050

If no Tor or Ahmia results, returns error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check proxy availability
			proxyURL := resolveProxy()
			if proxyURL == "" {
				return fmt.Errorf("Tor not running: connect and retry; export NOLE_PROXY_URL=socks5://127.0.0.1:9050")
			}

			// Set proxy env for Ahmia/Scrapling helper
			if err := os.Setenv("NOLE_PROXY_URL", proxyURL); err != nil {
				return fmt.Errorf("set NOLE_PROXY_URL: %w", err)
			}

			// Try Ahmia first (real .onion index via Scrapling)
			svc := svcFn()
			if svc == nil {
				return fmt.Errorf("darkweb: service not available")
			}

			ahmiaResp, ahmiaErr := svc.SearchWithProvider(cmd.Context(), "ahmia", core.SearchRequest{
				Query: args[0],
				Limit: limit,
			})
			if ahmiaErr == nil && len(ahmiaResp.Results) > 0 {
				if jsonOut {
					return writeJSONTo(cmd.OutOrStdout(), ahmiaResp)
				}
				writeWhatsAppResults(cmd.OutOrStdout(), ahmiaResp.Results)
				return nil
			}

			// Try Haystack (clearnet + dark web)
			haystackResp, haystackErr := svc.SearchWithProvider(cmd.Context(), "haystack", core.SearchRequest{
				Query: args[0],
				Limit: limit,
			})
			if haystackErr == nil && len(haystackResp.Results) > 0 {
				if jsonOut {
					return writeJSONTo(cmd.OutOrStdout(), haystackResp)
				}
				writeWhatsAppResults(cmd.OutOrStdout(), haystackResp.Results)
				return nil
			}

			// Try OnionEngine (enterprise threat intel)
			oeResp, oeErr := svc.SearchWithProvider(cmd.Context(), "onionengine", core.SearchRequest{
				Query: args[0],
				Limit: limit,
			})
			if oeErr == nil && len(oeResp.Results) > 0 {
				if jsonOut {
					return writeJSONTo(cmd.OutOrStdout(), oeResp)
				}
				writeWhatsAppResults(cmd.OutOrStdout(), oeResp.Results)
				return nil
			}

			// Try DDGS via Tor proxy (onion endpoint)
			resp, searchErr := search(cmd.Context(), args[0], core.TaskGeneral, limit, core.SearchOptions{})
			if searchErr == nil && len(resp.Results) > 0 {
				if jsonOut {
					return writeJSONTo(cmd.OutOrStdout(), resp)
				}
				writeWhatsAppResults(cmd.OutOrStdout(), resp.Results)
				return nil
			}

			// ALL darkweb providers failed — return error
			if ahmiaErr != nil {
				return fmt.Errorf("darkweb: no results from any provider (Ahmia: %v)", ahmiaErr)
			}
			if searchErr != nil {
				return fmt.Errorf("darkweb: no results from any provider (DDGS: %v)", searchErr)
			}
			return fmt.Errorf("darkweb: no results from Ahmia, Haystack, OnionEngine, or DDGS onion")
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 5, "maximum results")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output JSON")
	cmd.Flags().StringVar(&insightRaw, "insight", string(core.InsightCompact), "routing insight output (hidden in WhatsApp mode)")
	return cmd
}

type darkwebSearchFunc func(
	context.Context,
	string,
	core.TaskType,
	int,
	core.SearchOptions,
) (core.SearchResponse, error)

type svcFunc func() *core.Service

func writeWhatsAppResults(w io.Writer, results []core.SearchResult) {
	for _, r := range results {
		fmt.Fprintf(w, "%s\n%s\n%s\n\n", r.Title, r.URL, r.Snippet)
	}
}
