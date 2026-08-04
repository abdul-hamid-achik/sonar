package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
)

func TestTurnFilesystemSnapshotPinsEmbeddingUpdatesForNextTurn(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	ag := New(nil, nil, 0)
	ag.SetWorkDir(first)
	ag.SetIgnoreContent("first/**\n")
	pinned := ag.pinTurnFilesystem()
	t.Cleanup(ag.unpinTurnFilesystem)
	firstPolicy := config.EffectiveIgnoreContent("first/**\n")
	secondPolicy := config.EffectiveIgnoreContent("second/**\n")

	ag.SetWorkDir(second)
	ag.SetIgnoreContent("second/**\n")
	active := ag.filesystemContext()
	if active.workDir != first || active.ignoreContent != firstPolicy || active.version != pinned.version {
		t.Fatalf("active filesystem context changed: %#v", active)
	}
	if ag.WorkDir() != second {
		t.Fatalf("configured next workspace = %q, want %q", ag.WorkDir(), second)
	}
	resolved, err := ag.resolvePath("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	canonicalFirst, err := filepath.EvalSymlinks(first)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(resolved) != canonicalFirst {
		t.Fatalf("active turn resolved under %q, want %q", filepath.Dir(resolved), canonicalFirst)
	}

	ag.unpinTurnFilesystem()
	next := ag.filesystemContext()
	if next.workDir != second || next.ignoreContent != secondPolicy {
		t.Fatalf("next filesystem context = %#v", next)
	}
}

func TestInteractiveApprovalFailsClosedWhenFilesystemStateChanges(t *testing.T) {
	ag := New(nil, nil, 0)
	ag.SetWorkDir(t.TempDir())
	ag.SetPermissionChecker(permissionpkg.NewChecker(nil, false))
	ag.SetApprovalCallback(func(request permissionpkg.ApprovalRequest) {
		ag.SetIgnoreContent("newly-private/**\n")
		request.Response <- permissionpkg.AllowOnce()
	})

	authorization, err := ag.decideToolAuthorization(context.Background(), llm.ToolCall{
		ID:   "approval-filesystem-change",
		Name: "write",
		Arguments: map[string]any{
			"path":    "out.txt",
			"content": "content",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.allowed || !authorization.hostRefused || authorization.refusalCode != "approval_state_changed" {
		t.Fatalf("authorization = %#v", authorization)
	}
}

func TestInteractiveApprovalKeepsPinnedTurnFilesystem(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	ag := New(nil, nil, 0)
	ag.SetWorkDir(first)
	ag.SetPermissionChecker(permissionpkg.NewChecker(nil, false))
	ag.pinTurnFilesystem()
	t.Cleanup(ag.unpinTurnFilesystem)
	ag.SetApprovalCallback(func(request permissionpkg.ApprovalRequest) {
		ag.SetWorkDir(second)
		ag.SetIgnoreContent("next-turn/**\n")
		request.Response <- permissionpkg.AllowOnce()
	})

	authorization, err := ag.decideToolAuthorization(context.Background(), llm.ToolCall{
		ID:   "approval-pinned-filesystem",
		Name: "write",
		Arguments: map[string]any{
			"path":    "out.txt",
			"content": "content",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !authorization.allowed || authorization.hostRefused {
		t.Fatalf("authorization = %#v", authorization)
	}
	resolved, err := ag.resolvePath("out.txt")
	if err != nil {
		t.Fatal(err)
	}
	canonicalFirst, err := filepath.EvalSymlinks(first)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(resolved) != canonicalFirst {
		t.Fatalf("approved path resolved under %q, want %q", filepath.Dir(resolved), canonicalFirst)
	}
}

func TestInteractiveApprovalFailsClosedWhenPermissionChanges(t *testing.T) {
	checker := permissionpkg.NewChecker(nil, false)
	ag := New(nil, nil, 0)
	ag.SetWorkDir(t.TempDir())
	ag.SetPermissionChecker(checker)
	ag.SetApprovalCallback(func(request permissionpkg.ApprovalRequest) {
		if err := checker.SetPolicy("write", permissionpkg.PolicyDeny); err != nil {
			t.Errorf("SetPolicy: %v", err)
		}
		request.Response <- permissionpkg.AllowOnce()
	})

	authorization, err := ag.decideToolAuthorization(context.Background(), llm.ToolCall{
		ID:   "approval-policy-change",
		Name: "write",
		Arguments: map[string]any{
			"path":    "out.txt",
			"content": "content",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.allowed || !authorization.hostRefused || authorization.refusalCode != "approval_state_changed" {
		t.Fatalf("authorization = %#v", authorization)
	}
}

func TestInteractiveApprovalFailsClosedWhenCallbackChanges(t *testing.T) {
	ag := New(nil, nil, 0)
	ag.SetWorkspacePolicy(t.TempDir(), "")
	ag.SetPermissionChecker(permissionpkg.NewChecker(nil, false))
	ag.SetApprovalCallback(func(request permissionpkg.ApprovalRequest) {
		ag.SetApprovalCallback(permissionpkg.AlwaysAllow)
		request.Response <- permissionpkg.AllowOnce()
	})

	authorization, err := ag.decideToolAuthorization(context.Background(), llm.ToolCall{
		ID:   "approval-callback-change",
		Name: "write",
		Arguments: map[string]any{
			"path":    "out.txt",
			"content": "content",
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization.allowed || !authorization.hostRefused || authorization.refusalCode != "approval_state_changed" {
		t.Fatalf("authorization = %#v", authorization)
	}
}

func TestInteractiveApprovalRejectsChangedTargetPreview(t *testing.T) {
	for _, test := range []struct {
		name      string
		initial   *string
		changed   string
		tool      string
		arguments func(string) map[string]any
	}{
		{
			name:    "existing write target content",
			initial: pointerTo("before\n"),
			changed: "changed during modal\n",
			tool:    "write",
			arguments: func(path string) map[string]any {
				return map[string]any{"path": path, "content": "approved replacement\n"}
			},
		},
		{
			name:    "nonexistent write target created",
			changed: "created during modal\n",
			tool:    "write",
			arguments: func(path string) map[string]any {
				return map[string]any{"path": path, "content": "approved creation\n"}
			},
		},
		{
			name:    "edit source changed",
			initial: pointerTo("old\n"),
			changed: "other\n",
			tool:    "edit",
			arguments: func(path string) map[string]any {
				return map[string]any{"path": path, "patch": "@@ -1,1 +1,1 @@\n-old\n+new"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			target := filepath.Join(workspace, "target.txt")
			if test.initial != nil {
				if err := os.WriteFile(target, []byte(*test.initial), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ag := New(nil, nil, 0)
			ag.SetWorkspacePolicy(workspace, "")
			ag.SetPermissionChecker(permissionpkg.NewChecker(nil, false))
			ag.SetApprovalCallback(func(request permissionpkg.ApprovalRequest) {
				if err := os.WriteFile(target, []byte(test.changed), 0o600); err != nil {
					t.Errorf("mutate target: %v", err)
				}
				request.Response <- permissionpkg.AllowOnce()
			})

			authorization, err := ag.decideToolAuthorization(context.Background(), llm.ToolCall{
				ID:        "approval-target-change",
				Name:      test.tool,
				Arguments: test.arguments(target),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if authorization.allowed || !authorization.hostRefused || authorization.refusalCode != "approval_state_changed" {
				t.Fatalf("authorization = %#v", authorization)
			}
		})
	}
}

func pointerTo(value string) *string { return &value }

func TestSetWorkspacePolicyNeverSnapshotsMixedPair(t *testing.T) {
	ag := New(nil, nil, 0)
	type pair struct {
		dir    string
		ignore string
	}
	pairs := []pair{
		{dir: t.TempDir(), ignore: "first/**\n"},
		{dir: t.TempDir(), ignore: "second/**\n"},
	}
	ag.SetWorkspacePolicy(pairs[0].dir, pairs[0].ignore)

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for index := 0; index < 2_000; index++ {
			selected := pairs[index%len(pairs)]
			ag.SetWorkspacePolicy(selected.dir, selected.ignore)
		}
	}()
	go func() {
		defer workers.Done()
		for index := 0; index < 2_000; index++ {
			snapshot := ag.filesystemContext()
			valid := (snapshot.workDir == pairs[0].dir && snapshot.ignoreContent == config.EffectiveIgnoreContent(pairs[0].ignore)) ||
				(snapshot.workDir == pairs[1].dir && snapshot.ignoreContent == config.EffectiveIgnoreContent(pairs[1].ignore))
			if !valid {
				t.Errorf("mixed workspace policy snapshot: %#v", snapshot)
				return
			}
		}
	}()
	workers.Wait()
}

func TestFilesystemEmbeddingSettersAreRaceSafe(t *testing.T) {
	ag := New(nil, nil, 0)
	first := t.TempDir()
	second := t.TempDir()
	workspaces := []string{first, second}

	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		worker := worker
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := 0; index < 200; index++ {
				ag.SetWorkDir(workspaces[(worker+index)%len(workspaces)])
				ag.SetIgnoreContent("ignored-" + workspaces[index%len(workspaces)])
				_ = ag.WorkDir()
				_ = ag.filesystemContext()
			}
		}()
	}
	workers.Wait()
}
