package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestToolViewModelFromToolEntryProjectsOnlyBoundedSafeFields(t *testing.T) {
	const secret = "RAW_SECRET_MUST_NOT_CROSS_TOOL_UI_BOUNDARY"
	projection := ecosystem.ToolProjection{
		Specialist: "fcheap",
		Operation:  "fcheap_save",
		Role:       ecosystem.RoleArtifact,
		Transport:  ecosystem.TransportSucceeded,
		Domain:     ecosystem.DomainSucceeded,
		Evidence:   ecosystem.EvidenceSupported,
		Artifact: &ecosystem.ArtifactDigest{
			Kind:          ecosystem.ArtifactDigestFileCheapStash,
			ID:            "stash-123",
			SchemaVersion: "1.0",
			ContentSHA256: strings.Repeat("a", 64),
			FileCount:     2,
			TotalSize:     42,
			CreatedAt:     "2026-07-17T12:00:00Z",
		},
	}.Normalize()
	entry := ToolEntry{
		ID:         "call-123",
		Name:       "fcheap_save",
		Summary:    "safe summary",
		Args:       `{"value":"` + secret + `"}`,
		RawArgs:    map[string]any{"value": secret},
		Result:     secret,
		Status:     ToolStatusDone,
		Duration:   125 * time.Millisecond,
		Projection: projection,
	}
	chat := ChatEntry{BlockID: "block-tool-123", Revision: 7, Kind: "tool_group"}

	view, err := ToolViewModelFromToolEntry(chat, entry)
	if err != nil {
		t.Fatalf("project tool entry: %v", err)
	}
	if view.InvocationID != entry.ID || view.BlockID != chat.BlockID || view.Revision != chat.Revision {
		t.Fatalf("identity projection = %#v", view)
	}
	if view.Kind != ToolKindGeneric || view.Lifecycle != ToolLifecycleSucceeded {
		t.Fatalf("semantic projection = %#v", view)
	}
	if view.Transport != ecosystem.TransportSucceeded || view.Domain != ecosystem.DomainSucceeded ||
		view.Evidence != ecosystem.EvidenceSupported {
		t.Fatalf("ecosystem states were conflated: %#v", view)
	}
	if view.Target != "fcheap" || view.Artifact == nil || view.Artifact.URI != "fcheap://stash/stash-123" {
		t.Fatalf("typed artifact projection = %#v", view)
	}

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal safe view model: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("raw argument/result crossed view-model boundary: %s", encoded)
	}
	for _, field := range []string{"Args", "RawArgs", "Result", "ResultDisplay", "StructuredContent"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("view model unexpectedly exposed %s: %s", field, encoded)
		}
	}
}

func TestToolRenderModelAdapterExcludesEphemeralAndStructuredSurfaces(t *testing.T) {
	const (
		rawArgSecret = "RAW_ARG_SECRET_MUST_NOT_CROSS"
		beforeSecret = "BEFORE_SNAPSHOT_SECRET_MUST_NOT_CROSS"
		oscSecret    = "OSC_SECRET_MUST_NOT_CROSS"
	)
	entry := ToolEntry{
		ID:      "call-private-1",
		Name:    "read_file",
		Summary: "safe summary",
		Args:    "path=safe.txt",
		RawArgs: map[string]any{"token": rawArgSecret},
		Result:  "safe result",
		ResultDisplay: "\x1b[32msafe result\x1b[0m" +
			"\x1b]0;" + oscSecret + "\x07",
		Status:        ToolStatusDone,
		BeforeContent: beforeSecret,
		Projection: ecosystem.ToolProjection{
			Operation: "read_file",
			Transport: ecosystem.TransportSucceeded,
			Domain:    ecosystem.DomainSucceeded,
			Evidence:  ecosystem.EvidenceNone,
		}.Normalize(),
	}
	model, err := ToolRenderModelFromEntry(
		ChatEntry{BlockID: "block-private-1", Revision: 3, Kind: "tool_group"},
		entry,
	)
	if err != nil {
		t.Fatalf("project render model: %v", err)
	}
	card, err := ToolCardFromRenderModel(model, true, defaultThemeID)
	if err != nil {
		t.Fatalf("construct card: %v", err)
	}
	if card.ID != entry.ID || card.Result != "safe result" || card.Args != "path=safe.txt" {
		t.Fatalf("bounded card projection = %#v", card)
	}

	encoded, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("marshal render model: %v", err)
	}
	payload := string(encoded)
	for _, forbidden := range []string{
		rawArgSecret, beforeSecret, oscSecret,
		"RawArgs", "BeforeContent", "StructuredContent", "ResultDisplay",
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("render model exposed %q: %s", forbidden, payload)
		}
	}
}

func TestToolRenderModelUsesBlockIdentityForDuplicateToolNames(t *testing.T) {
	projection := ecosystem.ToolProjection{
		Operation: "read_file",
		Transport: ecosystem.TransportSucceeded,
		Domain:    ecosystem.DomainSucceeded,
	}.Normalize()
	first, err := ToolRenderModelFromEntry(
		ChatEntry{BlockID: "block-read-1", Revision: 1},
		ToolEntry{ID: "call-read-1", Name: "read_file", Result: "first", Status: ToolStatusDone, Projection: projection},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ToolRenderModelFromEntry(
		ChatEntry{BlockID: "block-read-2", Revision: 1},
		ToolEntry{ID: "call-read-2", Name: "read_file", Result: "second", Status: ToolStatusDone, Projection: projection},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.BlockID == second.BlockID || first.InvocationID == second.InvocationID ||
		first.Preview.Result != "first" || second.Preview.Result != "second" {
		t.Fatalf("duplicate-name projections lost identity: %#v %#v", first, second)
	}
}

func TestToolRenderModelReprojectsPrivateToolArgumentsFromTypedRoute(t *testing.T) {
	const secret = "PRIVATE_GATEWAY_ARGUMENT_MUST_NOT_CROSS"
	projection := ecosystem.ToolProjection{
		Specialist: "cortex",
		Operation:  "cortex_status",
		Role:       ecosystem.RoleCoordination,
		Transport:  ecosystem.TransportRunning,
		Domain:     ecosystem.DomainPending,
		Route: ecosystem.ToolRoute{
			Gateway: "mcphub",
			Server:  "cortex",
			Tool:    "cortex_status",
			CallID:  "stored-1",
			Lazy:    true,
		},
	}.Normalize()
	model, err := ToolRenderModelFromEntry(
		ChatEntry{BlockID: "block-private-route", Revision: 1},
		ToolEntry{
			ID: "call-private-route", Name: "mcphub__mcphub_call_tool",
			Args:   `server=cortex tool=cortex_status arguments={"token":"` + secret + `"}`,
			Status: ToolStatusRunning, Projection: projection,
		},
	)
	if err != nil {
		t.Fatalf("project private route: %v", err)
	}
	if strings.Contains(model.Preview.Arguments, secret) {
		t.Fatalf("private argument crossed render boundary: %q", model.Preview.Arguments)
	}
	for _, want := range []string{"cortex", "cortex_status"} {
		if !strings.Contains(model.Preview.Arguments, want) {
			t.Fatalf("bounded route omitted %q: %q", want, model.Preview.Arguments)
		}
	}
}

func TestToolRenderModelDerivesThemeWithoutMutableCardState(t *testing.T) {
	previousNoColor := noColor
	noColor = false
	t.Cleanup(func() { noColor = previousNoColor })

	entry := ToolEntry{
		ID: "call-theme-1", Name: "read_file", Status: ToolStatusDone,
		Projection: ecosystem.ToolProjection{
			Operation: "read_file",
			Transport: ecosystem.TransportSucceeded,
			Domain:    ecosystem.DomainSucceeded,
		}.Normalize(),
	}
	model, err := ToolRenderModelFromEntry(
		ChatEntry{BlockID: "block-theme-1", Revision: 1},
		entry,
	)
	if err != nil {
		t.Fatal(err)
	}
	light, err := ToolCardFromRenderModel(model, false, defaultThemeID)
	if err != nil {
		t.Fatal(err)
	}
	dark, err := ToolCardFromRenderModel(model, true, defaultThemeID)
	if err != nil {
		t.Fatal(err)
	}
	if light.ID != dark.ID || light.State != dark.State || light.Projection.Operation != dark.Projection.Operation {
		t.Fatalf("theme changed semantic identity: light=%#v dark=%#v", light, dark)
	}
	lightR, lightG, lightB, lightA := light.Styles.TitleRunning.GetForeground().RGBA()
	darkR, darkG, darkB, darkA := dark.Styles.TitleRunning.GetForeground().RGBA()
	if lightR == darkR && lightG == darkG && lightB == darkB && lightA == darkA {
		t.Fatal("theme did not derive fresh card styles")
	}
}

func TestToolViewModelAdaptersFailClosedOnInvalidIdentity(t *testing.T) {
	entry := ToolEntry{ID: "call\nspoof", Name: "read_file", Status: ToolStatusDone}
	if _, err := ToolViewModelFromToolEntry(ChatEntry{BlockID: "block-1", Revision: 1}, entry); err == nil {
		t.Fatal("unsafe invocation identity was accepted")
	}

	card := NewToolCard("read_file", ToolCardFile, true, defaultThemeID)
	card.ID = "call-1"
	card.State = ToolCardSuccess
	if _, err := ToolViewModelFromToolCard(ChatEntry{BlockID: "bad block", Revision: 1}, card); err == nil {
		t.Fatal("unsafe transcript identity was accepted")
	}
	if _, err := ToolViewModelFromToolCard(ChatEntry{BlockID: "block-1"}, card); err == nil {
		t.Fatal("zero semantic revision was accepted")
	}
}

func TestToolViewModelKeepsTransportDomainAndEvidenceIndependent(t *testing.T) {
	card := NewToolCard("remote_tool", ToolCardGeneric, true, defaultThemeID)
	card.ID = "call-1"
	card.State = ToolCardAttention
	card.Projection = ecosystem.ToolProjection{
		Specialist: "remote",
		Operation:  "inspect",
		Transport:  ecosystem.TransportSucceeded,
		Domain:     ecosystem.DomainUnknown,
		Evidence:   ecosystem.EvidenceNone,
	}

	view, err := ToolViewModelFromToolCard(ChatEntry{BlockID: "block-1", Revision: 1}, card)
	if err != nil {
		t.Fatalf("project tool card: %v", err)
	}
	if view.Lifecycle != ToolLifecycleAttention || view.Transport != ecosystem.TransportSucceeded ||
		view.Domain != ecosystem.DomainUnknown || view.Evidence != ecosystem.EvidenceNone {
		t.Fatalf("transport success became domain success: %#v", view)
	}
}

func TestToolViewModelRejectsTamperedArtifactReference(t *testing.T) {
	view := ToolViewModel{
		InvocationID: "call-1",
		BlockID:      "block-1",
		Kind:         ToolKindGeneric,
		Operation:    "fcheap_save",
		Target:       "fcheap",
		Lifecycle:    ToolLifecycleSucceeded,
		Transport:    ecosystem.TransportSucceeded,
		Domain:       ecosystem.DomainSucceeded,
		Evidence:     ecosystem.EvidenceSupported,
		Revision:     1,
		Artifact: &ecosystem.ArtifactDigest{
			Kind:          ecosystem.ArtifactDigestFileCheapStash,
			ID:            "../private",
			URI:           "file:///private",
			SchemaVersion: "1.0",
			ContentSHA256: strings.Repeat("a", 64),
			FileCount:     1,
			TotalSize:     1,
			CreatedAt:     "2026-07-17T12:00:00Z",
		},
	}
	if err := view.Validate(); err == nil {
		t.Fatal("tampered artifact reference was accepted")
	}
}

func TestToolViewModelRejectsLifecycleProjectionContradictions(t *testing.T) {
	failedProjection := ecosystem.ToolProjection{
		Operation: "read_file",
		Transport: ecosystem.TransportFailed,
		Domain:    ecosystem.DomainUnknown,
		Evidence:  ecosystem.EvidenceNone,
	}.Normalize()
	running := ToolEntry{
		ID: "call-1", Name: "read_file", Status: ToolStatusRunning,
		Projection: failedProjection,
	}
	if _, err := ToolViewModelFromToolEntry(
		ChatEntry{BlockID: "block-1", Revision: 1},
		running,
	); err == nil || !strings.Contains(err.Error(), "terminal semantic projection") {
		t.Fatalf("running/failed entry contradiction error = %v", err)
	}

	view := ToolViewModel{
		InvocationID: "call-1",
		BlockID:      "block-1",
		Kind:         ToolKindFile,
		Operation:    "read_file",
		Lifecycle:    ToolLifecycleRunning,
		Transport:    ecosystem.TransportFailed,
		Domain:       ecosystem.DomainUnknown,
		Evidence:     ecosystem.EvidenceNone,
		Revision:     1,
	}
	if err := view.Validate(); err == nil || !strings.Contains(err.Error(), "contradicts") {
		t.Fatalf("direct lifecycle/projection contradiction error = %v", err)
	}
}

func TestToolViewModelLongLegacyOperationRemainsSelfValidating(t *testing.T) {
	entry := ToolEntry{
		ID:     "call-1",
		Name:   strings.Repeat("very_long_operation_", 20),
		Status: ToolStatusDone,
	}
	view, err := ToolViewModelFromToolEntry(
		ChatEntry{BlockID: "block-1", Revision: 1},
		entry,
	)
	if err != nil {
		t.Fatalf("long legacy operation projection: %v", err)
	}
	if view.Operation == "" || lipgloss.Width(view.Operation) > maxToolViewOperationCells ||
		len(view.Operation) > maxToolViewOperationBytes {
		t.Fatalf("bounded operation = %q (%d cells, %d bytes)",
			view.Operation, lipgloss.Width(view.Operation), len(view.Operation))
	}
	if err := view.Validate(); err != nil {
		t.Fatalf("constructor produced a view model that rejects itself: %v", err)
	}
}

func TestProjectToolHeaderCellBudgetProperties(t *testing.T) {
	const (
		glyphCells    = 3
		nameCells     = 18
		summaryCells  = 80
		durationCells = 8
	)
	sawDuration := false
	sawSummaryWithoutDuration := false
	for width := 4; width <= 200; width++ {
		inner := width - 2
		effectiveGlyph := min(glyphCells, inner)
		budget := projectToolHeaderCellBudget(inner, effectiveGlyph, nameCells, summaryCells, durationCells, true)
		if budget.NameCells < 0 || budget.SummaryCells < 0 {
			t.Fatalf("width %d produced a negative budget: %#v", width, budget)
		}
		const summarySepCells = 1 // single space between verb and object
		used := effectiveGlyph
		if budget.NameCells > 0 {
			used += 1 + budget.NameCells
		}
		if budget.SummaryCells > 0 {
			used += summarySepCells + budget.SummaryCells
		}
		if budget.ShowDuration {
			used += 1 + durationCells
			sawDuration = true
		} else if budget.SummaryCells > 0 {
			sawSummaryWithoutDuration = true
		}
		if used > inner {
			t.Fatalf("width %d over-allocated %d cells into %d: %#v", width, used, inner, budget)
		}
		semanticCells := inner - effectiveGlyph - 1
		if budget.ShowDuration {
			semanticCells -= durationCells + 1
		}
		// Half is the base allocation. A short operation may donate cells that
		// would otherwise be empty, but the summary must not displace it.
		baseSummary := min(summaryCells, max(0, semanticCells/2))
		maxSummary := baseSummary
		if baseSummary > 0 {
			baseName := min(nameCells, max(0, semanticCells-baseSummary-summarySepCells))
			maxSummary += max(0, semanticCells-baseSummary-summarySepCells-baseName)
		}
		if budget.SummaryCells > maxSummary {
			t.Fatalf("width %d summary displaced semantic identity: %#v", width, budget)
		}
	}
	if !sawDuration || !sawSummaryWithoutDuration {
		t.Fatalf("degradation stages not exercised: duration=%v summary-without-duration=%v", sawDuration, sawSummaryWithoutDuration)
	}
}

func TestToolCardHeaderProjectionFitsEveryWidthAndDropsDurationFirst(t *testing.T) {
	card := NewToolCard("write_file", ToolCardFile, true, defaultThemeID)
	card.ID = "call-1"
	card.State = ToolCardSuccess
	card.SetSummary("write internal/ui/a-very-descriptive-file-name.go")
	card.Duration = 12*time.Second + 345*time.Millisecond

	sawSummaryWithoutDuration := false
	for width := 4; width <= 200; width++ {
		rendered := card.View(width)
		assertToolCardLinesFit(t, rendered, width)
		plain := stripANSIForToolViewTest(rendered)
		if strings.Contains(plain, "internal") && !strings.Contains(plain, "12.3s") {
			sawSummaryWithoutDuration = true
		}
	}
	if !sawSummaryWithoutDuration {
		t.Fatal("duration never yielded while semantic summary remained visible")
	}
}

func stripANSIForToolViewTest(value string) string {
	// Lip Gloss width already ignores SGR styling. Keeping this tiny helper
	// avoids coupling the projection property to a specific ANSI parser.
	var out strings.Builder
	inEscape := false
	for _, r := range value {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func TestToolCardHeaderPlannerUsesDisplayCellsNotBytes(t *testing.T) {
	card := NewToolCard("read_file", ToolCardFile, false, defaultThemeID)
	card.State = ToolCardSuccess
	card.SetSummary("部署🙂 résumé")
	card.Duration = 42 * time.Millisecond
	for width := 4; width <= 80; width++ {
		rendered := card.View(width)
		for _, line := range strings.Split(rendered, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d rendered %d cells: %q", width, got, line)
			}
		}
	}
}
