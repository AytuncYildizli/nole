package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

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

			fmt.Fprintf(cmd.ErrOrStderr(), "Using proxy: %s\n", proxyURL)

			// Try Ahmia first (real .onion index)
			svc := svcFn()
			if svc != nil {
				ahmiaResp, ahmiaErr := svc.SearchWithProvider(cmd.Context(), "ahmia", core.SearchRequest{
					Query: args[0],
					Limit: limit,
				})
				if ahmiaErr == nil && len(ahmiaResp.Results) > 0 {
					ahmiaResp = applySearchInsightMode(ahmiaResp, insightMode)
					if jsonOut {
						return writeJSONTo(cmd.OutOrStdout(), ahmiaResp)
					}
					writeHumanRoutingInsight(cmd.OutOrStdout(), ahmiaResp.RoutingInsight, ahmiaResp.RouteTrace, insightMode)
					if ahmiaResp.TaskSource != "" && insightMode != core.InsightOff {
						fmt.Fprintf(cmd.OutOrStdout(), "Task: %s (%s)\n", ahmiaResp.Task, ahmiaResp.TaskSource)
					}
					for _, result := range ahmiaResp.Results {
						writeHumanContentSafety(cmd.OutOrStdout(), result.ContentSafety)
						fmt.Fprintf(cmd.OutOrStdout(), "%s\n%s\n%s\n\n", result.Title, result.URL, result.Snippet)
					}
					return nil
				}
			}

			// Fallback: try Haystack (clearnet + .onion index)
			if svc != nil {
				haystackResp, haystackErr := svc.SearchWithProvider(cmd.Context(), "haystack", core.SearchRequest{
					Query: args[0],
					Limit: limit,
				})
				if haystackErr == nil && len(haystackResp.Results) > 0 {
					haystackResp = applySearchInsightMode(haystackResp, insightMode)
					if jsonOut {
						return writeJSONTo(cmd.OutOrStdout(), haystackResp)
					}
					writeHumanRoutingInsight(cmd.OutOrStdout(), haystackResp.RoutingInsight, haystackResp.RouteTrace, insightMode)
					if haystackResp.TaskSource != "" && insightMode != core.InsightOff {
						fmt.Fprintf(cmd.OutOrStdout(), "Task: %s (%s)\n", haystackResp.Task, haystackResp.TaskSource)
					}
					for _, result := range haystackResp.Results {
						writeHumanContentSafety(cmd.OutOrStdout(), result.ContentSafety)
						fmt.Fprintf(cmd.OutOrStdout(), "%s\n%s\n%s\n\n", result.Title, result.URL, result.Snippet)
					}
					return nil
				}
			}

			// Fallback: try OnionEngine (enterprise threat intel + .onion search)
			if svc != nil {
				oeResp, oeErr := svc.SearchWithProvider(cmd.Context(), "onionengine", core.SearchRequest{
					Query: args[0],
					Limit: limit,
				})
				if oeErr == nil && len(oeResp.Results) > 0 {
					oeResp = applySearchInsightMode(oeResp, insightMode)
					if jsonOut {
						return writeJSONTo(cmd.OutOrStdout(), oeResp)
					}
					writeHumanRoutingInsight(cmd.OutOrStdout(), oeResp.RoutingInsight, oeResp.RouteTrace, insightMode)
					if oeResp.TaskSource != "" && insightMode != core.InsightOff {
						fmt.Fprintf(cmd.OutOrStdout(), "Task: %s (%s)\n", oeResp.Task, oeResp.TaskSource)
					}
					for _, result := range oeResp.Results {
						writeHumanContentSafety(cmd.OutOrStdout(), result.ContentSafety)
						fmt.Fprintf(cmd.OutOrStdout(), "%s\n%s\n%s\n\n", result.Title, result.URL, result.Snippet)
					}
					return nil
				}
			}

			// Final fallback: normal search through Tor proxy
			queryLower := strings.ToLower(args[0])
			task := core.TaskGeneral
			if strings.Contains(queryLower, ".onion") || strings.Contains(queryLower, "onion ") || strings.Contains(queryLower, "tor ") {
				task = core.TaskGeneral
			}

			resp, searchErr := search(cmd.Context(), args[0], task, limit, core.SearchOptions{})
			resp = applySearchInsightMode(resp, insightMode)
			if searchErr != nil {
				if jsonOut {
					_ = writeJSONTo(cmd.OutOrStdout(), buildCLIErrorWithInsightMode("darkweb", searchErr, resp.Route, resp.RouteTrace, insightMode))
				}
				return searchErr
			}
			if jsonOut {
				return writeJSONTo(cmd.OutOrStdout(), resp)
			}
			writeHumanRoutingInsight(cmd.OutOrStdout(), resp.RoutingInsight, resp.RouteTrace, insightMode)
			if resp.TaskSource != "" && insightMode != core.InsightOff {
				fmt.Fprintf(cmd.OutOrStdout(), "Task: %s (%s)\n", resp.Task, resp.TaskSource)
			}
			for _, result := range resp.Results {
				writeHumanContentSafety(cmd.OutOrStdout(), result.ContentSafety)
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n%s\n%s\n\n", result.Title, result.URL, result.Snippet)
			}
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
