// Package core provides the Nólë service, registry, and routing primitives.
//
// Tor detection helpers: DetectTorProxy checks whether a Tor SOCKS5 proxy is
// running on the default 127.0.0.1:9050. When NOLE_PROXY_URL is not set but
// Tor is detected, this allows automatic proxy configuration for darkweb
// subcommands and .onion-aware providers.
package core

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultTorPort is the default Tor SOCKS5 proxy port.
const DefaultTorPort = 9050

// DefaultTorHost is the default Tor SOCKS5 proxy host.
const DefaultTorHost = "127.0.0.1"

// DetectTor checks whether a Tor SOCKS5 proxy is listening on
// DefaultTorHost:DefaultTorPort (127.0.0.1:9050). It dials the TCP port
// with a short timeout and returns true if the connection succeeds.
func DetectTor() bool {
	return DetectTorOn(DefaultTorHost, DefaultTorPort)
}

// DetectTorOn checks whether a Tor SOCKS5 proxy is listening on the given
// host and port by dialing the TCP port with a short timeout.
func DetectTorOn(host string, port int) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// TorProxyURL returns the socks5:// URL for the default Tor SOCKS5 proxy.
func TorProxyURL() string {
	return "socks5://" + net.JoinHostPort(DefaultTorHost, strconv.Itoa(DefaultTorPort))
}

// ResolveProxyURL resolves the effective proxy URL to use for Tor/.onion
// requests. If NOLE_PROXY_URL is set, it is returned as-is. If Tor is
// detected on 127.0.0.1:9050, the default Tor SOCKS5 URL is returned.
// Otherwise, an empty string is returned.
func ResolveProxyURL() string {
	if env := os.Getenv("NOLE_PROXY_URL"); env != "" {
		return strings.TrimSpace(env)
	}
	if DetectTor() {
		return TorProxyURL()
	}
	return ""
}
