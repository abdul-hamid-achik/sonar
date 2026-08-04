package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

// SessionRecoveryState is the validated subset of a durable session envelope
// that controls execution-hazard projection and recovery authority.
type SessionRecoveryState struct {
	Version         int
	GoalOwned       bool
	ExecutionCursor int64
}

// InspectSessionRecoveryState validates the authority-bearing session fields
// used by read-only audit and recovery projections. Early v1 states predate
// execution cursors, but later v1 writers persisted them before the envelope
// version advanced; a missing cursor therefore projects as zero while a
// present nonnegative cursor remains authoritative.
func InspectSessionRecoveryState(sessionID int64, stateJSON string) (SessionRecoveryState, error) {
	if sessionID <= 0 {
		return SessionRecoveryState{}, fmt.Errorf("invalid session id %d", sessionID)
	}
	if !utf8.ValidString(stateJSON) || !json.Valid([]byte(stateJSON)) {
		return SessionRecoveryState{}, errors.New("session state is not valid UTF-8 JSON")
	}
	if err := validateUniqueTopLevelJSONKeys([]byte(stateJSON)); err != nil {
		return SessionRecoveryState{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stateJSON), &fields); err != nil {
		return SessionRecoveryState{}, fmt.Errorf("decode session envelope: %w", err)
	}
	var version int
	versionRaw, present := fields["version"]
	if !present {
		return SessionRecoveryState{}, errors.New("session envelope has no version")
	}
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return SessionRecoveryState{}, fmt.Errorf("decode session envelope version: %w", err)
	}
	if version != 1 && !supportedReconciliationEnvelopeVersion(version) {
		return SessionRecoveryState{}, fmt.Errorf("unsupported session envelope version %d", version)
	}

	view := SessionRecoveryState{Version: version}
	goalRaw := bytes.TrimSpace(fields["goal"])
	if len(goalRaw) > 0 && !bytes.Equal(goalRaw, []byte("null")) {
		var snapshot goal.Snapshot
		if err := json.Unmarshal(goalRaw, &snapshot); err != nil {
			return SessionRecoveryState{}, fmt.Errorf("decode durable goal: %w", err)
		}
		if snapshot.SessionID != sessionID {
			return SessionRecoveryState{}, fmt.Errorf("durable goal session %d does not match state session %d", snapshot.SessionID, sessionID)
		}
		if _, err := goal.Restore(snapshot); err != nil {
			return SessionRecoveryState{}, fmt.Errorf("validate durable goal: %w", err)
		}
		view.GoalOwned = true
	}

	cursorRaw, hasCursor := fields["execution_cursor"]
	if hasCursor {
		if err := json.Unmarshal(cursorRaw, &view.ExecutionCursor); err != nil {
			return SessionRecoveryState{}, fmt.Errorf("decode session execution cursor: %w", err)
		}
	}
	if view.ExecutionCursor < 0 {
		return SessionRecoveryState{}, fmt.Errorf("session execution cursor is negative: %d", view.ExecutionCursor)
	}
	return view, nil
}
