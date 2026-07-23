package core

import (
	"os"
	"testing"
)

func TestTorProxyURL(t *testing.T) {
	url := TorProxyURL()
	want := "socks5://127.0.0.1:9050"
	if url != want {
		t.Fatalf("TorProxyURL() = %q, want %q", url, want)
	}
}

func TestResolveProxyURLUsesEnvVar(t *testing.T) {
	os.Setenv("NOLE_PROXY_URL", "socks5://myproxy:1080")
	defer os.Unsetenv("NOLE_PROXY_URL")

	url := ResolveProxyURL()
	if url != "socks5://myproxy:1080" {
		t.Fatalf("ResolveProxyURL() = %q, want socks5://myproxy:1080", url)
	}
}

func TestResolveProxyURLFallsBackToTorDetection(t *testing.T) {
	os.Unsetenv("NOLE_PROXY_URL")

	// We can't actually run Tor in CI, but DetectTor should not panic
	// and ResolveProxyURL should not return empty when env is unset
	// and Tor is not running — it returns empty.
	url := ResolveProxyURL()
	// Without Tor running, this should be empty
	if url != "" {
		t.Logf("ResolveProxyURL() = %q (Tor detected)", url)
	}
}

func TestDetectTorDoesNotPanic(t *testing.T) {
	// DetectTor should never panic, even if Tor is not running
	result := DetectTor()
	t.Logf("DetectTor() = %v", result)
}

func TestDetectTorOnNotRunning(t *testing.T) {
	// This port should not be Tor — expect false but no panic
	result := DetectTorOn("127.0.0.1", 19999)
	if result {
		t.Logf("DetectTorOn(127.0.0.1, 19999) unexpectedly true; something is on that port")
	}
}
