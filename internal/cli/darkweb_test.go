package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dorukardahan/nole/internal/core"
)

func TestDarkwebCommandRegistered(t *testing.T) {
	cmd := NewRootCommand()
	var found bool
	for _, sub := range cmd.Commands() {
		if sub.Use == "darkweb <query>" || sub.Name() == "darkweb" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("darkweb command not registered in root")
	}
}

func TestDarkwebCommandHasLimitFlag(t *testing.T) {
	cmd := newDarkwebCommand()
	flag := cmd.Flags().Lookup("limit")
	if flag == nil {
		t.Fatal("darkweb command missing --limit flag")
	}
}

func TestDarkwebCommandHasJSONFlag(t *testing.T) {
	cmd := newDarkwebCommand()
	flag := cmd.Flags().Lookup("json")
	if flag == nil {
		t.Fatal("darkweb command missing --json flag")
	}
}

func TestDarkwebCommandHasInsightFlag(t *testing.T) {
	cmd := newDarkwebCommand()
	flag := cmd.Flags().Lookup("insight")
	if flag == nil {
		t.Fatal("darkweb command missing --insight flag")
	}
	if flag.DefValue != "compact" {
		t.Fatalf("default insight mode = %q, want compact", flag.DefValue)
	}
}

func TestDarkwebCommandArgs(t *testing.T) {
	cmd := newDarkwebCommand()
	err := cmd.Args(cmd, []string{"query here"})
	if err != nil {
		t.Fatalf("expected 1 arg to be valid, got: %v", err)
	}
}

func TestDarkwebCommandRejectsNoArgs(t *testing.T) {
	cmd := newDarkwebCommand()
	err := cmd.Args(cmd, []string{})
	if err == nil {
		t.Fatal("expected error for 0 args, got nil")
	}
}

func TestDarkwebCommandRejectsTooManyArgs(t *testing.T) {
	cmd := newDarkwebCommand()
	err := cmd.Args(cmd, []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error for 2 args, got nil")
	}
}
func TestDarkwebCommandErrorsWithoutTor(t *testing.T) {
	searchCalled := false
	cmd := newDarkwebCommandWithDeps(
		func() string { return "" },
		func(context.Context, string, core.TaskType, int, core.SearchOptions) (core.SearchResponse, error) {
			searchCalled = true
			return core.SearchResponse{}, errors.New("search must not run")
		},
		func() *core.Service { return nil },
	)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"darkweb test"})
	if err == nil {
		t.Fatal("expected a deterministic Tor-unavailable error")
	}
	if !strings.Contains(err.Error(), "Tor not running") {
		t.Fatalf("error = %q, want Tor not running", err)
	}
	if searchCalled {
		t.Fatal("search ran without a proxy")
	}
}

func TestDarkwebCommandRunsWithProxyEnv(t *testing.T) {
	t.Setenv("NOLE_PROXY_URL", "")
	const proxyURL = "socks5://127.0.0.1:9050"
	searchCalled := false
	cmd := newDarkwebCommandWithDeps(
		func() string { return proxyURL },
		func(ctx context.Context, query string, task core.TaskType, limit int, _ core.SearchOptions) (core.SearchResponse, error) {
			searchCalled = true
			if ctx == nil {
				t.Fatal("search context is nil")
			}
			if query != "test query" || task != core.TaskGeneral || limit != 5 {
				t.Fatalf("unexpected search request: query=%q task=%q limit=%d", query, task, limit)
			}
			return core.SearchResponse{}, nil
		},
		func() *core.Service { return nil },
	)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"test query"})
	if err != nil {
		t.Fatalf("darkweb command: %v", err)
	}
	if !searchCalled {
		t.Fatal("search was not called")
	}
	if got := os.Getenv("NOLE_PROXY_URL"); got != proxyURL {
		t.Fatalf("NOLE_PROXY_URL = %q, want %q", got, proxyURL)
	}
}

func TestDarkwebShortDescription(t *testing.T) {
	cmd := newDarkwebCommand()
	if !strings.Contains(cmd.Short, "dark web") {
		t.Fatalf("short description should mention dark web: %q", cmd.Short)
	}
}
