package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

type approvalPersistenceOutput struct {
	outputRecorder
	system []string
}

func (o *approvalPersistenceOutput) SystemMessage(message string) {
	o.system = append(o.system, message)
}

func TestAllowSessionIsExactRequestScopedAndDoesNotPersistGlobally(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "always-approval.db")
	store, err := db.OpenPath(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scope := New(nil, nil, 4096)
	scope.SetWorkDir(workDir)
	workspaceID, err := scope.checkpointWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, db.CreateSessionParams{
		Title: "session approval", Model: "test-model", Mode: "BUILD", WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}

	var approvalPrompts atomic.Int64
	firstClient := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-first", Name: "write", Arguments: map[string]any{"path": "same.txt", "content": "same"}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "write-repeat", Name: "write", Arguments: map[string]any{"path": "same.txt", "content": "same"}}}, Done: true}},
		{{Text: "first complete", Done: true}},
	}}
	first := configuredPersistentApprovalAgent(firstClient, store, permission.NewChecker(store, false), workDir, session.ID, 0)
	first.SetApprovalCallback(func(request permission.ApprovalRequest) {
		approvalPrompts.Add(1)
		request.Response <- permission.AllowSession()
	})
	first.AddUserMessage("write the same file twice")
	if err := first.RunTurn(ctx, &approvalPersistenceOutput{}, "turn_always_first"); err != nil {
		t.Fatal(err)
	}
	if approvalPrompts.Load() != 1 {
		t.Fatalf("first turn approval prompts = %d, want 1", approvalPrompts.Load())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "same.txt")); err != nil || string(data) != "same" {
		t.Fatalf("first approved effect = %q err=%v", data, err)
	}
	if policy := permission.NewChecker(store, false).Check("write"); policy != permission.PolicyAsk {
		t.Fatalf("session approval persisted global write policy = %q", policy)
	}

	firstHazards, err := store.ListExecutionRecoveryHazards(ctx, session.ID, workspaceID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstHazards) != 2 {
		t.Fatalf("first completed execution states = %d, want 2", len(firstHazards))
	}
	for _, state := range firstHazards {
		firstEvents, eventsErr := store.ListExecutionEvents(ctx, session.ID, workspaceID, state.Identity.ExecutionID, 20)
		if eventsErr != nil {
			t.Fatal(eventsErr)
		}
		assertExecutionApproval(t, firstEvents, executionpkg.ApprovalSession)
	}
	firstCursor, err := store.LatestExecutionEventID(ctx, session.ID, workspaceID)
	if err != nil {
		t.Fatal(err)
	}

	secondClient := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-after-restart", Name: "write", Arguments: map[string]any{"path": "same.txt", "content": "same"}}}, Done: true}},
		{{Text: "second complete", Done: true}},
	}}
	restartedChecker := permission.NewChecker(store, false)
	second := configuredPersistentApprovalAgent(secondClient, store, restartedChecker, workDir, session.ID, firstCursor)
	second.SetApprovalCallback(func(request permission.ApprovalRequest) {
		approvalPrompts.Add(1)
		request.Response <- permission.AllowOnce()
	})
	second.AddUserMessage("write after restart")
	if err := second.RunTurn(ctx, &approvalPersistenceOutput{}, "turn_always_second"); err != nil {
		t.Fatal(err)
	}
	if approvalPrompts.Load() != 2 {
		t.Fatalf("session approval survived agent restart; total prompts=%d", approvalPrompts.Load())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "same.txt")); err != nil || string(data) != "same" {
		t.Fatalf("second policy-approved effect = %q err=%v", data, err)
	}
	secondHazards, err := store.ListExecutionRecoveryHazards(ctx, session.ID, workspaceID, firstCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondHazards) != 1 {
		t.Fatalf("second completed execution states = %d, want 1", len(secondHazards))
	}
	secondEvents, err := store.ListExecutionEvents(ctx, session.ID, workspaceID, secondHazards[0].Identity.ExecutionID, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertExecutionApproval(t, secondEvents, executionpkg.ApprovalOnce)
}

func TestAllowSessionDoesNotAuthorizeChangedArguments(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-one", Name: "write", Arguments: map[string]any{"path": "approved.txt", "content": "one"}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "write-two", Name: "write", Arguments: map[string]any{"path": "approved.txt", "content": "two"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.AllowSession()
	})
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_scoped_session"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 2 {
		t.Fatalf("changed canonical arguments reused session grant; prompts=%d", prompts.Load())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "approved.txt")); err != nil || string(data) != "two" {
		t.Fatalf("final approved effect = %q err=%v", data, err)
	}
	approved := 0
	for _, event := range ledger.snapshot() {
		if event.Type == executionpkg.EventApproved && event.Approval == executionpkg.ApprovalSession {
			approved++
		}
	}
	if approved != 2 {
		t.Fatalf("session-approved events = %d, want 2", approved)
	}
}

func TestAllowSessionToolAllowsChangedArgumentsWithoutRePrompt(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-one", Name: "write", Arguments: map[string]any{"path": "approved.txt", "content": "one"}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "write-two", Name: "write", Arguments: map[string]any{"path": "approved.txt", "content": "two"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ledger := &fakeExecutionLedger{}
	ag, workDir := newLedgerAgent(t, client, nil, ledger)
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.AllowSessionTool()
	})
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_session_tool"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 1 {
		t.Fatalf("session_tool re-prompted for changed content; prompts=%d", prompts.Load())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "approved.txt")); err != nil || string(data) != "two" {
		t.Fatalf("final approved effect = %q err=%v", data, err)
	}
	summaries := ag.ListSessionApprovalSummary()
	if len(summaries) != 1 || !strings.Contains(summaries[0], "write") || !strings.Contains(summaries[0], permission.ScopeSessionTool) {
		t.Fatalf("session summary = %#v", summaries)
	}
	// Session grants are process-local: a new agent does not inherit them.
	secondClient := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-restart", Name: "write", Arguments: map[string]any{"path": "approved.txt", "content": "three"}}}, Done: true}},
		{{Text: "restarted", Done: true}},
	}}
	second, _ := newLedgerAgent(t, secondClient, nil, &fakeExecutionLedger{})
	second.SetWorkDir(workDir)
	second.SetPermissionChecker(permission.NewChecker(nil, false))
	var restartedPrompts atomic.Int64
	second.SetApprovalCallback(func(request permission.ApprovalRequest) {
		restartedPrompts.Add(1)
		request.Response <- permission.AllowOnce()
	})
	if err := second.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_session_tool_restart"); err != nil {
		t.Fatal(err)
	}
	if restartedPrompts.Load() != 1 {
		t.Fatalf("session_tool grant survived agent restart; prompts=%d", restartedPrompts.Load())
	}
}

func TestAllowSessionBashPrefixCoversRelatedCommands(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "bash-one", Name: "bash", Arguments: map[string]any{"command": "go test ./..."}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "bash-two", Name: "bash", Arguments: map[string]any{"command": "go test ./internal/agent -count=1"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag, _ := newLedgerAgent(t, client, nil, &fakeExecutionLedger{})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.AllowSessionBashPrefix()
	})
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_bash_prefix"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 1 {
		t.Fatalf("bash prefix re-prompted; prompts=%d", prompts.Load())
	}
}

func TestAllowSessionPathCoversContentChangeSamePath(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-one", Name: "write", Arguments: map[string]any{"path": "same.txt", "content": "one"}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "write-two", Name: "write", Arguments: map[string]any{"path": "same.txt", "content": "two"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag, workDir := newLedgerAgent(t, client, nil, &fakeExecutionLedger{})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.AllowSessionPath()
	})
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_path_scope"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 1 {
		t.Fatalf("path scope re-prompted; prompts=%d", prompts.Load())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "same.txt")); err != nil || string(data) != "two" {
		t.Fatalf("effect = %q err=%v", data, err)
	}
}

func TestAllowSessionPathSharedAcrossWriteFamily(t *testing.T) {
	// Approving write for a path should also cover a later mkdir? mkdir is different path.
	// Cover write then write again was tested; for family, mkdir on same path is odd.
	// Use write then edit if edit applies patch - simpler: two tools write and second write after
	// remembering with different tool names is already same tool. Force manual grant check.
	ag, workDir := newLedgerAgent(t, &scriptedClient{}, nil, &fakeExecutionLedger{})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	path := filepath.Join(workDir, "shared.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a remembered path grant from write (same Abs+Clean as newApprovalRequest).
	ws, err := filepath.Abs(workDir)
	if err != nil {
		t.Fatal(err)
	}
	ws = filepath.Clean(ws)
	absPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	absPath = filepath.Clean(absPath)
	req := permission.ApprovalRequest{
		ToolName: "write",
		Scope: permission.ApprovalScope{
			Kind:      permission.ScopeSessionPath,
			Workspace: ws,
			Resource:  absPath,
		},
	}
	ag.rememberSessionApproval(req)

	// Build a write-family request as decideToolAuthorization would for edit.
	editReq := permission.ApprovalRequest{
		ToolName: "edit",
		Args:     map[string]any{"path": "shared.txt"},
		Preview:  permission.ApprovalPreview{Path: absPath},
		Scope: permission.ApprovalScope{
			Kind:      permission.ScopeExactRequest,
			Workspace: ws,
			Resource:  "hash",
		},
	}
	if !ag.hasSessionApproval(editReq) {
		t.Fatal("edit on same path should hit shared path grant")
	}
	mkdirReq := editReq
	mkdirReq.ToolName = "mkdir"
	if !ag.hasSessionApproval(mkdirReq) {
		t.Fatal("mkdir on same path should hit shared path grant")
	}
	// Summary should mention path family / session_path.
	summaries := ag.ListSessionApprovalSummary()
	if len(summaries) != 1 || !strings.Contains(summaries[0], permission.ScopeSessionPath) {
		t.Fatalf("summary = %#v", summaries)
	}
}

func TestWorkspaceBashRuleSkipsPrompt(t *testing.T) {
	store, err := permission.NewWorkspaceRulesStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "bash-one", Name: "bash", Arguments: map[string]any{"command": "go test ./..."}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag, workDir := newLedgerAgent(t, client, nil, &fakeExecutionLedger{})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	ag.SetWorkspaceRulesStore(store)
	if _, err := ag.AddWorkspaceBashPrefix("go test *"); err != nil {
		t.Fatal(err)
	}
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.Deny()
	})
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_workspace_bash"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 0 {
		t.Fatalf("workspace bash rule still prompted; prompts=%d", prompts.Load())
	}
	_ = workDir
}

func TestWorkspaceWritePathRuleSkipsPrompt(t *testing.T) {
	store, err := permission.NewWorkspaceRulesStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-one", Name: "write", Arguments: map[string]any{"path": "tracked.txt", "content": "v1"}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "write-two", Name: "write", Arguments: map[string]any{"path": "tracked.txt", "content": "v2"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag, workDir := newLedgerAgent(t, client, nil, &fakeExecutionLedger{})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	ag.SetWorkspaceRulesStore(store)
	target := filepath.Join(workDir, "tracked.txt")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.AddWorkspaceWritePath(target); err != nil {
		t.Fatal(err)
	}
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.Deny()
	})
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_workspace_path"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 0 {
		t.Fatalf("workspace write path still prompted; prompts=%d", prompts.Load())
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "v2" {
		t.Fatalf("effect = %q err=%v", data, err)
	}
}

func TestAllowSessionToolNotAppliedForBash(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "bash-one", Name: "bash", Arguments: map[string]any{"command": "true"}}}, Done: true}},
		{{ToolCalls: []llm.ToolCall{{ID: "bash-two", Name: "bash", Arguments: map[string]any{"command": "echo changed"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag, _ := newLedgerAgent(t, client, nil, &fakeExecutionLedger{})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		// Even if the host incorrectly returns session_tool for bash, the agent
		// must fall back to exact-request and re-prompt when arguments change.
		request.Response <- permission.AllowSessionTool()
	})
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_bash_session_tool"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 2 {
		t.Fatalf("bash session_tool widened tool grant; prompts=%d", prompts.Load())
	}
	for _, summary := range ag.ListSessionApprovalSummary() {
		if strings.Contains(summary, permission.ScopeSessionTool) {
			t.Fatalf("bash remembered session_tool grant: %#v", ag.ListSessionApprovalSummary())
		}
	}
	if got := len(ag.ListSessionApprovalSummary()); got != 2 {
		t.Fatalf("bash exact grants = %d, want 2 exact-request entries (%#v)", got, ag.ListSessionApprovalSummary())
	}
}

func TestAcceptWorkspaceEditsAutoApprovesWriteInNormalWithoutCallback(t *testing.T) {
	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{ID: "write-auto", Name: "write", Arguments: map[string]any{"path": "auto.txt", "content": "ok"}}}, Done: true}},
		{{Text: "done", Done: true}},
	}}
	ag, workDir := newLedgerAgent(t, client, nil, &fakeExecutionLedger{})
	ag.SetPermissionChecker(permission.NewChecker(nil, false))
	ag.SetAuthorityMode(AuthorityNormal)
	ag.SetApprovalPosture(ApprovalPostureAcceptWorkspaceEdits)
	var prompts atomic.Int64
	ag.SetApprovalCallback(func(request permission.ApprovalRequest) {
		prompts.Add(1)
		request.Response <- permission.Deny()
	})
	if !ag.authorityAutoApproves(AuthorityNormal, llm.ToolCall{
		Name: "write", Arguments: map[string]any{"path": "auto.txt", "content": "ok"},
	}, executionpkg.KindBuiltin) {
		t.Fatal("accept-edits did not auto-approve NORMAL write")
	}
	if err := ag.RunTurn(context.Background(), &approvalPersistenceOutput{}, "turn_accept_edits"); err != nil {
		t.Fatal(err)
	}
	if prompts.Load() != 0 {
		t.Fatalf("accept-edits still prompted; prompts=%d", prompts.Load())
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "auto.txt")); err != nil || string(data) != "ok" {
		t.Fatalf("auto-approved write = %q err=%v", data, err)
	}
	if ag.authorityAutoApproves(AuthorityNormal, llm.ToolCall{
		Name: "bash", Arguments: map[string]any{"command": "true"},
	}, executionpkg.KindBuiltin) {
		t.Fatal("accept-edits auto-approved bash")
	}
}

func TestAcceptWorkspaceEditsHonorsExplicitDeny(t *testing.T) {
	workspace := t.TempDir()
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(workspace)
	checker := permission.NewChecker(nil, false)
	if err := checker.SetPolicy("write", permission.PolicyDeny); err != nil {
		t.Fatal(err)
	}
	ag.SetPermissionChecker(checker)
	ag.SetApprovalPosture(ApprovalPostureAcceptWorkspaceEdits)
	if ag.authorityAutoApproves(AuthorityNormal, llm.ToolCall{
		Name: "write", Arguments: map[string]any{"path": "denied.txt", "content": "no"},
	}, executionpkg.KindBuiltin) {
		t.Fatal("accept-edits overrode explicit deny")
	}
}

func configuredPersistentApprovalAgent(client llm.Client, ledger *db.Store, checker *permission.Checker, workDir string, sessionID, cursor int64) *Agent {
	ag := New(client, nil, 4096)
	ag.SetWorkDir(workDir)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(checker)
	ag.SetExecutionLedger(ledger)
	ag.SetExecutionSessionID(sessionID, "")
	ag.SetExecutionSnapshotCursor(cursor)
	ag.RequireExecutionLedger(true)
	return ag
}

func assertExecutionApproval(t *testing.T, events []executionpkg.Event, want executionpkg.Approval) {
	t.Helper()
	for _, event := range events {
		if event.Type == executionpkg.EventApproved {
			if event.Approval != want {
				t.Fatalf("approved event = %q, want %q", event.Approval, want)
			}
			return
		}
	}
	t.Fatalf("execution events contain no approved receipt: %#v", events)
}
