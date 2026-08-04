package reconciliation

import (
	"strings"
	"testing"
	"time"
)

var testObservedAt = time.Date(2026, time.July, 12, 12, 30, 0, 123_000_000, time.UTC)

func validRequest() Request {
	return Request{
		Disposition: DispositionEffectNotApplied,
		Source: Source{
			Kind: SourceVerificationCheck, Reference: "check:workspace/file-absence",
			ObservedAt: testObservedAt,
		},
		Summary: "Verified the expected artifact was not created.",
	}
}

func validTarget(t *testing.T) Target {
	t.Helper()
	fingerprint := EventFingerprint{
		EventID: 17, SessionID: 7, WorkspaceID: "/workspace/project",
		RunID: "run_123", ExecutionID: "exec_123", IdempotencyKey: "idem_123", TurnID: "turn_123",
		CanonicalCallID: "call_123", ToolName: "write_file", Kind: "builtin", Iteration: 1, Ordinal: 1,
		EventType: EventTypeOutcomeUnknown, EffectClass: "effectful",
		Approval: "not_applicable", OccurredAt: testObservedAt.Add(-time.Minute),
		ArgumentsSHA256: Hash("redacted arguments"), ResultSHA256: Hash("unknown receipt"),
	}
	digest, err := fingerprint.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return Target{
		SessionID: 7, WorkspaceID: "/workspace/project", GoalID: "goal_123",
		ItemID: "ctrl_item", ItemPayloadSHA256: Hash("item payload"),
		ExecutionID: "exec_123", TurnID: "turn_123",
		LatestEventID: 17, LatestEventType: EventTypeOutcomeUnknown,
		LatestEventSHA256: digest, Actor: "local-user",
	}
}

func TestEvidenceRoundTripAllStableValues(t *testing.T) {
	dispositions := []Disposition{
		DispositionEffectApplied, DispositionEffectNotApplied, DispositionEffectCompensated,
	}
	sources := []SourceKind{
		SourceExternalReceipt, SourceWorkspaceArtifact, SourceVerificationCheck, SourceOperatorObservation,
	}
	for _, disposition := range dispositions {
		for _, source := range sources {
			request := validRequest()
			request.Disposition = disposition
			request.Source.Kind = source
			target := validTarget(t)
			envelope, err := request.Bind(target)
			if err != nil {
				t.Fatalf("bind %s/%s: %v", disposition, source, err)
			}
			document, digest, err := envelope.Marshal()
			if err != nil {
				t.Fatalf("marshal %s/%s: %v", disposition, source, err)
			}
			if strings.Contains(document, "arguments") || strings.Contains(document, "results") {
				t.Fatalf("private raw payload field leaked into %s", document)
			}
			parsed, err := Parse(document, digest)
			if err != nil {
				t.Fatalf("parse %s/%s: %v", disposition, source, err)
			}
			if parsed != envelope || !parsed.MatchesTarget(target) {
				t.Fatalf("round trip = %#v, want %#v", parsed, envelope)
			}
		}
	}
}

func TestEvidenceRejectsNonCanonicalUnboundedAndForgedInput(t *testing.T) {
	request := validRequest()
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{"disposition", func(r *Request) { r.Disposition = "applied" }},
		{"source kind", func(r *Request) { r.Source.Kind = "memory" }},
		{"empty reference", func(r *Request) { r.Source.Reference = "" }},
		{"padded reference", func(r *Request) { r.Source.Reference = " receipt " }},
		{"invalid reference utf8", func(r *Request) { r.Source.Reference = string([]byte{0xff}) }},
		{"zero observed time", func(r *Request) { r.Source.ObservedAt = time.Time{} }},
		{"non utc observed time", func(r *Request) { r.Source.ObservedAt = testObservedAt.In(time.FixedZone("offset", 3600)) }},
		{"empty summary", func(r *Request) { r.Summary = "" }},
		{"padded summary", func(r *Request) { r.Summary = " evidence " }},
		{"oversized summary", func(r *Request) { r.Summary = strings.Repeat("x", MaxSummaryBytes+1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := request
			test.mutate(&candidate)
			if _, err := candidate.Bind(validTarget(t)); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}

	for _, mutate := range []func(*Target){
		func(target *Target) { target.SessionID = 0 },
		func(target *Target) { target.WorkspaceID = " workspace " },
		func(target *Target) { target.GoalID = " goal " },
		func(target *Target) { target.ItemPayloadSHA256 = "bad" },
		func(target *Target) { target.ExecutionID = "" },
		func(target *Target) { target.TurnID = string([]byte{0xff}) },
		func(target *Target) { target.LatestEventID = 0 },
		func(target *Target) { target.LatestEventType = "completed" },
		func(target *Target) { target.LatestEventSHA256 = "bad" },
		func(target *Target) { target.Actor = "" },
	} {
		target := validTarget(t)
		mutate(&target)
		if _, err := request.Bind(target); err == nil {
			t.Fatalf("invalid target %#v was accepted", target)
		}
	}
}

func TestEvidenceAllowsGoalLessExecutionTarget(t *testing.T) {
	target := validTarget(t)
	target.GoalID = ""
	envelope, err := validRequest().Bind(target)
	if err != nil {
		t.Fatal(err)
	}
	document, digest, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(document, digest)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.GoalID != "" || !parsed.MatchesTarget(target) {
		t.Fatalf("goal-less envelope = %#v", parsed)
	}
}

func TestEvidenceParseRejectsDigestUnknownFieldsAndNonCanonicalJSON(t *testing.T) {
	envelope, err := validRequest().Bind(validTarget(t))
	if err != nil {
		t.Fatal(err)
	}
	document, digest, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(document, digest); err != nil {
		t.Fatalf("canonical evidence did not parse: %v", err)
	}
	if _, err := Parse(document, strings.Repeat("0", 64)); err == nil {
		t.Fatal("forged digest was accepted")
	}
	unknown := strings.TrimSuffix(document, "}") + `,"raw_arguments":{"token":"secret"}}`
	if _, err := Parse(unknown, Hash(unknown)); err == nil {
		t.Fatal("unknown raw arguments field was accepted")
	}
	nonCanonical := " " + document
	if _, err := Parse(nonCanonical, Hash(nonCanonical)); err == nil {
		t.Fatal("non-canonical JSON was accepted")
	}
	multiple := document + `{}`
	if _, err := Parse(multiple, Hash(multiple)); err == nil {
		t.Fatal("multiple JSON documents were accepted")
	}
}

func TestEventFingerprintHazardSemantics(t *testing.T) {
	target := validTarget(t)
	base := EventFingerprint{
		EventID: target.LatestEventID, SessionID: target.SessionID, WorkspaceID: target.WorkspaceID,
		RunID: "run", ExecutionID: target.ExecutionID, IdempotencyKey: "idem", TurnID: target.TurnID,
		CanonicalCallID: "call", ToolName: "tool", Kind: "builtin", Iteration: 1, Ordinal: 1,
		EventType: EventTypeOutcomeUnknown, EffectClass: "read_only", Approval: "not_applicable",
		ArgumentsSHA256: Hash("arguments"), OccurredAt: testObservedAt,
	}
	if _, err := base.Digest(); err != nil {
		t.Fatalf("outcome_unknown read-only fingerprint was rejected: %v", err)
	}
	base.EventType = EventTypeStarted
	if _, err := base.Digest(); err == nil {
		t.Fatal("started read-only fingerprint was accepted as hazardous")
	}
	base.EffectClass = "effectful"
	if _, err := base.Digest(); err != nil {
		t.Fatalf("started effectful fingerprint was rejected: %v", err)
	}
}
