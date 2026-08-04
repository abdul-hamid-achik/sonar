package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// maxAutoCheckpoints bounds how many checkpoints are retained per session so
// pre-compaction snapshots don't grow without limit.
const maxAutoCheckpoints = 25

const checkpointSnapshotVersion = 1

// checkpointSnapshot is a bounded host-owned checkpoint envelope. The exact
// provider prompt receipt must rewind with the transcript it describes;
// persisting messages alone would let a pre-compaction restore erase the
// conservative context-admission floor.
type checkpointSnapshot struct {
	Version            int                `json:"version"`
	Messages           []llm.Message      `json:"messages"`
	ContextPromptFloor ContextPromptFloor `json:"context_prompt_floor,omitempty"`
}

// CheckpointStore is the subset of db.Store the agent needs to persist and
// restore conversation snapshots. Kept as an interface so the agent has no hard
// dependency on a concrete store (and tests can supply a fake).
type CheckpointStore interface {
	CreateCheckpointForWorkspace(ctx context.Context, sessionID int64, workspaceID, label, kind, messagesJSON string, msgCount int64) (int64, error)
	ListCheckpointsForWorkspace(ctx context.Context, sessionID int64, workspaceID string) ([]db.Checkpoint, error)
	GetCheckpoint(ctx context.Context, id int64) (db.Checkpoint, error)
	PruneCheckpoints(ctx context.Context, sessionID int64, keep int) error
	PruneCheckpointsByKindForWorkspace(ctx context.Context, sessionID int64, workspaceID, kind string, keep int) error
}

// SetCheckpointStore wires a checkpoint store and the session it belongs to.
// Without it, checkpointing is silently disabled.
func (a *Agent) SetCheckpointStore(cs CheckpointStore, sessionID int64) {
	a.checkpointStore = cs
	a.checkpointSessionID = sessionID
}

// SetCheckpointSessionID updates the session a checkpoint is associated with
// (e.g. once a session row is created mid-run).
func (a *Agent) SetCheckpointSessionID(sessionID int64) {
	a.mu.Lock()
	a.checkpointSessionID = sessionID
	a.mu.Unlock()
}

// snapshotMessagesJSON marshals the current history and its exact prompt-floor
// projection from one read-locked snapshot.
func (a *Agent) snapshotMessagesJSON() (string, int, error) {
	a.mu.RLock()
	msgs := make([]llm.Message, len(a.messages))
	copy(msgs, a.messages)
	floor := a.contextPromptFloor
	a.mu.RUnlock()
	msgs = SanitizeMessagesForPersistence(msgs)
	if err := floor.Validate(); err != nil {
		return "", 0, fmt.Errorf("validate context prompt floor: %w", err)
	}

	data, err := json.Marshal(checkpointSnapshot{
		Version:            checkpointSnapshotVersion,
		Messages:           msgs,
		ContextPromptFloor: floor,
	})
	if err != nil {
		return "", 0, fmt.Errorf("marshal checkpoint snapshot: %w", err)
	}
	return string(data), len(msgs), nil
}

func decodeCheckpointSnapshot(raw string) (checkpointSnapshot, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return checkpointSnapshot{}, false, fmt.Errorf("empty checkpoint snapshot")
	}
	if strings.HasPrefix(trimmed, "[") {
		// Version 0 checkpoints stored only the message array. They remain
		// readable, but restore retains the live exact floor conservatively.
		var messages []llm.Message
		if err := decodeCheckpointJSON(trimmed, &messages); err != nil {
			return checkpointSnapshot{}, true, err
		}
		return checkpointSnapshot{Messages: messages}, true, nil
	}
	if !strings.HasPrefix(trimmed, "{") {
		return checkpointSnapshot{}, false, fmt.Errorf("unsupported checkpoint snapshot encoding")
	}
	var snapshot checkpointSnapshot
	if err := decodeCheckpointJSON(trimmed, &snapshot); err != nil {
		return checkpointSnapshot{}, false, err
	}
	if snapshot.Version != checkpointSnapshotVersion {
		return checkpointSnapshot{}, false, fmt.Errorf("unsupported checkpoint snapshot version %d", snapshot.Version)
	}
	if err := snapshot.ContextPromptFloor.Validate(); err != nil {
		return checkpointSnapshot{}, false, fmt.Errorf("invalid context prompt floor: %w", err)
	}
	return snapshot, false, nil
}

func decodeCheckpointJSON(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("checkpoint snapshot contains multiple JSON values")
		}
		return err
	}
	return nil
}

func (a *Agent) restoreCheckpointSnapshot(snapshot checkpointSnapshot, legacy bool) (int, error) {
	floor := snapshot.ContextPromptFloor
	if legacy {
		// A legacy snapshot cannot prove which provider receipt accompanied its
		// transcript. Refuse to replace a live exact boundary with an unrelated
		// message prefix; legacy snapshots remain readable when no such receipt
		// exists in the active session.
		if liveFloor := a.ContextPromptFloor(); liveFloor.Tokens > 0 {
			return 0, fmt.Errorf("legacy checkpoint lacks the active context receipt; start a fresh session to restore it safely")
		}
		floor = ContextPromptFloor{}
	}
	if err := floor.Validate(); err != nil {
		return 0, err
	}
	currentModel := ""
	if a.llmClient != nil {
		currentModel = a.llmClient.Model()
	}
	if floor.Tokens > 0 && config.CanonicalModelName(floor.Model) != config.CanonicalModelName(currentModel) {
		return 0, fmt.Errorf("checkpoint context receipt belongs to model %q, current model is %q; switch back before restoring", floor.Model, currentModel)
	}
	messages := SanitizeMessagesForPersistence(snapshot.Messages)
	messages = cloneMessagesWithImages(messages)
	messages = restoreConversationSummaryOwnership(messages)

	// Restore the transcript and the receipt that describes it under one lock.
	// No observer can see the rewound messages with a temporarily cleared floor.
	a.mu.Lock()
	a.messages = messages
	a.contextPromptFloor = floor
	a.continuationHistory = newContinuationTurnState(0)
	a.resetAutoContinuationHistoryLocked()
	a.invalidateBobWorkspaceContextLocked()
	a.mu.Unlock()
	return len(messages), nil
}

func (a *Agent) checkpointWorkspaceID() (string, error) {
	workDir := a.WorkDir()
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve checkpoint workspace: %w", err)
		}
	}
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve checkpoint workspace: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

// CreateCheckpoint snapshots the current conversation. kind is typically
// db.CheckpointManual or db.CheckpointPreCompaction. Returns the new id, or 0
// if no checkpoint store is configured.
func (a *Agent) CreateCheckpoint(ctx context.Context, label, kind string) (int64, error) {
	if a.checkpointStore == nil {
		return 0, nil
	}
	msgsJSON, count, err := a.snapshotMessagesJSON()
	if err != nil {
		return 0, err
	}

	a.mu.RLock()
	sid := a.checkpointSessionID
	a.mu.RUnlock()
	workspaceID, err := a.checkpointWorkspaceID()
	if err != nil {
		return 0, err
	}

	id, err := a.checkpointStore.CreateCheckpointForWorkspace(ctx, sid, workspaceID, label, kind, msgsJSON, int64(count))
	if err != nil {
		return 0, err
	}
	// Best-effort prune only automatic snapshots. Manual checkpoints are an
	// explicit user artifact and must not disappear behind their back.
	if kind == db.CheckpointPreCompaction {
		_ = a.checkpointStore.PruneCheckpointsByKindForWorkspace(ctx, sid, workspaceID, kind, maxAutoCheckpoints)
	}
	if a.logger != nil {
		a.logger.Info("checkpoint created", "id", id, "kind", kind, "messages", count)
	}
	return id, nil
}

// ListCheckpoints returns the checkpoints for the current session, newest first.
func (a *Agent) ListCheckpoints(ctx context.Context) ([]db.Checkpoint, error) {
	if a.checkpointStore == nil {
		return nil, nil
	}
	a.mu.RLock()
	sid := a.checkpointSessionID
	a.mu.RUnlock()
	workspaceID, err := a.checkpointWorkspaceID()
	if err != nil {
		return nil, err
	}
	return a.checkpointStore.ListCheckpointsForWorkspace(ctx, sid, workspaceID)
}

// RestoreCheckpoint replaces the live conversation with a stored snapshot and
// returns the number of messages restored.
func (a *Agent) RestoreCheckpoint(ctx context.Context, id int64) (int, error) {
	if a.checkpointStore == nil {
		return 0, fmt.Errorf("checkpoints are not enabled")
	}
	cp, err := a.checkpointStore.GetCheckpoint(ctx, id)
	if err != nil {
		return 0, err
	}
	a.mu.RLock()
	sid := a.checkpointSessionID
	a.mu.RUnlock()
	workspaceID, err := a.checkpointWorkspaceID()
	if err != nil {
		return 0, err
	}
	if cp.WorkspaceID != workspaceID {
		return 0, fmt.Errorf("checkpoint %d belongs to a different workspace", id)
	}
	if cp.SessionID != sid {
		return 0, fmt.Errorf("checkpoint %d belongs to session %d, not active session %d", id, cp.SessionID, sid)
	}
	snapshot, legacy, err := decodeCheckpointSnapshot(cp.Messages)
	if err != nil {
		return 0, fmt.Errorf("decode checkpoint %d: %w", id, err)
	}
	count, err := a.restoreCheckpointSnapshot(snapshot, legacy)
	if err != nil {
		return 0, fmt.Errorf("restore checkpoint %d: %w", id, err)
	}
	if a.logger != nil {
		a.logger.Info("checkpoint restored", "id", id, "messages", count)
	}
	return count, nil
}
