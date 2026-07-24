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
		Short: "Search the dark web (.onion) via Tor proxy + Ahmia/Haystack/OnionEngine",
		Long: `Search the dark web using Ahmia.fi hidden-service index, Haystak, or OnionEngine
through a Tor proxy.

Ahmia indexes real .onion hidden services. Haystak and OnionEngine provide
additional clearnet + .onion coverage as fallback providers.

The command automatically detects Tor by:
  1. Checking NOLE_PROXY_URL environment variable
  2. Detecting Tor on port 127.0.0.1:9050

If neither is available, it prints an error suggesting to start Tor first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			insightMode, err := parseInsightModeFlag(insightRaw)
			if err != nil {
				return err
			}

			// Check proxy availability
			proxyURL := resolveProxy()
			if proxyURL == "" {
				return fmt.Errorf("Tor not running: connect and retry; export NOLE_PROXY_URL=socks5://127.0.0.1:9050")
			}

			// Set the proxy env for the search service to pick up
			if err := os.Setenv("NOLE_PROXY_URL", proxyURL); err != nil {
				return fmt.Errorf("set NOLE_PROXY_URL: %w", err)
			}

			// Try Ahmia first (real .onion index)
			svc := svcFn()
			if svc != nil {
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

				// Fallback: try Haystack
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

				// Fallback: try OnionEngine
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
			}

			// Final fallback: standard search through Tor proxy
			resp, searchErr := search(cmd.Context(), args[0], core.TaskGeneral, limit, core.SearchOptions{})
			if searchErr != nil {
				if jsonOut {
					_ = writeJSONTo(cmd.OutOrStdout(), buildCLIErrorWithInsightMode("darkweb", searchErr, resp.Route, resp.RouteTrace, insightMode))
				}
				return searchErr
			}
			if jsonOut {
				return writeJSONTo(cmd.OutOrStdout(), resp)
			}
			writeWhatsAppResults(cmd.OutOrStdout(), resp.Results)
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 5, "maximum results")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output JSON")
	cmd.Flags().StringVar(&insightRaw, "insight", string(core.InsightCompact), "routing insight output: compact, off, or verbose")
	return cmd
}

// darkwebSearchFunc is the standard search function signature.
type darkwebSearchFunc func(
	context.Context,
	string,
	core.TaskType,
	int,
	core.SearchOptions,
) (core.SearchResponse, error)

// svcFunc returns a Service instance.
type svcFunc func() *core.Service

// writeWhatsAppResults prints search results in WhatsApp-friendly format.
// Simple: each result as [bold title] + URL + snippet line.
func writeWhatsAppResults(w io.Writer, results []core.SearchResult) {
	for _, r := range results {
		fmt.Fprintf(w, "%s\n%s\n%s\n\n", r.Title, r.URL, r.Snippet)
	}
}
