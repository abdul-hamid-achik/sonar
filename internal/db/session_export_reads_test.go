package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestGetSessionStateForExportEnforcesByteBound(t *testing.T) {
	store := testStore(t)
	session := createSessionExportReadTestSession(t, store, "/workspace/export-state")
	raw := `{"version":2,"goal":null,"padding":"` + strings.Repeat("界", 32) + `"}`
	if err := store.SaveSessionState(context.Background(), session.ID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSessionStateForExport(context.Background(), session.ID, len(raw)-1); !errors.Is(err, ErrSessionExportStateTooLarge) {
		t.Fatalf("oversize state error = %v", err)
	}
	got, err := store.GetSessionStateForExport(context.Background(), session.ID, len(raw))
	if err != nil || got != raw {
		t.Fatalf("bounded state = %q, err=%v", got, err)
	}
	if _, err := store.GetSessionStateForExport(context.Background(), session.ID, 0); err == nil {
		t.Fatal("zero byte limit was accepted")
	}
	missing := createSessionExportReadTestSession(t, store, "/workspace/export-state-missing")
	if _, err := store.GetSessionStateForExport(context.Background(), missing.ID, 64); !errors.Is(err, ErrSessionStateNotFound) {
		t.Fatalf("missing state error = %v", err)
	}
}

func TestListRecentSessionTokenStatsReturnsBoundedChronologicalTail(t *testing.T) {
	store := testStore(t)
	session := createSessionExportReadTestSession(t, store, "/workspace/export-token-stats")
	for turn := int64(1); turn <= 5; turn++ {
		if _, err := store.RecordTokenUsage(context.Background(), RecordTokenUsageParams{
			SessionID: session.ID, Turn: turn, EvalCount: turn * 10,
			PromptTokens: turn * 20, Model: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}

	stats, err := store.ListRecentSessionTokenStats(context.Background(), session.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 3 {
		t.Fatalf("recent token stats count = %d, want 3", len(stats))
	}
	for index, wantTurn := range []int64{3, 4, 5} {
		if stats[index].Turn != wantTurn || stats[index].SessionID != session.ID {
			t.Fatalf("recent token stats[%d] = %#v, want turn %d for session %d", index, stats[index], wantTurn, session.ID)
		}
		if index > 0 && stats[index-1].ID >= stats[index].ID {
			t.Fatalf("recent token stats are not chronological: %#v", stats)
		}
	}
}

func TestListRecentSessionFileChangesReturnsBoundedChronologicalTail(t *testing.T) {
	store := testStore(t)
	session := createSessionExportReadTestSession(t, store, "/workspace/export-file-changes")
	for index := 1; index <= 5; index++ {
		if _, err := store.RecordFileChange(context.Background(), RecordFileChangeParams{
			SessionID: session.ID, FilePath: fmt.Sprintf("file-%d.go", index),
			ToolName: "write", Added: int64(index),
		}); err != nil {
			t.Fatal(err)
		}
	}

	changes, err := store.ListRecentSessionFileChanges(context.Background(), session.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("recent file changes count = %d, want 3", len(changes))
	}
	for index, wantPath := range []string{"file-3.go", "file-4.go", "file-5.go"} {
		if changes[index].FilePath != wantPath || changes[index].SessionID != session.ID {
			t.Fatalf("recent file changes[%d] = %#v, want %q for session %d", index, changes[index], wantPath, session.ID)
		}
		if index > 0 && changes[index-1].ID >= changes[index].ID {
			t.Fatalf("recent file changes are not chronological: %#v", changes)
		}
	}
}

func TestListRecentSessionCheckpointsReturnsNewestFirstBound(t *testing.T) {
	store := testStore(t)
	workspace := "/workspace/export-checkpoints"
	session := createSessionExportReadTestSession(t, store, workspace)
	for index := 1; index <= 5; index++ {
		if _, err := store.CreateCheckpointForWorkspace(
			context.Background(), session.ID, workspace, fmt.Sprintf("checkpoint-%d", index),
			CheckpointManual, `[{}]`, int64(index),
		); err != nil {
			t.Fatal(err)
		}
	}

	checkpoints, err := store.ListRecentSessionCheckpoints(context.Background(), session.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 3 {
		t.Fatalf("recent checkpoints count = %d, want 3", len(checkpoints))
	}
	for index, wantLabel := range []string{"checkpoint-5", "checkpoint-4", "checkpoint-3"} {
		if checkpoints[index].Label != wantLabel || checkpoints[index].SessionID != session.ID || checkpoints[index].Messages != "" {
			t.Fatalf("recent checkpoints[%d] = %#v, want metadata for %q", index, checkpoints[index], wantLabel)
		}
		if index > 0 && checkpoints[index-1].ID <= checkpoints[index].ID {
			t.Fatalf("recent checkpoints are not newest first: %#v", checkpoints)
		}
	}
}

func TestSessionExportReadLimitsAreValidated(t *testing.T) {
	store := testStore(t)
	session := createSessionExportReadTestSession(t, store, "/workspace/export-read-limits")
	readers := map[string]func(int) error{
		"token stats": func(limit int) error {
			_, err := store.ListRecentSessionTokenStats(context.Background(), session.ID, limit)
			return err
		},
		"file changes": func(limit int) error {
			_, err := store.ListRecentSessionFileChanges(context.Background(), session.ID, limit)
			return err
		},
		"checkpoints": func(limit int) error {
			_, err := store.ListRecentSessionCheckpoints(context.Background(), session.ID, limit)
			return err
		},
	}
	for name, read := range readers {
		for _, limit := range []int{-1, 0, MaxSessionExportReadLimit + 1} {
			if err := read(limit); err == nil {
				t.Errorf("%s accepted invalid limit %d", name, limit)
			}
		}
		if err := read(MaxSessionExportReadLimit); err != nil {
			t.Errorf("%s rejected maximum limit: %v", name, err)
		}
	}
}

func createSessionExportReadTestSession(t *testing.T, store *Store, workspace string) Session {
	t.Helper()
	session, err := store.CreateSession(context.Background(), CreateSessionParams{
		Title: "bounded export reads", Model: "test", Mode: "AUTO", WorkspaceID: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}
