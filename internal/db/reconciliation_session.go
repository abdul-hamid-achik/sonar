package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/reconciliation"
)

const (
	persistedSessionEnvelopeVersion     = 3
	oldestReconciliationEnvelopeVersion = 2
)

func supportedReconciliationEnvelopeVersion(version int) bool {
	return version >= oldestReconciliationEnvelopeVersion &&
		version <= persistedSessionEnvelopeVersion
}

type reconciliationSession struct {
	record          SessionStateRecord
	goal            goal.Snapshot
	goalSHA256      string
	executionCursor int64
}

func decodeReconciliationSession(record SessionStateRecord) (reconciliationSession, error) {
	if record.SessionID <= 0 || record.Revision < 0 {
		return reconciliationSession{}, errors.New("invalid revisioned reconciliation session state")
	}
	if !utf8.ValidString(record.StateJSON) || !json.Valid([]byte(record.StateJSON)) {
		return reconciliationSession{}, errors.New("reconciliation session state is not valid UTF-8 JSON")
	}
	if err := validateUniqueTopLevelJSONKeys([]byte(record.StateJSON)); err != nil {
		return reconciliationSession{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(record.StateJSON), &fields); err != nil {
		return reconciliationSession{}, fmt.Errorf("decode reconciliation session envelope: %w", err)
	}
	var version int
	// An explicit null is an absent version, not a malformed number: it decodes
	// without error and leaves the zero value behind, which would otherwise be
	// reported as "unsupported version 0".
	raw := bytes.TrimSpace(fields["version"])
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return reconciliationSession{}, errors.New("reconciliation session envelope has no version")
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return reconciliationSession{}, fmt.Errorf("decode reconciliation session envelope version: %w", err)
	}
	if !supportedReconciliationEnvelopeVersion(version) {
		return reconciliationSession{}, fmt.Errorf("unsupported reconciliation session envelope version %d", version)
	}
	goalRaw := fields["goal"]
	if len(goalRaw) == 0 || bytes.Equal(bytes.TrimSpace(goalRaw), []byte("null")) {
		return reconciliationSession{}, errors.New("reconciliation session has no durable goal")
	}
	var snapshot goal.Snapshot
	if err := json.Unmarshal(goalRaw, &snapshot); err != nil {
		return reconciliationSession{}, fmt.Errorf("decode reconciliation goal: %w", err)
	}
	if snapshot.SessionID != record.SessionID {
		return reconciliationSession{}, fmt.Errorf("reconciliation goal session %d does not match state session %d", snapshot.SessionID, record.SessionID)
	}
	if _, err := goal.Restore(snapshot); err != nil {
		return reconciliationSession{}, fmt.Errorf("validate reconciliation goal: %w", err)
	}
	canonicalGoal, err := json.Marshal(snapshot)
	if err != nil {
		return reconciliationSession{}, fmt.Errorf("canonicalize reconciliation goal: %w", err)
	}
	cursor := int64(0)
	if raw := fields["execution_cursor"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &cursor); err != nil {
			return reconciliationSession{}, fmt.Errorf("decode reconciliation execution cursor: %w", err)
		}
	}
	if cursor < 0 {
		return reconciliationSession{}, fmt.Errorf("reconciliation execution cursor is negative: %d", cursor)
	}
	return reconciliationSession{
		record: record, goal: snapshot, goalSHA256: reconciliation.Hash(string(canonicalGoal)),
		executionCursor: cursor,
	}, nil
}

func outcomeUnknownTurn(snapshot goal.Snapshot) (string, error) {
	if snapshot.State != goal.StateBlocked || snapshot.Blocker == nil || snapshot.Blocker.Kind != goal.BlockOutcomeUnknown {
		return "", goal.ErrOutcomeUnknown
	}
	if snapshot.PendingContinuation != nil {
		return "", fmt.Errorf("%w: outcome reconciliation cannot clear a pending turn admission", goal.ErrTurnPending)
	}
	reference := snapshot.Blocker.Reference
	turnID := ""
	bind := func(candidate, source string) error {
		if candidate == "" {
			return nil
		}
		if turnID != "" && turnID != candidate {
			return fmt.Errorf("outcome reconciliation correlations disagree: %q versus %q (%s)", turnID, candidate, source)
		}
		turnID = candidate
		return nil
	}
	if recovery := snapshot.LastPendingRecovery; recovery != nil &&
		recovery.Recovery.Kind == goal.PendingOutcomeUnknown && recovery.Recovery.OutcomeRef == reference {
		if err := bind(recovery.Permit.TurnID, "pending recovery"); err != nil {
			return "", err
		}
	}
	if turn := snapshot.LastTurn; turn != nil && turn.OutcomeUnknown && turn.OutcomeRef == reference {
		if err := bind(turn.TurnID, "turn receipt"); err != nil {
			return "", err
		}
	}
	if turnID == "" {
		return "", errors.New("outcome-unknown blocker has no exact durable turn correlation")
	}
	return turnID, nil
}

func patchReconciliationSession(raw string, nextGoal goal.Snapshot, nextCursor int64) (string, error) {
	if nextCursor < 0 {
		return "", errors.New("reconciliation cursor must not be negative")
	}
	goalJSON, err := json.Marshal(nextGoal)
	if err != nil {
		return "", fmt.Errorf("encode reconciled goal: %w", err)
	}
	return patchTopLevelJSONObject([]byte(raw), map[string][]byte{
		"goal":             goalJSON,
		"execution_cursor": []byte(strconv.FormatInt(nextCursor, 10)),
	})
}

// patchTopLevelJSONObject replaces only selected top-level JSON values. Every
// unknown field, key order, and byte outside those value spans is retained.
func patchTopLevelJSONObject(raw []byte, replacements map[string][]byte) (string, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return "", errors.New("session envelope is not valid UTF-8 JSON")
	}
	i := skipJSONSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return "", errors.New("session envelope must be a JSON object")
	}
	i++
	last := 0
	found := make(map[string]bool, len(replacements))
	seen := make(map[string]struct{})
	var output bytes.Buffer
	memberCount := 0
	for {
		i = skipJSONSpace(raw, i)
		if i >= len(raw) {
			return "", errors.New("unterminated session envelope")
		}
		if raw[i] == '}' {
			break
		}
		keyEnd, err := scanJSONString(raw, i)
		if err != nil {
			return "", err
		}
		var key string
		if err := json.Unmarshal(raw[i:keyEnd], &key); err != nil {
			return "", fmt.Errorf("decode session envelope key: %w", err)
		}
		if _, duplicate := seen[key]; duplicate {
			return "", fmt.Errorf("session envelope contains duplicate top-level key %q", key)
		}
		seen[key] = struct{}{}
		i = skipJSONSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return "", errors.New("session envelope key has no value")
		}
		valueStart := skipJSONSpace(raw, i+1)
		valueEnd, err := scanJSONValue(raw, valueStart)
		if err != nil {
			return "", err
		}
		if replacement, ok := replacements[key]; ok {
			output.Write(raw[last:valueStart])
			output.Write(replacement)
			last = valueEnd
			found[key] = true
		}
		i = skipJSONSpace(raw, valueEnd)
		memberCount++
		if i >= len(raw) {
			return "", errors.New("unterminated session envelope")
		}
		switch raw[i] {
		case ',':
			i++
		case '}':
			continue
		default:
			return "", errors.New("invalid session envelope member separator")
		}
	}
	objectEnd := i
	output.Write(raw[last:objectEnd])
	missing := make([]string, 0, len(replacements))
	for key := range replacements {
		if found[key] {
			continue
		}
		missing = append(missing, key)
	}
	sort.Strings(missing)
	for _, key := range missing {
		replacement := replacements[key]
		if memberCount > 0 {
			output.WriteByte(',')
		}
		keyJSON, _ := json.Marshal(key)
		output.Write(keyJSON)
		output.WriteByte(':')
		output.Write(replacement)
		memberCount++
	}
	output.Write(raw[objectEnd:])
	patched := output.String()
	if !json.Valid([]byte(patched)) {
		return "", errors.New("patched reconciliation session is invalid JSON")
	}
	return patched, nil
}

func validateUniqueTopLevelJSONKeys(raw []byte) error {
	_, err := patchTopLevelJSONObject(raw, nil)
	return err
}

func skipJSONSpace(raw []byte, index int) int {
	for index < len(raw) {
		switch raw[index] {
		case ' ', '\t', '\n', '\r':
			index++
		default:
			return index
		}
	}
	return index
}

func scanJSONString(raw []byte, start int) (int, error) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, errors.New("expected JSON string")
	}
	escaped := false
	for index := start + 1; index < len(raw); index++ {
		if escaped {
			escaped = false
			continue
		}
		switch raw[index] {
		case '\\':
			escaped = true
		case '"':
			return index + 1, nil
		}
	}
	return 0, errors.New("unterminated JSON string")
}

func scanJSONValue(raw []byte, start int) (int, error) {
	if start >= len(raw) {
		return 0, errors.New("missing JSON value")
	}
	if raw[start] == '"' {
		return scanJSONString(raw, start)
	}
	if raw[start] == '{' || raw[start] == '[' {
		stack := []byte{raw[start]}
		inString, escaped := false, false
		for index := start + 1; index < len(raw); index++ {
			char := raw[index]
			if inString {
				if escaped {
					escaped = false
				} else if char == '\\' {
					escaped = true
				} else if char == '"' {
					inString = false
				}
				continue
			}
			switch char {
			case '"':
				inString = true
			case '{', '[':
				stack = append(stack, char)
			case '}', ']':
				open := stack[len(stack)-1]
				if (open == '{' && char != '}') || (open == '[' && char != ']') {
					return 0, errors.New("mismatched JSON container")
				}
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return index + 1, nil
				}
			}
		}
		return 0, errors.New("unterminated JSON container")
	}
	index := start
	for index < len(raw) && raw[index] != ',' && raw[index] != '}' {
		index++
	}
	end := index
	for end > start {
		switch raw[end-1] {
		case ' ', '\t', '\n', '\r':
			end--
		default:
			return end, nil
		}
	}
	return 0, errors.New("empty JSON value")
}
