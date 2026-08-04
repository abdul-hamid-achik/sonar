package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestInspectedDirectoryWriteGrantScopesBuiltinMutationAndReadback(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	sibling := filepath.Join(base, "sibling")
	for _, directory := range []string{workspace, external, sibling} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	canonicalExternal := grantInspectedWritePath(t, ag, external)
	if resolved, err := ag.resolvePath(filepath.Join("..", "external", "relative.txt")); err != nil || resolved != filepath.Join(canonicalExternal, "relative.txt") {
		t.Fatalf("relative external intent = %q, %v", resolved, err)
	}

	target := filepath.Join(external, "nested", "result.txt")
	writeCall := llm.ToolCall{Name: "write", Arguments: map[string]any{"path": target}}
	if !ag.authorityAutoApproves(AuthorityAutoScoped, writeCall, executionpkg.KindBuiltin) {
		t.Fatal("AUTO did not honor the explicit directory write authority")
	}
	if ag.authorityAutoApproves(AuthorityNormal, writeCall, executionpkg.KindBuiltin) {
		t.Fatal("temporary write authority silently widened NORMAL authority")
	}
	if result, isErr := ag.handleWrite(map[string]any{"path": target, "content": "bounded\n"}); isErr {
		t.Fatalf("external write = %q", result)
	}
	if result, isErr := ag.handleRead(map[string]any{"path": target}); isErr || result != "bounded\n" {
		t.Fatalf("write-authority readback = %q, error=%v", result, isErr)
	}
	if result, isErr := ag.handleGrep(context.Background(), map[string]any{
		"path": external, "pattern": "bounded",
	}); isErr || !strings.Contains(result, "result.txt") {
		t.Fatalf("write-authority grep = %q, error=%v", result, isErr)
	}

	outside := filepath.Join(sibling, "denied.txt")
	if ag.authorityAutoApproves(AuthorityAutoScoped, llm.ToolCall{
		Name: "write", Arguments: map[string]any{"path": outside},
	}, executionpkg.KindBuiltin) {
		t.Fatal("directory authority leaked to a sibling")
	}
	if result, isErr := ag.handleWrite(map[string]any{"path": outside, "content": "no"}); !isErr || !strings.Contains(result, "workspace") {
		t.Fatalf("sibling write = %q, error=%v", result, isErr)
	}
	if result, isErr := ag.handleRemove(map[string]any{"path": target}); !isErr || !strings.Contains(result, "workspace") {
		t.Fatalf("destructive operation inherited grant = %q, error=%v", result, isErr)
	}
}

func TestExactFileWriteGrantSupportsDiffEditWithoutSiblingAuthority(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	configDir := filepath.Join(base, ".config", "mcphub")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(configDir, "mcphub.yaml")
	sibling := filepath.Join(configDir, "other.yaml")
	if err := os.WriteFile(target, []byte("verbatim: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	grantInspectedWritePath(t, ag, target)

	for _, tool := range []string{"write", "edit"} {
		if !ag.authorityAutoApproves(AuthorityAutoScoped, llm.ToolCall{
			Name: tool, Arguments: map[string]any{"path": target},
		}, executionpkg.KindBuiltin) {
			t.Fatalf("AUTO did not honor exact-file authority for %s", tool)
		}
	}
	if ag.authorityAutoApproves(AuthorityAutoScoped, llm.ToolCall{
		Name: "mkdir", Arguments: map[string]any{"path": target},
	}, executionpkg.KindBuiltin) {
		t.Fatal("exact-file authority admitted mkdir")
	}
	if ag.authorityAutoApproves(AuthorityAutoScoped, llm.ToolCall{
		Name: "write", Arguments: map[string]any{"path": sibling},
	}, executionpkg.KindBuiltin) {
		t.Fatal("exact-file authority leaked to a sibling")
	}

	before, exists, omitted := ag.approvalExistingContent(target)
	if !exists || before != "verbatim: false\n" || omitted != "" {
		t.Fatalf("approval existing content = %q, exists=%v, omitted=%q", before, exists, omitted)
	}
	if result, isErr := ag.handleEdit(map[string]any{
		"path": target, "patch": "@@ -1,1 +1,1 @@\n-verbatim: false\n+verbatim: true",
	}); isErr {
		t.Fatalf("exact-file edit = %q", result)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "verbatim: true\n" {
		t.Fatalf("edited data = %q, err=%v", data, err)
	}
	if result, isErr := ag.handleWrite(map[string]any{"path": sibling, "content": "changed"}); !isErr {
		t.Fatalf("sibling write unexpectedly succeeded: %q", result)
	}
	if data, err := os.ReadFile(sibling); err != nil || string(data) != "untouched\n" {
		t.Fatalf("sibling changed: %q, err=%v", data, err)
	}

	// A shell recipe cannot smuggle a broader replacement through an exact
	// file grant. Built-in edit/write remain the only AUTO mutation surface.
	if ag.autoScopedCommandAllowed("cat /tmp/mcphub.yaml > " + target) {
		t.Fatal("dynamic shell overwrite inherited exact-file AUTO authority")
	}
	if ag.autoScopedCommandAllowed("cat " + target + " | sed 's/false/true/' > /tmp/mcphub.yaml && cp /tmp/mcphub.yaml " + target) {
		t.Fatal("cat|sed|cp fallback inherited exact-file AUTO authority")
	}
}

func TestWriteGrantRejectsBroadSensitiveSymlinkAndStaleScopes(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)

	if _, err := ag.InspectWritePath(string(filepath.Separator)); err == nil || !strings.Contains(err.Error(), "broad directory") {
		t.Fatalf("filesystem root inspection error = %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !broadWriteDirectory(home) || !broadWriteDirectory(filepath.Join(home, "projects")) {
		t.Fatal("home or an immediate home child was not classified as broad")
	}

	secret := filepath.Join(external, ".env")
	if err := os.WriteFile(secret, []byte("TOKEN=no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.InspectWritePath(secret); err == nil || !strings.Contains(err.Error(), "secret-bearing") {
		t.Fatalf("secret inspection error = %v", err)
	}
	link := filepath.Join(base, "external-link")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.InspectWritePath(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink inspection error = %v", err)
	}

	target := filepath.Join(external, "config.yaml")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := ag.InspectWritePath(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, target+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.AddInspectedWriteGrant(inspection.Grant()); err == nil || !strings.Contains(err.Error(), "changed after approval preview") {
		t.Fatalf("stale write inspection error = %v", err)
	}
	if grants := ag.WriteGrants(); len(grants) != 0 {
		t.Fatalf("replacement inherited stale authority: %#v", grants)
	}
}

func TestWriteGrantEnforcesAgentIgnoreAndRevokesEphemerally(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	external := filepath.Join(base, "external")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, ".agentignore"), []byte("private/**\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	grantInspectedWritePath(t, ag, external)
	blocked := filepath.Join(external, "private", "token.txt")
	if result, isErr := ag.handleWrite(map[string]any{"path": blocked, "content": "no"}); !isErr || !strings.Contains(result, "excluded") {
		t.Fatalf("ignored write = %q, error=%v", result, isErr)
	}
	if grants := ag.WriteGrants(); len(grants) != 1 || grants[0].identity != nil {
		t.Fatalf("display grants = %#v", grants)
	}
	count, err := ag.ClearWriteGrants()
	if err != nil || count != 1 {
		t.Fatalf("ClearWriteGrants = %d, %v", count, err)
	}
	if _, err := ag.resolvePath(filepath.Join(external, "public.txt")); err == nil {
		t.Fatal("cleared authority remained usable")
	}
	ag.Close()
}

func TestExactWriteGrantRejectsSymlinkReplacement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "config.yaml")
	sibling := filepath.Join(base, "sibling.yaml")
	if err := os.WriteFile(target, []byte("approved\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	grantInspectedWritePath(t, ag, target)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, target); err != nil {
		t.Fatal(err)
	}
	if result, isErr := ag.handleWrite(map[string]any{"path": target, "content": "changed\n"}); !isErr || !strings.Contains(result, "regular file") {
		t.Fatalf("symlink replacement write = %q, error=%v", result, isErr)
	}
	if data, err := os.ReadFile(sibling); err != nil || string(data) != "secret\n" {
		t.Fatalf("symlink target changed: %q, err=%v", data, err)
	}
}

func TestRemoveExactWriteGrantUsesApprovedLexicalEntryAfterSymlinkReplacement(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "config.yaml")
	sibling := filepath.Join(base, "sibling.yaml")
	for _, path := range []string{target, sibling} {
		if err := os.WriteFile(path, []byte("value\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	canonical := grantInspectedWritePath(t, ag, target)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, target); err != nil {
		t.Fatal(err)
	}
	removed, err := ag.RemoveWritePath(canonical)
	if err != nil || removed.Path != canonical || removed.Kind != WriteGrantExactFile {
		t.Fatalf("RemoveWritePath = %#v, %v", removed, err)
	}
	if grants := ag.WriteGrants(); len(grants) != 0 {
		t.Fatalf("revoked exact grant survived symlink replacement: %#v", grants)
	}
}

func TestBroadWriteDirectoryRejectsHostTempRoot(t *testing.T) {
	tempRoot := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(tempRoot); err == nil {
		tempRoot = resolved
	}
	if !broadWriteDirectory(tempRoot) {
		t.Fatalf("host temp root %q was eligible for broad write authority", tempRoot)
	}
	child, err := os.MkdirTemp(tempRoot, "sonar-write-scope-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(child) })
	if broadWriteDirectory(child) {
		t.Fatalf("an explicit temp child was incorrectly treated as the whole temp root: %q", child)
	}
}

func TestBroadWriteDirectoryRejectsDarwinVarTempAlias(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("/var/tmp host alias is a macOS boundary")
	}
	if _, err := os.Stat("/var/tmp"); err != nil {
		t.Skipf("/var/tmp unavailable: %v", err)
	}
	if !broadWriteDirectory("/var/tmp") {
		t.Fatal("/var/tmp was eligible for directory-wide write authority")
	}
}

func grantInspectedWritePath(t *testing.T, ag *Agent, path string) string {
	t.Helper()
	inspection, err := ag.InspectWritePath(path)
	if err != nil {
		t.Fatalf("InspectWritePath(%q): %v", path, err)
	}
	if !inspection.External || inspection.AlreadyWritable {
		t.Fatalf("write inspection = %#v", inspection)
	}
	canonical, err := ag.AddInspectedWriteGrant(inspection.Grant())
	if err != nil {
		t.Fatalf("AddInspectedWriteGrant(%q): %v", path, err)
	}
	return canonical
}
