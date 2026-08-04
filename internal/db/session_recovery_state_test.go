package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestInspectSessionRecoveryStateValidatesAuthorityFields(t *testing.T) {
	runtime, err := goal.New(goal.Spec{
		SessionID: 7, Objective: "recover safely",
		AcceptanceCriteria: []goal.AcceptanceCriterion{{ID: "safe", Description: "Recovery remains explicit."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	goalJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	view, err := InspectSessionRecoveryState(7, `{"version":2,"goal":`+string(goalJSON)+`,"execution_cursor":42}`)
	if err != nil {
		t.Fatal(err)
	}
	if view.Version != 2 || !view.GoalOwned || view.ExecutionCursor != 42 {
		t.Fatalf("recovery state = %#v", view)
	}

	legacy, err := InspectSessionRecoveryState(7, `{"version":1,"goal":null,"execution_cursor":17}`)
	if err != nil || legacy.Version != 1 || legacy.GoalOwned || legacy.ExecutionCursor != 17 {
		t.Fatalf("legacy recovery state = %#v, err=%v", legacy, err)
	}
}

func TestInspectSessionRecoveryStateRejectsHazardHidingOrAmbiguousState(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "future version", raw: `{"version":999,"execution_cursor":9223372036854775807}`, want: "unsupported"},
		{name: "duplicate cursor", raw: `{"version":2,"execution_cursor":0,"execution_cursor":99}`, want: "duplicate"},
		{name: "negative cursor", raw: `{"version":2,"execution_cursor":-1}`, want: "negative"},
		{name: "negative legacy cursor", raw: `{"version":1,"execution_cursor":-1}`, want: "negative"},
		{name: "invalid goal", raw: `{"version":2,"goal":{"session_id":7}}`, want: "validate durable goal"},
		{name: "foreign goal", raw: `{"version":2,"goal":{"session_id":8}}`, want: "does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InspectSessionRecoveryState(7, test.raw); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
