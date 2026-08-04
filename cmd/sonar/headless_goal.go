package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type headlessGoalState struct {
	Version            int                      `json:"version"`
	Model              string                   `json:"model"`
	Messages           []llm.Message            `json:"messages"`
	ContextPromptFloor agent.ContextPromptFloor `json:"context_prompt_floor"`
	ExecutionCursor    int64                    `json:"execution_cursor"`
	Goal               *goal.Snapshot           `json:"goal"`
}

func boundedHeadlessGoalError(err error) string {
	const prefix = "headless goal turn failed: "
	if err == nil {
		return "headless goal turn failed"
	}
	value := prefix + strings.TrimSpace(err.Error())
	if len(value) <= goal.MaxReasonBytes {
		return value
	}
	cut := goal.MaxReasonBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func loadHeadlessGoalState(ctx context.Context, store *db.Store, workspace string, sessionID int64) (db.Session, *goal.Runtime, headlessGoalState, db.SessionStateRecord, error) {
	session, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, err
	}
	if workspace == "" || session.WorkspaceID != workspace {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, fmt.Errorf("session %d belongs to a different workspace", sessionID)
	}
	record, err := store.GetSessionStateRecord(ctx, sessionID)
	if err != nil {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, err
	}
	var state headlessGoalState
	if err := json.Unmarshal([]byte(record.StateJSON), &state); err != nil {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, fmt.Errorf("decode goal session state: %w", err)
	}
	if state.Version < 1 || state.Version > 3 {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, fmt.Errorf("session %d has unsupported state version %d", sessionID, state.Version)
	}
	if state.ExecutionCursor < 0 || state.Goal == nil || state.Goal.SessionID != sessionID {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, fmt.Errorf("session %d does not contain a valid bound goal snapshot", sessionID)
	}
	if err := state.ContextPromptFloor.Validate(); err != nil {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, fmt.Errorf("session %d has invalid context prompt floor: %w", sessionID, err)
	}
	if state.ContextPromptFloor.Tokens > 0 && config.CanonicalModelName(state.ContextPromptFloor.Model) != config.CanonicalModelName(state.Model) {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, fmt.Errorf("session %d context prompt floor model does not match saved model", sessionID)
	}
	runtime, err := goal.Restore(*state.Goal)
	if err != nil {
		return db.Session{}, nil, headlessGoalState{}, db.SessionStateRecord{}, fmt.Errorf("restore goal runtime: %w", err)
	}
	state.Messages = agent.SanitizeMessagesForPersistence(state.Messages)
	return session, runtime, state, record, nil
}
