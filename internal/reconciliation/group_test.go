package reconciliation

import (
	"strings"
	"testing"
)

func validGroupTarget() GroupTarget {
	return GroupTarget{
		SessionID: 7, WorkspaceID: "/workspace/project", GoalID: "goal_123", TurnID: "turn_123",
		GroupItemID: "recongrp_123", GroupPayloadSHA256: Hash("group payload"),
		BlockerReference: "exec_123", GoalSnapshotSHA256: Hash("goal snapshot"),
		SnapshotCursor: 42, MemberSetSHA256: Hash("member set"), ExecutionMemberCount: 2,
		Actor: "local-user",
	}
}

func validTurnRequest() TurnRequest {
	request := validRequest()
	return TurnRequest{
		Conclusion: TurnAbandonedAfterInspection,
		Source:     request.Source, Summary: "Inspected the abandoned provider turn and every durable member.",
	}
}

func TestGroupEvidenceRoundTripAndZeroExecutionAuthority(t *testing.T) {
	for _, count := range []int{0, 2} {
		target := validGroupTarget()
		target.ExecutionMemberCount = count
		envelope, err := validTurnRequest().Bind(target)
		if err != nil {
			t.Fatal(err)
		}
		document, digest, err := envelope.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(document, "raw_arguments") || strings.Contains(document, "result_receipt") {
			t.Fatalf("raw execution data leaked into group evidence: %s", document)
		}
		parsed, err := ParseGroup(document, digest)
		if err != nil || parsed != envelope || !parsed.MatchesTarget(target) {
			t.Fatalf("group round trip = %#v, error=%v", parsed, err)
		}
	}
}

func TestGroupEvidenceRejectsForgedBindingsAndUnknownFields(t *testing.T) {
	target := validGroupTarget()
	for _, mutate := range []func(*GroupTarget){
		func(value *GroupTarget) { value.SessionID = 0 },
		func(value *GroupTarget) { value.GoalID = "" },
		func(value *GroupTarget) { value.GroupPayloadSHA256 = "bad" },
		func(value *GroupTarget) { value.SnapshotCursor = -1 },
		func(value *GroupTarget) { value.ExecutionMemberCount = MaxGroupMembers + 1 },
		func(value *GroupTarget) { value.Actor = " forged " },
	} {
		candidate := target
		mutate(&candidate)
		if _, err := validTurnRequest().Bind(candidate); err == nil {
			t.Fatalf("invalid group target accepted: %#v", candidate)
		}
	}
	envelope, err := validTurnRequest().Bind(target)
	if err != nil {
		t.Fatal(err)
	}
	document, _, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.TrimSuffix(document, "}") + `,"raw_results":"secret"}`
	if _, err := ParseGroup(forged, Hash(forged)); err == nil {
		t.Fatal("unknown raw result field was accepted")
	}
	invalid := validTurnRequest()
	invalid.Conclusion = "effect_applied"
	if _, err := invalid.Bind(target); err == nil {
		t.Fatal("execution disposition was accepted as a turn conclusion")
	}
	invalidUTF8 := string([]byte{0xff, '{', '}'})
	if _, err := ParseGroup(invalidUTF8, Hash(invalidUTF8)); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 group evidence error = %v", err)
	}
}
