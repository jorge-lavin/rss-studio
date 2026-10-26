package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerUsesResourceLimits(t *testing.T) {
	server := newHTTPServer(":8080", http.NewServeMux())
	if server.ReadTimeout != 30*time.Second || server.WriteTimeout != 30*time.Second {
		t.Fatalf("server read/write timeouts = %s/%s; want 30s/30s", server.ReadTimeout, server.WriteTimeout)
	}
	if server.ReadHeaderTimeout != 5*time.Second || server.IdleTimeout != 60*time.Second {
		t.Fatalf("server header/idle timeouts = %s/%s; want 5s/60s", server.ReadHeaderTimeout, server.IdleTimeout)
	}
	if server.MaxHeaderBytes != 64<<10 {
		t.Fatalf("server max header bytes = %d; want %d", server.MaxHeaderBytes, 64<<10)
	}
}

func TestMCPEnabledFromEnvDefaultsToCompatibility(t *testing.T) {
	t.Setenv("RSS_STUDIO_MCP_ENABLED", "")
	enabled, err := mcpEnabledFromEnv()
	if err != nil || !enabled {
		t.Fatalf("default MCP configuration = %t, %v; want enabled", enabled, err)
	}
}

func TestMCPEnabledFromEnvAcceptsExplicitDisable(t *testing.T) {
	t.Setenv("RSS_STUDIO_MCP_ENABLED", "false")
	enabled, err := mcpEnabledFromEnv()
	if err != nil || enabled {
		t.Fatalf("disabled MCP configuration = %t, %v; want disabled", enabled, err)
	}
}

func TestMCPEnabledFromEnvRejectsInvalidValue(t *testing.T) {
	t.Setenv("RSS_STUDIO_MCP_ENABLED", "sometimes")
	if _, err := mcpEnabledFromEnv(); err == nil {
		t.Fatal("invalid MCP configuration was accepted")
	}
}
