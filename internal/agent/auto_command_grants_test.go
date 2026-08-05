package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

// installGrantTestExecutables places host-owned fixture executables on PATH so
// grant assessments do not depend on which tools the test machine has.
func installGrantTestExecutables(t *testing.T, names ...string) {
	t.Helper()
	hostBin := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(hostBin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("install host executable %s: %v", name, err)
		}
	}
	t.Setenv("PATH", hostBin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSessionBashPrefixGrantAuthorizesValidatedSegments is the end of the
// "always is a placebo" failure: in the audited session the user pressed an
// always-style allow 14 times and zero grants ever fired, because every
// prompted command carried a composition marker, DeriveBashPrefix refused to
// derive from it, and the exact-request fallback keyed on an argument hash a
// model never re-sends byte-identically. The test drives the same code path
// decideToolAuthorization uses after DecisionAllowSession.
func TestSessionBashPrefixGrantAuthorizesValidatedSegments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	installGrantTestExecutables(t, "go", "node")
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())

	compound := "node scripts/check.js && go test ./..."
	if ag.assessAutoScopedCommand(compound).admitted() {
		t.Fatal("uncatalogued segment gained AUTO authority before any grant")
	}

	call := llm.ToolCall{Name: "bash", Arguments: map[string]any{"command": compound}}
	request := ag.newApprovalRequest(context.Background(), AuthorityAutoScoped, call, "hash")
	applySessionScope(&request, permission.ScopeSessionBashPrefix)
	if request.Scope.Kind != permission.ScopeSessionBashPrefix {
		t.Fatalf("compound command did not derive a prefix scope: %#v", request.Scope)
	}
	ag.rememberSessionApproval(request)

	if assessment := ag.assessAutoScopedCommand(compound); !assessment.admitted() {
		t.Fatalf("granted segment still refused after the always grant: %#v", assessment)
	}
	// The derived grant is "node" — the same executable-level breadth the
	// simple form `node scripts/check.js` has always produced — so a variant
	// script and tail are covered as long as every operand stays confined.
	variant := "node scripts/other.js --verbose && go vet ./..."
	if assessment := ag.assessAutoScopedCommand(variant); !assessment.admitted() {
		t.Fatalf("granted prefix did not cover a variant tail: %#v", assessment)
	}

	for _, command := range []string{
		// The grant covers its own segment, never a neighbour.
		"node scripts/check.js && rm -rf .",
		// A different executable falls outside the derived prefix.
		"deno scripts/check.js && go test ./...",
		// Path authority still applies inside a granted segment.
		"node scripts/check.js /etc/passwd && go test ./...",
		"node scripts/check.js ../outside.js && go test ./...",
		// Composition checks are never grant-bypassed.
		"node scripts/check.js $(cat /etc/passwd) && go test ./...",
		"node scripts/check.js > /tmp/leak.txt && go test ./...",
	} {
		t.Run(command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("grant leaked beyond its segment authority: %#v", assessment)
			}
		})
	}
}

func TestDurableBashPrefixRuleAuthorizesValidatedSegments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	installGrantTestExecutables(t, "go", "node")
	store, err := permission.NewWorkspaceRulesStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	ag.SetWorkspaceRulesStore(store)
	if _, err := ag.AddWorkspaceBashPrefix("node *"); err != nil {
		t.Fatal(err)
	}
	if assessment := ag.assessAutoScopedCommand("node scripts/check.js && go test ./..."); !assessment.admitted() {
		t.Fatalf("durable rule did not authorize its segment: %#v", assessment)
	}
	if assessment := ag.assessAutoScopedCommand("node scripts/check.js && rm -rf ."); assessment.admitted() {
		t.Fatalf("durable rule leaked beyond its segment: %#v", assessment)
	}
}

// TestBashGrantNeverCuresWorkspaceExecutableProvenance pins that a user rule
// supplies executable authority by NAME while the host still owns provenance:
// PATH resolution reaching a workspace-resident binary stays refused (the
// planted-binary rule), and a path-qualified spelling of a granted name never
// matches the grant.
func TestBashGrantNeverCuresWorkspaceExecutableProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "evil"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	store, err := permission.NewWorkspaceRulesStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	ag.SetWorkspaceRulesStore(store)
	if _, err := ag.AddWorkspaceBashPrefix("evil *"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"evil payload",
		"./evil payload",
		"evil payload && echo done",
	} {
		t.Run(command, func(t *testing.T) {
			if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
				t.Fatalf("workspace-resident executable gained AUTO authority through a grant: %#v", assessment)
			}
		})
	}
}
