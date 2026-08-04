package command

import (
	"strings"
	"testing"
)

func TestGoalActionsHaveStableSharedMetadata(t *testing.T) {
	r := newTestRegistry()
	states := r.Actions("goal", nil)
	want := []ActionID{
		GoalActionNew,
		GoalActionInspect,
		GoalActionPause,
		GoalActionResume,
		GoalActionBudget,
		GoalActionDrop,
	}
	if len(states) != len(want) {
		t.Fatalf("goal actions = %d, want %d", len(states), len(want))
	}
	for index, state := range states {
		if state.Spec.ID != want[index] {
			t.Fatalf("action %d = %q, want %q", index, state.Spec.ID, want[index])
		}
		if state.Spec.CommandText() == "" || state.Spec.Title == "" || state.Spec.Description == "" {
			t.Fatalf("action %q has incomplete metadata: %#v", state.Spec.ID, state.Spec)
		}
	}
	if spec, ok := r.MatchAction("goal", "retry"); !ok || spec.ID != GoalActionResume {
		t.Fatalf("retry alias = %#v, %v", spec, ok)
	}
	if spec, ok := r.MatchAction("goal", "status"); !ok || spec.ID != GoalActionInspect {
		t.Fatalf("status alias = %#v, %v", spec, ok)
	}
}

func TestGoalActionStatesExplainUnsafeTransitions(t *testing.T) {
	r := newTestRegistry()
	tests := []struct {
		name       string
		ctx        *Context
		id         ActionID
		enabled    bool
		reasonPart string
	}{
		{name: "active pause", ctx: &Context{GoalConfigured: true, GoalStatus: "active"}, id: GoalActionPause, enabled: true},
		{name: "paused pause", ctx: &Context{GoalConfigured: true, GoalStatus: "paused"}, id: GoalActionPause, reasonPart: "active"},
		{name: "pending drop", ctx: &Context{GoalConfigured: true, GoalStatus: "active", GoalPending: true}, id: GoalActionDrop, reasonPart: "settle"},
		{name: "unknown outcome resume", ctx: &Context{GoalConfigured: true, GoalStatus: "blocked", GoalBlocker: "outcome_unknown"}, id: GoalActionResume, reasonPart: "reconcile"},
		{name: "exhausted resume", ctx: &Context{GoalConfigured: true, GoalStatus: "exhausted", GoalExhausted: true}, id: GoalActionResume, reasonPart: "budget"},
		{name: "replenished resume", ctx: &Context{GoalConfigured: true, GoalStatus: "exhausted"}, id: GoalActionResume, enabled: true},
		{name: "dirty drop", ctx: &Context{GoalConfigured: true, GoalStatus: "paused", GoalPersistenceDirty: true}, id: GoalActionDrop, reasonPart: "persistence"},
		{name: "busy budget", ctx: &Context{GoalConfigured: true, GoalStatus: "paused", GoalBusy: true}, id: GoalActionBudget, reasonPart: "settle"},
		{name: "terminal budget", ctx: &Context{GoalConfigured: true, GoalStatus: "completed"}, id: GoalActionBudget, reasonPart: "completed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ActionState
			for _, state := range r.Actions("goal", tt.ctx) {
				if state.Spec.ID == tt.id {
					got = state
					break
				}
			}
			if got.Spec.ID == "" {
				t.Fatalf("action %q not found", tt.id)
			}
			if got.Enabled != tt.enabled {
				t.Fatalf("enabled = %v, want %v (%q)", got.Enabled, tt.enabled, got.DisabledReason)
			}
			if !tt.enabled && !strings.Contains(strings.ToLower(got.DisabledReason), tt.reasonPart) {
				t.Fatalf("disabled reason = %q, want %q", got.DisabledReason, tt.reasonPart)
			}
		})
	}
}

func TestImageActionsHaveStableSharedMetadata(t *testing.T) {
	r := newTestRegistry()
	states := r.Actions("image", nil)
	want := []struct {
		id          ActionID
		action      Action
		destructive bool
	}{
		{id: ImageActionList, action: ActionListImages},
		{id: ImageActionClear, action: ActionClearImages, destructive: true},
		{id: ImageActionForgetHistory, action: ActionForgetImageHistory, destructive: true},
	}
	if len(states) != len(want) {
		t.Fatalf("image actions = %d, want %d", len(states), len(want))
	}
	for index, expected := range want {
		got := states[index]
		if got.Spec.ID != expected.id || got.Spec.Action != expected.action || got.Spec.Destructive != expected.destructive {
			t.Fatalf("action %d = %#v, want id=%q action=%d destructive=%v", index, got.Spec, expected.id, expected.action, expected.destructive)
		}
		if got.Spec.CommandText() == "" || got.Spec.Title == "" || got.Spec.Description == "" || !got.Enabled {
			t.Fatalf("action %q has incomplete metadata: %#v", got.Spec.ID, got)
		}
	}
	if spec, ok := r.MatchAction("image", "ls"); !ok || spec.ID != ImageActionList {
		t.Fatalf("list alias = %#v, %v", spec, ok)
	}
	if spec, ok := r.MatchAction("image", "remove-all"); !ok || spec.ID != ImageActionClear {
		t.Fatalf("clear alias = %#v, %v", spec, ok)
	}
	if spec, ok := r.MatchAction("image", "drop-history"); !ok || spec.ID != ImageActionForgetHistory {
		t.Fatalf("forget-history alias = %#v, %v", spec, ok)
	}
}
