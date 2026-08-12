package permission

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
)

func TestChecker_DefaultPolicy(t *testing.T) {
	c := NewChecker(nil, false)
	if got := c.Check("some_tool"); got != PolicyAsk {
		t.Errorf("Check() = %q, want %q", got, PolicyAsk)
	}
}

func TestCheckerSkipApprovalsAllowsUnconfiguredRequests(t *testing.T) {
	c := NewChecker(nil, true)
	if got := c.Check("any_tool"); got != PolicyAllow {
		t.Errorf("Check() = %q, want %q", got, PolicyAllow)
	}
	if got := c.ToCheckResult("any_tool"); got != CheckAllow {
		t.Errorf("ToCheckResult() = %v, want %v", got, CheckAllow)
	}
	if !c.IsYolo() {
		t.Error("expected IsYolo() = true")
	}
}

func TestCheckerSkipApprovalsPreservesInMemoryDeny(t *testing.T) {
	c := NewChecker(nil, true)
	mustSetPolicy(t, c, "bash", PolicyDeny)
	if got := c.Check("bash"); got != PolicyDeny {
		t.Fatalf("Check() = %q, want explicit %q", got, PolicyDeny)
	}
	if got := c.ToCheckResult("bash"); got != CheckDeny {
		t.Fatalf("ToCheckResult() = %v, want %v", got, CheckDeny)
	}
	if got := c.ToCheckResult("read"); got != CheckAllow {
		t.Fatalf("unconfigured request = %v, want %v", got, CheckAllow)
	}
}

func TestCheckerSkipApprovalsPreservesPersistedDeny(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "skip-deny.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	mustSetPolicy(t, NewChecker(store, false), "file_write", PolicyDeny)
	checker := NewChecker(store, true)
	if got := checker.Check("file_write"); got != PolicyDeny {
		t.Fatalf("persisted Check() = %q, want %q", got, PolicyDeny)
	}
	if got := checker.ToCheckResult("file_write"); got != CheckDeny {
		t.Fatalf("persisted ToCheckResult() = %v, want %v", got, CheckDeny)
	}
}

func TestChecker_SetPolicy(t *testing.T) {
	c := NewChecker(nil, false)
	if err := c.SetPolicy("bash", PolicyAllow); !errors.Is(err, ErrBroadAllowUnsupported) {
		t.Fatalf("SetPolicy(allow) error = %v, want ErrBroadAllowUnsupported", err)
	}
	if got := c.Check("bash"); got != PolicyAsk {
		t.Errorf("Check() after rejected allow = %q, want %q", got, PolicyAsk)
	}
	mustSetPolicy(t, c, "bash", PolicyDeny)
	if got := c.Check("bash"); got != PolicyDeny {
		t.Errorf("Check() = %q, want %q", got, PolicyDeny)
	}
}

func TestChecker_WithDB(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	c := NewChecker(store, false)
	mustSetPolicy(t, c, "file_write", PolicyDeny)

	// Create a new checker from the same DB to test persistence.
	c2 := NewChecker(store, false)
	if got := c2.Check("file_write"); got != PolicyDeny {
		t.Errorf("persisted Check() = %q, want %q", got, PolicyDeny)
	}
}

func TestCheckerIgnoresPersistedLegacyAllow(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "legacy-allow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.UpsertToolPermission(context.Background(), db.UpsertToolPermissionParams{
		ToolName: "bash",
		Policy:   string(PolicyAllow),
	}); err != nil {
		t.Fatal(err)
	}

	checker := NewChecker(store, false)
	if got := checker.Check("bash"); got != PolicyAsk {
		t.Fatalf("legacy persisted allow = %q, want %q", got, PolicyAsk)
	}
	if got := checker.ToCheckResult("bash"); got != CheckAsk {
		t.Fatalf("legacy persisted allow check result = %v, want CheckAsk", got)
	}
}

func TestChecker_Reset(t *testing.T) {
	c := NewChecker(nil, false)
	mustSetPolicy(t, c, "tool1", PolicyAsk)
	mustSetPolicy(t, c, "tool2", PolicyDeny)
	if err := c.Reset(); err != nil {
		t.Fatalf("reset: %v", err)
	}

	if got := c.Check("tool1"); got != PolicyAsk {
		t.Errorf("after reset Check() = %q, want %q", got, PolicyAsk)
	}
}

func TestCheckerResetWithDBClearsCacheAndPersistence(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "reset.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	checker := NewChecker(store, false)
	mustSetPolicy(t, checker, "bash", PolicyDeny)
	if err := checker.Reset(); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if got := checker.Check("bash"); got != PolicyAsk {
		t.Fatalf("cached policy after reset = %q, want %q", got, PolicyAsk)
	}
	if got := NewChecker(store, false).Check("bash"); got != PolicyAsk {
		t.Fatalf("persisted policy after reset = %q, want %q", got, PolicyAsk)
	}
}

func TestCheckerResetFailurePreservesCache(t *testing.T) {
	store, err := db.OpenPath(filepath.Join(t.TempDir(), "reset-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	checker := NewChecker(store, false)
	mustSetPolicy(t, checker, "bash", PolicyDeny)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if err := checker.Reset(); err == nil {
		t.Fatal("Reset() succeeded with a closed persistence store")
	}
	if got := checker.Check("bash"); got != PolicyDeny {
		t.Fatalf("cached policy after failed reset = %q, want preserved %q", got, PolicyDeny)
	}
}

func TestChecker_AllPolicies(t *testing.T) {
	c := NewChecker(nil, false)
	mustSetPolicy(t, c, "a", PolicyAsk)
	mustSetPolicy(t, c, "b", PolicyDeny)

	policies := c.AllPolicies()
	if len(policies) != 2 {
		t.Errorf("AllPolicies() len = %d, want 2", len(policies))
	}
	if policies["a"] != PolicyAsk {
		t.Errorf("policies[a] = %q, want %q", policies["a"], PolicyAsk)
	}
}

func TestToCheckResult(t *testing.T) {
	c := NewChecker(nil, false)
	mustSetPolicy(t, c, "denied", PolicyDeny)

	if c.ToCheckResult("unknown") != CheckAsk {
		t.Error("expected CheckAsk for unknown tool")
	}
	if c.ToCheckResult("denied") != CheckDeny {
		t.Error("expected CheckDeny for denied tool")
	}
}

func TestToCheckResult_Nil(t *testing.T) {
	var c *Checker
	if c.ToCheckResult("anything") != CheckAllow {
		t.Error("nil checker should return CheckAllow")
	}
}

func mustSetPolicy(t *testing.T, c *Checker, toolName string, policy Policy) {
	t.Helper()
	if err := c.SetPolicy(toolName, policy); err != nil {
		t.Fatalf("set policy for %s: %v", toolName, err)
	}
}

func TestRequestApprovalFailsClosedWithoutCallback(t *testing.T) {
	allowed, always := RequestApproval("bash", map[string]any{"command": "true"}, nil)
	if allowed || always {
		t.Fatalf("missing callback returned allowed=%v always=%v, want false/false", allowed, always)
	}
}

func TestRequestApprovalContextCancels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	block := make(chan struct{})
	allowed, always := RequestApprovalContext(ctx, "bash", nil, func(ApprovalRequest) { <-block })
	if allowed || always {
		t.Fatalf("cancelled approval returned allowed=%v always=%v", allowed, always)
	}
	close(block)
}

func TestAlwaysAllowResponds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	allowed, _ := RequestApprovalContext(ctx, "read", nil, AlwaysAllow)
	if !allowed {
		t.Fatal("AlwaysAllow did not approve")
	}
}

func TestResolveApprovalContextDistinguishesHostUserAndCancellation(t *testing.T) {
	t.Run("host refusal", func(t *testing.T) {
		response := ResolveApprovalContext(context.Background(), ApprovalRequest{ToolName: "write"}, func(request ApprovalRequest) {
			request.Response <- Refuse("preview_unavailable", "could not render diff")
		})
		if response.Decision != DecisionHostRefuse || response.Code != "preview_unavailable" || response.Allowed {
			t.Fatalf("response = %#v", response)
		}
	})

	t.Run("user deny", func(t *testing.T) {
		response := ResolveApprovalContext(context.Background(), ApprovalRequest{ToolName: "write"}, func(request ApprovalRequest) {
			request.Response <- Deny()
		})
		if response.Decision != DecisionUserDeny || response.Allowed {
			t.Fatalf("response = %#v", response)
		}
	})

	t.Run("missing host", func(t *testing.T) {
		response := ResolveApprovalContext(context.Background(), ApprovalRequest{ToolName: "write"}, nil)
		if response.Decision != DecisionHostRefuse || response.Code != "approval_ui_unavailable" {
			t.Fatalf("response = %#v", response)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		response := ResolveApprovalContext(ctx, ApprovalRequest{ToolName: "write"}, func(ApprovalRequest) {})
		if response.Decision != DecisionCancelled || response.Allowed {
			t.Fatalf("response = %#v", response)
		}
	})
}

func TestLegacyAlwaysNormalizesToSessionOnly(t *testing.T) {
	response := (ApprovalResponse{Allowed: true, Always: true}).Normalize()
	if response.Decision != DecisionAllowSession || !response.Allowed || !response.Always {
		t.Fatalf("response = %#v", response)
	}
	if response.ScopeKind != "" {
		t.Fatalf("legacy always retained scope kind %q", response.ScopeKind)
	}
}

func TestAllowSessionToolNormalizesWithScopeKind(t *testing.T) {
	response := AllowSessionTool().Normalize()
	if response.Decision != DecisionAllowSession || !response.Allowed || !response.Always {
		t.Fatalf("response = %#v", response)
	}
	if response.ScopeKind != ScopeSessionTool {
		t.Fatalf("scope kind = %q, want %q", response.ScopeKind, ScopeSessionTool)
	}
	// Unknown scope kinds fail closed to exact-request.
	cleared := (ApprovalResponse{Decision: DecisionAllowSession, ScopeKind: "path_prefix"}).Normalize()
	if cleared.ScopeKind != "" {
		t.Fatalf("unknown scope retained: %#v", cleared)
	}
}

// The TUI offers "shell reads under this directory this session" and
// "this MCP server this session". Those answers travel through
// ResolveApprovalContextWithTimeout, which Normalize()s every host
// response. Dropping the kinds here collapses them to exact-request.
func TestSessionReadDirAndMCPServerSurviveNormalize(t *testing.T) {
	for _, want := range []string{ScopeSessionShellReadDir, ScopeSessionMCPServer} {
		var constructor func() ApprovalResponse
		switch want {
		case ScopeSessionShellReadDir:
			constructor = AllowSessionShellReadDir
		case ScopeSessionMCPServer:
			constructor = AllowSessionMCPServer
		}
		got := constructor().Normalize()
		if got.Decision != DecisionAllowSession || got.ScopeKind != want {
			t.Fatalf("Normalize(%s) = %#v", want, got)
		}
		live := ResolveApprovalContextWithTimeout(
			context.Background(),
			ApprovalRequest{ToolName: "bash"},
			func(request ApprovalRequest) { request.Response <- constructor() },
			time.Second,
		)
		if live.Decision != DecisionAllowSession || live.ScopeKind != want {
			t.Fatalf("live approval %s = %#v", want, live)
		}
	}
}
