package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dorukardahan/nole/internal/core"
)

func newMockService() *core.Service {
	registry := core.NewRegistry()
	registry.Register(newMockProvider("ahmia"))
	registry.Register(newMockProvider("haystack"))
	registry.Register(newMockProvider("onionengine"))
	// Use core.NewService with nil ledger and default route matrix
	svc := core.NewService(registry, core.NewMemoryQuotaLedger(), core.DefaultRouteMatrix())
	return svc
}

type mockProvider struct {
	name string
}

func newMockProvider(name string) mockProvider {
	return mockProvider{name: name}
}

func (m mockProvider) Name() string { return m.name }
func (m mockProvider) Capabilities() []core.Capability { return []core.Capability{core.CapabilitySearch, core.CapabilityStatus} }
func (m mockProvider) Search(ctx context.Context, req core.SearchRequest) (core.SearchResponse, error) {
	return core.SearchResponse{}, nil
}
func (m mockProvider) Extract(ctx context.Context, req core.ExtractRequest) (core.ExtractResponse, error) {
	return core.ExtractResponse{}, nil
}
func (m mockProvider) Status(ctx context.Context) core.ProviderStatus {
	return core.ProviderStatus{Name: m.name, Available: true}
}

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
	cmd := newDarkwebCommandWithDeps(
		func() string { return "" },
		func(context.Context, string, core.TaskType, int, core.SearchOptions) (core.SearchResponse, error) {
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
}

func TestDarkwebCommandRunsWithProxyEnv(t *testing.T) {
	t.Setenv("NOLE_PROXY_URL", "")
	const proxyURL = "socks5://127.0.0.1:9050"
	searchCalled := false
	svc := newMockService()
	cmd := newDarkwebCommandWithDeps(
		func() string { return proxyURL },
		func(ctx context.Context, query string, task core.TaskType, limit int, _ core.SearchOptions) (core.SearchResponse, error) {
			searchCalled = true
			return core.SearchResponse{
				Results: []core.SearchResult{{Title: "ddgs result", URL: "http://ddgs", Snippet: "ddgs snippet"}},
			}, nil
		},
		func() *core.Service { return svc },
	)
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"test query"})
	if err != nil {
		t.Fatalf("darkweb command: %v", err)
	}
	if !searchCalled {
		t.Fatal("search was not called")
	}
}

func TestDarkwebShortDescription(t *testing.T) {
	cmd := newDarkwebCommand()
	if !strings.Contains(cmd.Short, "fail-closed") {
		t.Fatalf("short description should mention fail-closed: %q", cmd.Short)
	}
}
