package main

import "testing"

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
