package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

const (
	executionCrashHelperEnv       = "SONAR_EXECUTION_CRASH_HELPER"
	executionCrashPhaseEnv        = "SONAR_EXECUTION_CRASH_PHASE"
	executionCrashDBEnv           = "SONAR_EXECUTION_CRASH_DB"
	executionCrashSessionEnv      = "SONAR_EXECUTION_CRASH_SESSION"
	executionCrashWorkspaceEnv    = "SONAR_EXECUTION_CRASH_WORKSPACE"
	executionCrashTargetEnv       = "SONAR_EXECUTION_CRASH_TARGET"
	executionCrashLeaseMarkerEnv  = "SONAR_EXECUTION_CRASH_LEASE_MARKER"
	executionCrashOutputMarkerEnv = "SONAR_EXECUTION_CRASH_OUTPUT_MARKER"
	executionCrashExitCode        = 86
)

type executionCrashPhase string

const (
	executionCrashAfterStarted         executionCrashPhase = "after_started"
	executionCrashBeforeTerminalCommit executionCrashPhase = "before_terminal_commit"
	executionCrashAfterTerminalCommit  executionCrashPhase = "after_terminal_commit"
)

// storeCrashLedger deliberately terminates the helper process at a precise
// append boundary. The wrapped Store remains the authority for every event
// that is meant to be durable in that phase.
type storeCrashLedger struct {
	store *db.Store
	phase executionCrashPhase
}

func (l *storeCrashLedger) AppendExecutionEvent(ctx context.Context, event executionpkg.Event) (executionpkg.Event, bool, error) {
	if l.phase == executionCrashBeforeTerminalCommit && event.Type.Terminal() {
		os.Exit(executionCrashExitCode)
	}
	stored, inserted, err := l.store.AppendExecutionEvent(ctx, event)
	if err != nil {
		return executionpkg.Event{}, false, err
	}
	if l.phase == executionCrashAfterStarted && event.Type == executionpkg.EventStarted {
		os.Exit(executionCrashExitCode)
	}
	if l.phase == executionCrashAfterTerminalCommit && event.Type == executionpkg.EventCompleted {
		os.Exit(executionCrashExitCode)
	}
	return stored, inserted, nil
}

func (l *storeCrashLedger) ListExecutionRecoveryHazards(ctx context.Context, sessionID int64, workspaceID string, afterEventID int64, limit int) ([]executionpkg.State, error) {
	return l.store.ListExecutionRecoveryHazards(ctx, sessionID, workspaceID, afterEventID, limit)
}

type crashMarkerOutput struct {
	markerPath string
}

func (*crashMarkerOutput) StreamText(string)                            {}
func (*crashMarkerOutput) StreamReasoning(string)                       {}
func (*crashMarkerOutput) StreamDone(int, int)                          {}
func (*crashMarkerOutput) ToolCallStart(string, string, map[string]any) {}
func (o *crashMarkerOutput) ToolCallResult(string, string, string, bool, time.Duration) {
	_ = os.WriteFile(o.markerPath, []byte("tool result reached UI"), 0o600)
}
func (*crashMarkerOutput) SystemMessage(string) {}
func (*crashMarkerOutput) Error(string)         {}

// Keep the lease reachable for the lifetime of the crash helper. In
// particular, do not let os.File's finalizer release it while Agent.Run is
// between the durable boundary and the deliberate process exit.
var executionCrashHeldLease *db.ExecutionSessionLease

// TestExecutionLedgerCrashHelperProcess is invoked by
// TestExecutionLedgerRecoversAcrossProcessCrash in a fresh copy of this test
// binary. os.Exit intentionally bypasses all deferred cleanup so SQLite and
// the kernel-backed session lease experience an actual process crash.
func TestExecutionLedgerCrashHelperProcess(t *testing.T) {
	if os.Getenv(executionCrashHelperEnv) != "1" {
		return
	}

	phase := executionCrashPhase(os.Getenv(executionCrashPhaseEnv))
	switch phase {
	case executionCrashAfterStarted, executionCrashBeforeTerminalCommit, executionCrashAfterTerminalCommit:
	default:
		executionCrashHelperFatal("invalid crash phase %q", phase)
	}
	sessionID, err := strconv.ParseInt(os.Getenv(executionCrashSessionEnv), 10, 64)
	if err != nil || sessionID <= 0 {
		executionCrashHelperFatal("invalid session id %q: %v", os.Getenv(executionCrashSessionEnv), err)
	}
	dbPath := os.Getenv(executionCrashDBEnv)
	workspaceID := os.Getenv(executionCrashWorkspaceEnv)
	targetPath := os.Getenv(executionCrashTargetEnv)
	leaseMarker := os.Getenv(executionCrashLeaseMarkerEnv)
	outputMarker := os.Getenv(executionCrashOutputMarkerEnv)
	if dbPath == "" || workspaceID == "" || targetPath == "" || leaseMarker == "" || outputMarker == "" {
		executionCrashHelperFatal("crash helper environment is incomplete")
	}

	store, err := db.OpenPath(dbPath)
	if err != nil {
		executionCrashHelperFatal("open SQLite store: %v", err)
	}
	// Persist the exact pre-turn recovery boundary in the child. The helper
	// must crash before any caller can advance this cursor.
	if err := store.SaveSessionState(context.Background(), sessionID, `{"version":1,"execution_cursor":0}`); err != nil {
		executionCrashHelperFatal("save cursor-zero session snapshot: %v", err)
	}
	lease, err := store.AcquireExecutionSessionLease(context.Background(), sessionID, workspaceID)
	if err != nil {
		executionCrashHelperFatal("acquire execution session lease: %v", err)
	}
	executionCrashHeldLease = lease
	if err := os.WriteFile(leaseMarker, []byte(string(phase)), 0o600); err != nil {
		executionCrashHelperFatal("write lease marker: %v", err)
	}

	client := &scriptedClient{responses: [][]llm.StreamChunk{
		{{ToolCalls: []llm.ToolCall{{
			ID:   "crash-write",
			Name: "write",
			Arguments: map[string]any{
				"path":    filepath.Base(targetPath),
				"content": "durable backend effect",
			},
		}}, Done: true}},
		{{Text: "unexpected continuation", Done: true}},
	}}
	ag := New(client, nil, 4096)
	ag.SetWorkDir(workspaceID)
	ag.SetModeContext("test", BuildToolPolicy())
	ag.SetPermissionChecker(permission.NewChecker(nil, true))
	ag.SetExecutionLedger(&storeCrashLedger{store: store, phase: phase})
	ag.SetExecutionSessionID(sessionID, "")
	ag.SetExecutionSnapshotCursor(0)
	ag.RequireExecutionLedger(true)
	ag.AddUserMessage("write the crash harness file")
	if err := ag.Run(context.Background(), &crashMarkerOutput{markerPath: outputMarker}); err != nil {
		executionCrashHelperFatal("Agent.Run returned before crash boundary: %v", err)
	}
	executionCrashHelperFatal("Agent.Run completed without reaching crash phase %q", phase)
}

func executionCrashHelperFatal(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func TestExecutionLedgerRecoversAcrossProcessCrash(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("execution session leases require POSIX advisory locks")
	}

	tests := []struct {
		name             string
		phase            executionCrashPhase
		wantLatest       executionpkg.EventType
		wantEventCount   int
		wantBackendFile  bool
		wantUnresolvedAt executionpkg.EventType
	}{
		{
			name:             "exit after durable started before backend",
			phase:            executionCrashAfterStarted,
			wantLatest:       executionpkg.EventStarted,
			wantEventCount:   3,
			wantBackendFile:  false,
			wantUnresolvedAt: executionpkg.EventStarted,
		},
		{
			name:             "exit after backend before terminal commit",
			phase:            executionCrashBeforeTerminalCommit,
			wantLatest:       executionpkg.EventStarted,
			wantEventCount:   3,
			wantBackendFile:  true,
			wantUnresolvedAt: executionpkg.EventStarted,
		},
		{
			name:             "exit after terminal commit before UI snapshot",
			phase:            executionCrashAfterTerminalCommit,
			wantLatest:       executionpkg.EventCompleted,
			wantEventCount:   4,
			wantBackendFile:  true,
			wantUnresolvedAt: executionpkg.EventCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "workspace")
			if err := os.Mkdir(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			scopeAgent := New(nil, nil, 4096)
			scopeAgent.SetWorkDir(workspace)
			workspaceID, err := scopeAgent.checkpointWorkspaceID()
			if err != nil {
				t.Fatal(err)
			}

			dbPath := filepath.Join(root, "execution.db")
			store, err := db.OpenPath(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.CreateSession(context.Background(), db.CreateSessionParams{
				Title:       "crash recovery",
				Model:       "test-model",
				Mode:        "BUILD",
				WorkspaceID: workspaceID,
			})
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			targetPath := filepath.Join(workspaceID, "crash-effect.txt")
			leaseMarker := filepath.Join(root, "lease-held")
			outputMarker := filepath.Join(root, "ui-result")
			runExecutionCrashHelper(t, tt.phase, dbPath, session.ID, workspaceID, targetPath, leaseMarker, outputMarker)

			marker, err := os.ReadFile(leaseMarker)
			if err != nil {
				t.Fatalf("child did not prove lease acquisition: %v", err)
			}
			if string(marker) != string(tt.phase) {
				t.Fatalf("lease marker = %q, want phase %q", marker, tt.phase)
			}
			if _, err := os.Stat(outputMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("tool result reached UI before crash: %v", err)
			}
			if _, err := os.Stat(targetPath); tt.wantBackendFile && err != nil {
				t.Fatalf("backend effect is absent after phase %q: %v", tt.phase, err)
			} else if !tt.wantBackendFile && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("backend effect exists before dispatch in phase %q: %v", tt.phase, err)
			}

			restarted, err := db.OpenPath(dbPath)
			if err != nil {
				t.Fatalf("reopen SQLite after process exit: %v", err)
			}
			t.Cleanup(func() { _ = restarted.Close() })
			lease, err := restarted.AcquireExecutionSessionLease(context.Background(), session.ID, workspaceID)
			if err != nil {
				t.Fatalf("kernel did not auto-release crashed child lease: %v", err)
			}
			t.Cleanup(func() { _ = lease.Close() })

			snapshot, err := restarted.GetSessionState(context.Background(), session.ID)
			if err != nil {
				t.Fatalf("read child session snapshot: %v", err)
			}
			if snapshot != `{"version":1,"execution_cursor":0}` {
				t.Fatalf("session snapshot = %s, want cursor zero", snapshot)
			}
			hazards, err := restarted.ListExecutionRecoveryHazards(context.Background(), session.ID, workspaceID, 0, 100)
			if err != nil {
				t.Fatalf("inspect restarted recovery hazards: %v", err)
			}
			if len(hazards) != 1 {
				t.Fatalf("recovery hazards = %d, want 1: %#v", len(hazards), hazards)
			}
			state := hazards[0]
			if state.Latest.Type != tt.wantLatest || state.EventCount != tt.wantEventCount {
				t.Fatalf("durable state = %s/%d events, want %s/%d", state.Latest.Type, state.EventCount, tt.wantLatest, tt.wantEventCount)
			}

			restartClient := &scriptedClient{responses: [][]llm.StreamChunk{{{Text: "provider must remain blocked", Done: true}}}}
			restartAgent := New(restartClient, nil, 4096)
			restartAgent.SetWorkDir(workspaceID)
			restartAgent.SetExecutionLedger(restarted)
			restartAgent.SetExecutionSessionID(session.ID, "")
			restartAgent.SetExecutionSnapshotCursor(0)
			restartAgent.RequireExecutionLedger(true)
			restartAgent.AddUserMessage("resume")
			err = restartAgent.Run(context.Background(), &outputRecorder{})
			var unresolved *UnresolvedExecutionError
			if !errors.As(err, &unresolved) {
				t.Fatalf("strict restart error = %v, want UnresolvedExecutionError", err)
			}
			if unresolved.EventType != tt.wantUnresolvedAt {
				t.Fatalf("unresolved event type = %q, want %q", unresolved.EventType, tt.wantUnresolvedAt)
			}
			if restartClient.calls != 0 {
				t.Fatalf("provider called before recovery reconciliation: %d", restartClient.calls)
			}
		})
	}
}

func runExecutionCrashHelper(t *testing.T, phase executionCrashPhase, dbPath string, sessionID int64, workspaceID, targetPath, leaseMarker, outputMarker string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecutionLedgerCrashHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		executionCrashHelperEnv+"=1",
		executionCrashPhaseEnv+"="+string(phase),
		executionCrashDBEnv+"="+dbPath,
		executionCrashSessionEnv+"="+strconv.FormatInt(sessionID, 10),
		executionCrashWorkspaceEnv+"="+workspaceID,
		executionCrashTargetEnv+"="+targetPath,
		executionCrashLeaseMarkerEnv+"="+leaseMarker,
		executionCrashOutputMarkerEnv+"="+outputMarker,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("crash helper timed out in phase %q: %v\n%s", phase, ctx.Err(), output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != executionCrashExitCode {
		t.Fatalf("crash helper phase %q exited with %v, want code %d\n%s", phase, err, executionCrashExitCode, output)
	}
}
