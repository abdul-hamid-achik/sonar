package mcp

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestChildEnvironmentDropsAmbientSecrets(t *testing.T) {
	got := childEnvironment([]string{
		"PATH=/usr/bin:/bin",
		"HOME=/Users/test",
		"LANG=en_US.UTF-8",
		"TVAULT_PASSPHRASE=secret",
		"OPENAI_API_KEY=secret",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
	}, nil)

	for _, forbidden := range []string{
		"TVAULT_PASSPHRASE=secret",
		"OPENAI_API_KEY=secret",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
	} {
		if slices.Contains(got, forbidden) {
			t.Fatalf("ambient credential leaked to child: %q in %#v", forbidden, got)
		}
	}
	if !slices.Contains(filepath.SplitList(environmentValue(got, "PATH")), "/usr/bin") || !slices.Contains(got, "HOME=/Users/test") {
		t.Fatalf("runtime environment was not preserved: %#v", got)
	}
}

func TestChildEnvironmentAllowsExplicitValuesAndOverrides(t *testing.T) {
	got := childEnvironment(
		[]string{"PATH=/usr/bin", "TVAULT_AGENT_TOKEN=ambient"},
		[]string{"PATH=/opt/tools:/usr/bin", "TVAULT_AGENT_TOKEN=scoped"},
	)
	pathParts := filepath.SplitList(environmentValue(got, "PATH"))
	if !slices.Contains(pathParts, "/opt/tools") || !slices.Contains(pathParts, "/usr/bin") {
		t.Fatalf("explicit PATH did not override inherited value: %#v", got)
	}
	if !slices.Contains(got, "TVAULT_AGENT_TOKEN=scoped") {
		t.Fatalf("explicit capability was not passed: %#v", got)
	}
	if slices.Contains(got, "TVAULT_AGENT_TOKEN=ambient") {
		t.Fatalf("ambient capability survived explicit override: %#v", got)
	}
}

func environmentValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
