package config

import (
	"strings"
	"testing"
	"time"
)

// A slow-but-honest MCP server (building a semantic index, walking a large
// repository) can legitimately exceed the built-in call timeout. When it does,
// the call is "unanswered", and an unanswered effectful call becomes an
// outcome_unknown that halts the turn for manual reconciliation. Being able to
// raise the bound is what keeps that from happening on every large repo.
func TestMCPTimeoutIsConfigurable(t *testing.T) {
	base := func() Config {
		c := Defaults()
		return c
	}

	c := base()
	if got := c.MCPCallTimeout(); got != 0 {
		t.Errorf("unset mcp_timeout = %v, want 0 (built-in default)", got)
	}

	c = base()
	c.Tools.MCPTimeout = "5m"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid mcp_timeout rejected: %v", err)
	}
	if got := c.MCPCallTimeout(); got != 5*time.Minute {
		t.Errorf("mcp_timeout = %v, want 5m", got)
	}
}

func TestMCPTimeoutRejectsUnusableValues(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{"not-a-duration", "invalid tools.mcp_timeout"},
		{"0s", "must be positive"},
		{"-30s", "must be positive"},
		// A hung server must still fail eventually; without a ceiling a
		// misconfiguration could hold a turn open indefinitely, which is the
		// exact failure the timeout exists to prevent.
		{"48h", "exceeds"},
	} {
		c := Defaults()
		c.Tools.MCPTimeout = test.value
		err := c.Validate()
		if err == nil {
			t.Errorf("mcp_timeout %q was accepted", test.value)
			continue
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Errorf("mcp_timeout %q error = %v, want it to mention %q", test.value, err, test.want)
		}
	}
}
