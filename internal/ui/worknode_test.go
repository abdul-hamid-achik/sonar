package ui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestWorkNodeValidationIsBoundedAndStatusSpecific(t *testing.T) {
	valid := WorkNode{
		ID: "expert-00", Index: 0, Kind: WorkNodeKindExpert,
		Label: "critic", Model: "qwen3.5:2b",
		Location: WorkNodeLocationLocal, Status: WorkNodeRunning,
		Activity: WorkNodeActivityRunning, Revision: 2,
	}
	if !valid.valid(2) {
		t.Fatal("valid running node rejected")
	}

	tests := []struct {
		name string
		edit func(*WorkNode)
	}{
		{"unknown status", func(node *WorkNode) { node.Status = 0 }},
		{"unknown kind", func(node *WorkNode) { node.Kind = "" }},
		{"activity mismatch", func(node *WorkNode) { node.Activity = WorkNodeActivityWaiting }},
		{"revision mismatch", func(node *WorkNode) { node.Revision = 1 }},
		{"negative elapsed", func(node *WorkNode) { node.Elapsed = -1 }},
		{"unread beyond revision", func(node *WorkNode) { node.Unread = 3 }},
		{"invalid id", func(node *WorkNode) { node.ID = "private/path" }},
		{"out of range index", func(node *WorkNode) { node.Index = 2 }},
		{"oversized label", func(node *WorkNode) { node.Label = strings.Repeat("x", maxWorkNodeLabelBytes+1) }},
		{"oversized model", func(node *WorkNode) { node.Model = strings.Repeat("x", maxWorkNodeModelBytes+1) }},
		{"running tokens", func(node *WorkNode) { node.EvalTokens = 1 }},
		{"raw failure", func(node *WorkNode) {
			node.Status, node.FailureCode = WorkNodeFailed, "provider said secret"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node := valid
			test.edit(&node)
			if node.valid(2) {
				t.Fatalf("unsafe node survived validation: %#v", node)
			}
		})
	}

	queued := WorkNode{
		ID: "expert-01", Index: 1, Kind: WorkNodeKindExpert,
		Status: WorkNodeQueued, Location: WorkNodeLocationUnknown,
		Activity: WorkNodeActivityQueued, Revision: 1,
	}
	if !queued.valid(2) {
		t.Fatal("valid queued node rejected")
	}
	queued.Label = "untrusted"
	if queued.valid(2) {
		t.Fatal("queued node accepted child-controlled identity")
	}
}

func TestExpertProgressAdapterIsPureSafeAndIndexStable(t *testing.T) {
	state := &ExpertProgressState{
		Sequence: 5, Strategy: "swarm", Total: 4, Parallelism: 2,
		Running: 1, Queued: 1, Completed: 1, Failed: 1,
		Experts: []ExpertProgressItem{
			{Index: 0, Expert: "done", Model: "m0", Location: llm.OllamaModelLocationLocal, Phase: expertteam.ProgressCompleted, Status: expertteam.ExpertCompleted, EvalTokens: 9},
			{Index: 1, Expert: "active", Model: "m1", Location: llm.OllamaModelLocationCloud, Phase: expertteam.ProgressStarted},
			{},
			{Index: 3, Expert: "failed", Model: "m3", Location: llm.OllamaModelLocationRemote, Phase: expertteam.ProgressFailed, Status: expertteam.ExpertFailed, FailureCode: "timed_out"},
		},
	}
	before := cloneExpertProgressState(state)
	nodes, ok := workNodesFromExpertProgress(state)
	if !ok {
		t.Fatal("valid progress state rejected")
	}
	if !reflect.DeepEqual(state, before) {
		t.Fatalf("adapter mutated its input:\nbefore %#v\nafter  %#v", before, state)
	}
	got := make([]WorkNodeStatus, len(nodes))
	for index := range nodes {
		got[index] = nodes[index].Status
	}
	want := []WorkNodeStatus{WorkNodeCompleted, WorkNodeRunning, WorkNodeQueued, WorkNodeFailed}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status order = %v, want %v", got, want)
	}
	if nodes[0].ID != "expert-00" || nodes[2].ID != "expert-02" {
		t.Fatalf("IDs are not host-derived and stable: %#v", nodes)
	}
	if nodes[0].Kind != WorkNodeKindExpert || nodes[1].Kind != WorkNodeKindExpert {
		t.Fatalf("adapter did not assign the host-owned kind: %#v", nodes)
	}
	for _, node := range nodes {
		if node.Revision != workNodeRevisionForStatus(node.Status) ||
			node.Activity != workNodeActivityForStatus(node.Status) ||
			node.ParentID != "" || node.Elapsed != 0 || node.Unread != 0 ||
			node.ReportRef != nil {
			t.Fatalf("adapter invented or omitted generic work metadata: %#v", node)
		}
	}
	if nodes[0].Location != WorkNodeLocationLocal ||
		nodes[1].Location != WorkNodeLocationCloud ||
		nodes[3].Location != WorkNodeLocationRemoteHost {
		t.Fatalf("adapter location projection = %#v", nodes)
	}
}

func TestWorkNodeOrderingStaysStableAcrossStatusChanges(t *testing.T) {
	nodes := []WorkNode{
		{ID: "node-z", Index: 2, Status: WorkNodeRunning},
		{ID: "node-b", Index: 1, Status: WorkNodeFailed},
		{ID: "node-a", Index: 1, Status: WorkNodeAttention},
		{ID: "node-c", Index: 0, Status: WorkNodeCompleted},
	}
	ordered := orderedWorkNodes(nodes)
	got := []string{ordered[0].ID, ordered[1].ID, ordered[2].ID, ordered[3].ID}
	want := []string{"node-c", "node-a", "node-b", "node-z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered IDs = %v, want %v", got, want)
	}
	if nodes[0].ID != "node-z" {
		t.Fatal("ordering mutated caller slice")
	}
}

func TestWorkNodePresentationPrioritizesLiveWorkWithoutMutatingCanonicalOrder(t *testing.T) {
	nodes := []WorkNode{
		{ID: "completed-0", Index: 0, Status: WorkNodeCompleted},
		{ID: "queued-1", Index: 1, Status: WorkNodeQueued},
		{ID: "failed-2", Index: 2, Status: WorkNodeFailed},
		{ID: "attention-3", Index: 3, Status: WorkNodeAttention},
		{ID: "waiting-4", Index: 4, Status: WorkNodeWaiting},
		{ID: "running-5", Index: 5, Status: WorkNodeRunning},
		{ID: "completed-6", Index: 6, Status: WorkNodeCompleted},
	}

	presented := presentedWorkNodes(nodes)
	got := make([]string, len(presented))
	for index := range presented {
		got[index] = presented[index].ID
	}
	want := []string{
		"attention-3", "waiting-4", "running-5",
		"queued-1",
		"completed-0", "failed-2", "completed-6",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("presented IDs = %v, want %v", got, want)
	}
	if nodes[0].ID != "completed-0" || nodes[3].ID != "attention-3" {
		t.Fatalf("presentation order mutated canonical caller slice: %#v", nodes)
	}
}

func TestWorkNodeProjectionHasNoPrivateAuthority(t *testing.T) {
	typ := reflect.TypeOf(WorkNode{})
	forbiddenFields := map[string]bool{
		"prompt": true, "objective": true, "reporttext": true, "reasoning": true,
		"error": true, "path": true, "transcript": true, "metadata": true,
	}
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if forbiddenFields[strings.ToLower(field.Name)] {
			t.Fatalf("WorkNode exposes forbidden field %q", field.Name)
		}
	}

	encoded, err := json.Marshal(WorkNode{
		ID: "expert-00", Index: 0, Kind: WorkNodeKindExpert,
		Label: "critic", Model: "safe-model",
		Location: WorkNodeLocationLocal, Status: WorkNodeFailed,
		Activity: WorkNodeActivityFailed, Revision: 3,
		FailureCode: "timed_out",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"objective", "report", "raw_error", "reasoning", "transcript", "path", "metadata"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("safe projection serialized forbidden authority %q: %s", forbidden, encoded)
		}
	}
}

func TestWorkNodeParentRevisionActivityAndUnreadAreBounded(t *testing.T) {
	node := WorkNode{
		ID:       "node-safe",
		ParentID: "group-safe",
		Index:    0,
		Kind:     WorkNodeKindExpert,
		Label:    "critic",
		Model:    "safe-model",
		Location: WorkNodeLocationLocal,
		Status:   WorkNodeCompleted,
		Activity: WorkNodeActivityCompleted,
		Unread:   2,
		Revision: 3,
	}
	if !node.valid(1) {
		t.Fatalf("valid hierarchical node rejected: %#v", node)
	}
	for _, edit := range []func(*WorkNode){
		func(candidate *WorkNode) { candidate.ParentID = "private/path" },
		func(candidate *WorkNode) { candidate.Activity = WorkNodeActivityRunning },
		func(candidate *WorkNode) { candidate.Revision = 0 },
		func(candidate *WorkNode) { candidate.Unread = 4 },
	} {
		candidate := node
		edit(&candidate)
		if candidate.valid(1) {
			t.Fatalf("invalid hierarchical metadata survived: %#v", candidate)
		}
	}
}

func TestWorkReportRefRequiresExactSupportedArtifactEvidence(t *testing.T) {
	artifact := ecosystem.ArtifactDigest{
		Kind:          ecosystem.ArtifactDigestFileCheapStash,
		ID:            "report-stash",
		URI:           "fcheap://stash/report-stash",
		SchemaVersion: "1.0",
		ContentSHA256: strings.Repeat("a", 64),
		FileCount:     1,
		TotalSize:     42,
		CreatedAt:     "2026-07-18T12:00:00Z",
	}
	projection := ecosystem.ToolProjection{
		Specialist: "filecheap",
		Operation:  "filecheap_save",
		Role:       ecosystem.RoleArtifact,
		Transport:  ecosystem.TransportSucceeded,
		Domain:     ecosystem.DomainSucceeded,
		Evidence:   ecosystem.EvidenceSupported,
		Artifact:   &artifact,
	}
	ref, ok := workReportRefFromProjection(projection)
	if !ok || ref == nil || !reflect.DeepEqual(*ref, artifact) {
		t.Fatalf("supported report artifact was rejected: %#v, ok=%v", ref, ok)
	}
	node := WorkNode{
		ID:        "node-safe",
		ParentID:  "group-safe",
		Index:     0,
		Kind:      WorkNodeKindExpert,
		Label:     "critic",
		Model:     "safe-model",
		Location:  WorkNodeLocationLocal,
		Status:    WorkNodeCompleted,
		Activity:  WorkNodeActivityCompleted,
		Revision:  3,
		ReportRef: ref,
	}
	if !node.valid(1) {
		t.Fatalf("node rejected exact report artifact: %#v", node)
	}

	tampered := artifact
	tampered.URI = "file:///private/report"
	node.ReportRef = &tampered
	if node.valid(1) {
		t.Fatal("node accepted a caller-authored report URI")
	}
	projection.Evidence = ecosystem.EvidenceCandidate
	if ref, ok := workReportRefFromProjection(projection); ok || ref != nil {
		t.Fatalf("candidate evidence became a report ref: %#v, ok=%v", ref, ok)
	}
}

func TestWorkNodeLocationAdapterIsExhaustiveAndFailsClosed(t *testing.T) {
	tests := []struct {
		source llm.OllamaModelLocation
		want   WorkNodeLocation
	}{
		{llm.OllamaModelLocationUnknown, WorkNodeLocationUnknown},
		{llm.OllamaModelLocationLocal, WorkNodeLocationLocal},
		{llm.OllamaModelLocationCloud, WorkNodeLocationCloud},
		{llm.OllamaModelLocationRemote, WorkNodeLocationRemoteHost},
	}
	for _, test := range tests {
		got, ok := workNodeLocationFromExpertSource(test.source)
		if !ok || got != test.want {
			t.Fatalf("location %q = %q, %v; want %q, true", test.source, got, ok, test.want)
		}
	}
	if got, ok := workNodeLocationFromExpertSource(llm.OllamaModelLocation("provider-private")); ok || got != "" {
		t.Fatalf("unknown source location survived as %q, %v", got, ok)
	}

	typ := reflect.TypeOf(WorkNode{})
	field, ok := typ.FieldByName("Location")
	if !ok || field.Type != reflect.TypeOf(WorkNodeLocation("")) {
		t.Fatalf("WorkNode location type = %v, want UI-owned WorkNodeLocation", field.Type)
	}
}

func TestExpertProgressDetailsKeepIndexOrderWithoutChangingGrammar(t *testing.T) {
	state := &ExpertProgressState{
		Sequence: 4, Strategy: "team", Total: 3, Parallelism: 1,
		Running: 1, Queued: 1, Failed: 1,
		Experts: []ExpertProgressItem{
			{Index: 0, Expert: "active", Model: "m0", Location: llm.OllamaModelLocationLocal, Phase: expertteam.ProgressStarted},
			{},
			{Index: 2, Expert: "failed", Model: "m2", Location: llm.OllamaModelLocationCloud, Phase: expertteam.ProgressFailed, Status: expertteam.ExpertFailed, FailureCode: "model_unavailable"},
		},
	}
	view := state.renderDetails(80, NewToolCardStyles(true, defaultThemeID))
	failedAt, runningAt, queuedAt := strings.Index(view, "failed"), strings.Index(view, "active"), strings.Index(view, "1 more queued")
	if failedAt < 0 || runningAt < 0 || queuedAt < 0 ||
		runningAt >= queuedAt || queuedAt >= failedAt {
		t.Fatalf("unexpected work-node order or grammar:\n%s", view)
	}
}

func TestExpertProgressAdapterDistinguishesCancellationFromFailure(t *testing.T) {
	state := &ExpertProgressState{
		Sequence: 3, Strategy: "team", Total: 1, Parallelism: 1,
		Failed: 1,
		Experts: []ExpertProgressItem{{
			Index: 0, Expert: "critic", Model: "m0",
			Location: llm.OllamaModelLocationLocal,
			Phase:    expertteam.ProgressFailed, Status: expertteam.ExpertFailed,
			FailureCode: "cancelled",
		}},
	}
	nodes, ok := workNodesFromExpertProgress(state)
	if !ok || len(nodes) != 1 || nodes[0].Status != WorkNodeCancelled {
		t.Fatalf("cancelled progress projection = %#v, ok=%v", nodes, ok)
	}
	if view := state.renderDetails(80, NewToolCardStyles(true, defaultThemeID)); !strings.Contains(view, "cancelled") {
		t.Fatalf("cancelled work node lacks distinct presentation:\n%s", view)
	}
}
