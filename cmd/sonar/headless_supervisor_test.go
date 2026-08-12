package main

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestHeadlessRefusesCortexLinkedGoalBeforeDispatch(t *testing.T) {
	if headlessRefusesCortexLinkedGoal(goal.Snapshot{}) {
		t.Fatal("an unlinked goal was refused")
	}
	if !headlessRefusesCortexLinkedGoal(goal.Snapshot{
		Cortex: goal.CortexCorrelation{TaskID: "task_1", Actor: "sonar"},
	}) {
		t.Fatal("a Cortex-linked goal was admitted to a headless path with no evaluator")
	}
}
