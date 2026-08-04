package db

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestPatchReconciliationSessionPreservesUnknownBytes(t *testing.T) {
	raw := `{ "version" : 2, "future" : { "spacing" : [1, 2, {"x":"y"}] }, "goal" : {"old":true}, "tail":"keep" }`
	nextGoal := goal.Snapshot{Version: goal.SnapshotVersion, ID: "goal_test", SessionID: 1}
	patched, err := patchReconciliationSession(raw, nextGoal, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patched, `"future" : { "spacing" : [1, 2, {"x":"y"}] }`) ||
		!strings.Contains(patched, `"tail":"keep"`) || !strings.Contains(patched, `"execution_cursor":42`) {
		t.Fatalf("unknown session bytes were not preserved: %s", patched)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(patched), &fields); err != nil {
		t.Fatal(err)
	}
	var restored goal.Snapshot
	if err := json.Unmarshal(fields["goal"], &restored); err != nil || restored.ID != nextGoal.ID {
		t.Fatalf("patched goal = %#v, error=%v", restored, err)
	}
}

func TestPatchTopLevelJSONObjectHandlesEscapesAndRejectsInvalidInput(t *testing.T) {
	raw := `{"escaped\"key":{"nested":"} ] ,"},"goal":{"old":true}}`
	patched, err := patchTopLevelJSONObject([]byte(raw), map[string][]byte{"goal": []byte(`{"new":true}`)})
	if err != nil || patched != `{"escaped\"key":{"nested":"} ] ,"},"goal":{"new":true}}` {
		t.Fatalf("escaped patch = %q, error=%v", patched, err)
	}
	for _, invalid := range []string{"[]", `{}`, `{"goal":`, string([]byte{0xff})} {
		if _, err := patchTopLevelJSONObject([]byte(invalid), map[string][]byte{"goal": []byte(`{}`)}); err == nil && invalid != `{}` {
			t.Fatalf("invalid JSON %q was accepted", invalid)
		}
	}
	duplicate := `{"version":2,"goal":{"first":true},"goal":{"second":true}}`
	if _, err := patchTopLevelJSONObject([]byte(duplicate), map[string][]byte{"goal": []byte(`{}`)}); err == nil {
		t.Fatal("duplicate authority key was accepted")
	}
}
