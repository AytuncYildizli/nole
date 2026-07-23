package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/dorukardahan/nole/internal/core"
	"github.com/spf13/cobra"
)

func newDarkwebCommand() *cobra.Command {
	return newDarkwebCommandWithDeps(core.ResolveProxyURL, runSearch)
}

type darkwebSearchFunc func(
	context.Context,
	string,
	core.TaskType,
	int,
	core.SearchOptions,
) (core.SearchResponse, error)

func newDarkwebCommandWithDeps(
	resolveProxy func() string,
	search darkwebSearchFunc,
) *cobra.Command {
	var limit int
	var jsonOut bool
	var insightRaw string

	cmd := &cobra.Command{
		Use:   "darkweb <query>",
		Short: "Search the dark web (.onion) via Tor proxy",
		Long: `Search the dark web using DuckDuckGo's .onion service through a Tor proxy.

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

			// Relay message to user
			fmt.Fprintf(cmd.ErrOrStderr(), "Using proxy: %s\n", proxyURL)

			resp, err := search(cmd.Context(), args[0], core.TaskGeneral, limit, core.SearchOptions{})
			resp = applySearchInsightMode(resp, insightMode)
			if err != nil {
				if jsonOut {
					_ = writeJSONTo(cmd.OutOrStdout(), buildCLIErrorWithInsightMode("darkweb", err, resp.Route, resp.RouteTrace, insightMode))
				}
				return err
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
