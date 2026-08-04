package db

import (
	"context"
	"errors"
	"testing"
)

func TestCheckpointCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.CreateCheckpoint(ctx, 0, "first", CheckpointManual, `[{"role":"user","content":"hi"}]`, 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	got, err := s.GetCheckpoint(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "first" || got.Kind != CheckpointManual || got.MsgCount != 1 {
		t.Fatalf("unexpected checkpoint: %+v", got)
	}
	if got.Messages != `[{"role":"user","content":"hi"}]` {
		t.Fatalf("messages payload mismatch: %q", got.Messages)
	}

	list, err := s.ListCheckpoints(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 checkpoint, got %d", len(list))
	}
	// Listing omits the payload to stay cheap.
	if list[0].Messages != "" {
		t.Fatalf("expected empty payload in listing, got %q", list[0].Messages)
	}

	if err := s.DeleteCheckpoint(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetCheckpoint(ctx, id); !errors.Is(err, ErrCheckpointNotFound) {
		t.Fatalf("expected ErrCheckpointNotFound, got %v", err)
	}
}

func TestCheckpointDefaults(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateCheckpoint(ctx, 0, "", "", "", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetCheckpoint(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != CheckpointManual {
		t.Errorf("empty kind should default to manual, got %q", got.Kind)
	}
	if got.Messages != "[]" {
		t.Errorf("empty messages should default to [], got %q", got.Messages)
	}
}

func TestCheckpointWorkspaceFiltering(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, err := s.CreateCheckpointForWorkspace(ctx, 0, "/workspace/a", "a", CheckpointManual, "[]", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCheckpointForWorkspace(ctx, 0, "/workspace/b", "b", CheckpointManual, "[]", 0); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := s.ListCheckpointsForWorkspace(ctx, 0, "/workspace/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 || checkpoints[0].ID != id || checkpoints[0].WorkspaceID != "/workspace/a" {
		t.Fatalf("workspace checkpoint list leaked: %#v", checkpoints)
	}
}

func TestPruneCheckpoints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.CreateCheckpoint(ctx, 7, "cp", CheckpointPreCompaction, "[]", 0); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	// A different session's checkpoints must be untouched by pruning session 7.
	otherID, _ := s.CreateCheckpoint(ctx, 99, "other", CheckpointManual, "[]", 0)

	if err := s.PruneCheckpoints(ctx, 7, 2); err != nil {
		t.Fatalf("prune: %v", err)
	}

	list, err := s.ListCheckpoints(ctx, 7)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 after prune, got %d", len(list))
	}
	// Most recent kept (DESC order).
	if list[0].ID < list[1].ID {
		t.Fatal("expected newest-first ordering")
	}
	if _, err := s.GetCheckpoint(ctx, otherID); err != nil {
		t.Fatalf("other session's checkpoint should survive: %v", err)
	}

	// keep <= 0 is a no-op.
	if err := s.PruneCheckpoints(ctx, 7, 0); err != nil {
		t.Fatalf("prune 0: %v", err)
	}
	if list, _ := s.ListCheckpoints(ctx, 7); len(list) != 2 {
		t.Fatalf("prune(0) should be a no-op, got %d", len(list))
	}
}

func TestPruneCheckpointsByKindPreservesManualSnapshots(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	manualID, err := s.CreateCheckpoint(ctx, 7, "manual", CheckpointManual, "[]", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.CreateCheckpoint(ctx, 7, "auto", CheckpointPreCompaction, "[]", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PruneCheckpointsByKind(ctx, 7, CheckpointPreCompaction, 1); err != nil {
		t.Fatal(err)
	}
	checkpoints, err := s.ListCheckpoints(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoints after kind prune = %#v", checkpoints)
	}
	foundManual := false
	for _, checkpoint := range checkpoints {
		foundManual = foundManual || checkpoint.ID == manualID
	}
	if !foundManual {
		t.Fatal("manual checkpoint was pruned")
	}
}

func TestClaimLegacyCheckpointsRequiresBoundActiveSession(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sessionA, err := store.CreateSession(ctx, CreateSessionParams{
		Title: "A", Model: "qwen3.5:2b", Mode: "BUILD", WorkspaceID: "/workspace/a",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := store.CreateSession(ctx, CreateSessionParams{
		Title: "B", Model: "qwen3.5:2b", Mode: "BUILD", WorkspaceID: "/workspace/b",
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyA, err := store.CreateCheckpoint(ctx, sessionA.ID, "legacy-a", CheckpointManual, "[]", 0)
	if err != nil {
		t.Fatal(err)
	}
	legacyB, err := store.CreateCheckpoint(ctx, sessionB.ID, "legacy-b", CheckpointManual, "[]", 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.ClaimLegacyCheckpointsForActiveSession(ctx, sessionA.ID, "/workspace/b"); err == nil {
		t.Fatal("different workspace claimed session A checkpoints")
	}
	before, err := store.GetCheckpoint(ctx, legacyA)
	if err != nil {
		t.Fatal(err)
	}
	if before.WorkspaceID != "" {
		t.Fatal("rejected claim mutated checkpoint")
	}

	claimed, err := store.ClaimLegacyCheckpointsForActiveSession(ctx, sessionA.ID, "/workspace/a")
	if err != nil {
		t.Fatal(err)
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	checkpointA, err := store.GetCheckpoint(ctx, legacyA)
	if err != nil {
		t.Fatal(err)
	}
	checkpointB, err := store.GetCheckpoint(ctx, legacyB)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointA.WorkspaceID != "/workspace/a" || checkpointB.WorkspaceID != "" {
		t.Fatalf("claim crossed session boundary: A=%#v B=%#v", checkpointA, checkpointB)
	}

	claimed, err = store.ClaimLegacyCheckpointsForActiveSession(ctx, sessionA.ID, "/workspace/a")
	if err != nil || claimed != 0 {
		t.Fatalf("repeat claim = %d, %v", claimed, err)
	}
	if _, err := store.ClaimLegacyCheckpointsForActiveSession(ctx, 0, "/workspace/a"); err == nil {
		t.Fatal("unbound session zero was allowed to claim checkpoints")
	}
	if _, err := store.ClaimLegacyCheckpointsForActiveSession(ctx, 9999, "/workspace/a"); err == nil {
		t.Fatal("missing active session was allowed to claim checkpoints")
	}
}

func TestClaimUnboundLegacyCheckpointsRequiresPreviewAndCreatesReceipt(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sessionA, err := store.CreateSession(ctx, CreateSessionParams{
		Title: "A", Model: "qwen3.5:2b", Mode: "BUILD", WorkspaceID: "/workspace/a",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := store.CreateSession(ctx, CreateSessionParams{
		Title: "B", Model: "qwen3.5:2b", Mode: "BUILD", WorkspaceID: "/workspace/b",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateCheckpoint(ctx, 0, "legacy-one", CheckpointManual, "[]", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateCheckpoint(ctx, 0, "legacy-two", CheckpointManual, "[]", 0)
	if err != nil {
		t.Fatal(err)
	}

	preview, err := store.CountUnboundLegacyCheckpoints(ctx)
	if err != nil || preview != 2 {
		t.Fatalf("preview = %d, %v", preview, err)
	}
	if _, err := store.ClaimUnboundLegacyCheckpointsForActiveSession(ctx, sessionA.ID, "/workspace/a", preview-1); err == nil {
		t.Fatal("stale/incorrect preview count was accepted")
	}
	if count, err := store.CountUnboundLegacyCheckpoints(ctx); err != nil || count != 2 {
		t.Fatalf("failed claim mutated unbound set: %d, %v", count, err)
	}

	receipt, err := store.ClaimUnboundLegacyCheckpointsForActiveSession(ctx, sessionA.ID, "/workspace/a", preview)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Claimed != 2 || receipt.AlreadyClaimed || receipt.SessionID != sessionA.ID || receipt.WorkspaceID != "/workspace/a" {
		t.Fatalf("claim receipt = %#v", receipt)
	}
	for _, id := range []int64{first, second} {
		checkpoint, err := store.GetCheckpoint(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint.SessionID != sessionA.ID || checkpoint.WorkspaceID != "/workspace/a" {
			t.Fatalf("checkpoint %d not rebound: %#v", id, checkpoint)
		}
	}
	if count, err := store.CountUnboundLegacyCheckpoints(ctx); err != nil || count != 0 {
		t.Fatalf("unbound count after claim = %d, %v", count, err)
	}

	repeat, err := store.ClaimUnboundLegacyCheckpointsForActiveSession(ctx, sessionA.ID, "/workspace/a", 0)
	if err != nil || !repeat.AlreadyClaimed || repeat.Claimed != 2 {
		t.Fatalf("repeat receipt = %#v, %v", repeat, err)
	}
	if _, err := store.ClaimUnboundLegacyCheckpointsForActiveSession(ctx, sessionB.ID, "/workspace/b", 0); !errors.Is(err, ErrUnboundLegacyCheckpointsAlreadyClaimed) {
		t.Fatalf("second-workspace claim error = %v", err)
	}

	if _, err := store.CreateCheckpoint(ctx, 0, "late legacy", CheckpointManual, "[]", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimUnboundLegacyCheckpointsForActiveSession(ctx, sessionA.ID, "/workspace/a", 1); err == nil {
		t.Fatal("new unbound data was silently folded into the completed one-time claim")
	}
}
