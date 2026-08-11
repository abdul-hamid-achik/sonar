package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
)

// Session 4d01085 logged the same "operand outside the workspace" refusal
// eight times: each approval cured exactly one command, because no grant kind
// could carry path authority. These tests pin the cure — one session grant
// covering shell READS under one named outside directory — and every boundary
// it must not cross.

func newReadDirTestAgent(t *testing.T) (*Agent, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("AUTO shell catalog requires a POSIX shell")
	}
	ag := New(nil, nil, 4096)
	workspace := t.TempDir()
	ag.SetWorkDir(workspace)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "data.txt"), []byte("x,y\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ag, workspace, outside
}

func grantShellReadDir(t *testing.T, ag *Agent, dir string) {
	t.Helper()
	request := permissionpkg.ApprovalRequest{
		ToolName: "bash",
		Scope: permissionpkg.ApprovalScope{
			Kind:      permissionpkg.ScopeSessionShellReadDir,
			Workspace: ag.approvalScopeWorkspace(),
			Resource:  dir,
		},
	}
	ag.rememberSessionApproval(request)
}

func TestShellReadDirGrantCuresPathRefusal(t *testing.T) {
	ag, _, outside := newReadDirTestAgent(t)
	command := "cat " + filepath.Join(outside, "data.txt")

	before := ag.assessAutoScopedCommand(command)
	if before.admitted() {
		t.Fatalf("outside read admitted without a grant: %#v", before)
	}
	if before.reason != autoCommandReasonPathAuthority {
		t.Fatalf("expected a path-authority refusal, got %#v", before)
	}

	dir := ag.autoCommandShellReadDir(AuthorityAutoScoped, command)
	if dir == "" {
		t.Fatal("host offered no shell-read directory for a read-only path refusal")
	}
	grantShellReadDir(t, ag, dir)

	after := ag.assessAutoScopedCommand(command)
	if !after.admitted() {
		t.Fatalf("grant for %q did not cure the refusal: %#v", dir, after)
	}
	if !after.usesReadGrant {
		t.Fatal("admission must be marked as grant-backed")
	}
}

func TestShellReadDirGrantCoversSiblingReadsNotJustOneCommand(t *testing.T) {
	ag, _, outside := newReadDirTestAgent(t)
	if err := os.WriteFile(filepath.Join(outside, "other.log"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := ag.autoCommandShellReadDir(AuthorityAutoScoped, "cat "+filepath.Join(outside, "data.txt"))
	grantShellReadDir(t, ag, dir)

	for _, command := range []string{
		"head -n 1 " + filepath.Join(outside, "other.log"),
		"wc -l " + filepath.Join(outside, "data.txt"),
		"awk '{print $1}' " + filepath.Join(outside, "data.txt"),
	} {
		if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
			t.Fatalf("sibling read %q not covered by the directory grant: %#v", command, assessment)
		}
	}
}

func TestShellReadDirGrantNeverAdmitsEffectfulCommands(t *testing.T) {
	ag, _, outside := newReadDirTestAgent(t)
	dir := ag.autoCommandShellReadDir(AuthorityAutoScoped, "cat "+filepath.Join(outside, "data.txt"))
	grantShellReadDir(t, ag, dir)

	for _, command := range []string{
		// Effectful classification: the grant is never consulted.
		"sort -o " + filepath.Join(outside, "out.txt") + " " + filepath.Join(outside, "data.txt"),
		"mkdir " + filepath.Join(outside, "newdir"),
		"touch " + filepath.Join(outside, "new.txt"),
		// Redirect targets stay workspace-only regardless of any grant.
		"cat " + filepath.Join(outside, "data.txt") + " > " + filepath.Join(outside, "copy.txt"),
	} {
		if assessment := ag.assessAutoScopedCommand(command); assessment.admitted() {
			t.Fatalf("effectful command %q rode in on a read grant: %#v", command, assessment)
		}
	}
}

func TestShellReadDirGrantStopsAtSecretPaths(t *testing.T) {
	ag, _, outside := newReadDirTestAgent(t)
	if err := os.WriteFile(filepath.Join(outside, ".env"), []byte("KEY=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := ag.autoCommandShellReadDir(AuthorityAutoScoped, "cat "+filepath.Join(outside, "data.txt"))
	grantShellReadDir(t, ag, dir)

	if assessment := ag.assessAutoScopedCommand("cat " + filepath.Join(outside, ".env")); assessment.admitted() {
		t.Fatal("a directory grant must not reach conventional secret paths beneath it")
	}
}

func TestShellReadDirOfferedOnlyForReadOnlyAutoRefusals(t *testing.T) {
	ag, _, outside := newReadDirTestAgent(t)

	if dir := ag.autoCommandShellReadDir(AuthorityAutoScoped, "mkdir "+filepath.Join(outside, "newdir")); dir != "" {
		t.Fatalf("effectful refusal must not carry a shell-read offer, got %q", dir)
	}
	if dir := ag.autoCommandShellReadDir(AuthorityNormal, "cat "+filepath.Join(outside, "data.txt")); dir != "" {
		t.Fatalf("NORMAL mode must not compute AUTO grant offers, got %q", dir)
	}
	if dir := ag.autoCommandShellReadDir(AuthorityAutoScoped, "cat data.txt"); dir != "" {
		t.Fatalf("a workspace-admitted command has nothing to offer, got %q", dir)
	}
}

func TestShellReadDirScopeBindsToHostComputedPreview(t *testing.T) {
	ag, _, outside := newReadDirTestAgent(t)
	command := "cat " + filepath.Join(outside, "data.txt")
	dir := ag.autoCommandShellReadDir(AuthorityAutoScoped, command)

	request := permissionpkg.ApprovalRequest{
		ToolName: "bash",
		Args:     map[string]any{"command": command},
		Preview:  permissionpkg.ApprovalPreview{ShellReadDir: dir},
		Scope:    permissionpkg.ApprovalScope{Workspace: ag.approvalScopeWorkspace()},
	}
	applySessionScope(&request, permissionpkg.ScopeSessionShellReadDir)
	if request.Scope.Kind != permissionpkg.ScopeSessionShellReadDir || request.Scope.Resource != dir {
		t.Fatalf("scope did not bind to the preview directory: %#v", request.Scope)
	}

	ag.rememberSessionApproval(request)
	if assessment := ag.assessAutoScopedCommand(command); !assessment.admitted() {
		t.Fatalf("full approval roundtrip did not cure the refusal: %#v", assessment)
	}
}
