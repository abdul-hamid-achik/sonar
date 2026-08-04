package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

func TestBuildCommandContextProjectsCompletedArtifactsInTranscriptOrder(t *testing.T) {
	m := newTestModel(t)
	providerSentinel := "/private/provider/path SUPER_SECRET_PROVIDER_PROSE"
	first := testArtifactProjection("stash-b", strings.Repeat("b", 64), true, false)
	first.Artifact.URI = "https://provider.invalid/hostile-uri"
	duplicate := testArtifactProjection("stash-b", strings.Repeat("b", 64), false, true)
	second := testArtifactProjection("stash-a", strings.Repeat("a", 64), false, true)
	second.Artifact.URI = "file:///provider/path"
	m.toolEntries = []ToolEntry{
		{ID: "done-b", Name: "fcheap_save", Status: ToolStatusDone, Args: providerSentinel, Result: providerSentinel, Projection: first},
		{ID: "running", Name: "fcheap_save", Status: ToolStatusRunning, Projection: testArtifactProjection("stash-running", strings.Repeat("c", 64), false, false)},
		{ID: "invalid", Name: "read_file", Status: ToolStatusDone, Projection: ecosystem.ProjectToolCall("read_file", nil)},
		{ID: "duplicate-b", Name: "fcheap_save", Status: ToolStatusDone, Projection: duplicate},
		{ID: "done-a", Name: "fcheap_save", Status: ToolStatusDone, Projection: second},
	}

	ctx := m.buildCommandContext()
	if len(ctx.Artifacts) != 2 {
		t.Fatalf("command artifacts = %d, want 2: %#v", len(ctx.Artifacts), ctx.Artifacts)
	}
	if got, want := ctx.Artifacts[0].URI, "fcheap://stash/stash-b"; got != want {
		t.Fatalf("first artifact URI = %q, want %q", got, want)
	}
	if got, want := ctx.Artifacts[1].URI, "fcheap://stash/stash-a"; got != want {
		t.Fatalf("second artifact URI = %q, want %q", got, want)
	}
	if !ctx.Artifacts[0].SecretsWarning || ctx.Artifacts[0].IndexingFailed {
		t.Fatalf("first occurrence did not win deduplication: %#v", ctx.Artifacts[0])
	}
	if !ctx.Artifacts[1].IndexingFailed {
		t.Fatalf("second artifact lost indexing flag: %#v", ctx.Artifacts[1])
	}

	result := m.cmdRegistry.Execute(ctx, "artifacts", nil)
	if result.Error != "" {
		t.Fatalf("/artifacts error = %q", result.Error)
	}
	if strings.Contains(result.Text, providerSentinel) || strings.Contains(result.Text, "provider.invalid") || strings.Contains(result.Text, "file:///provider") {
		t.Fatalf("/artifacts leaked provider-controlled content:\n%s", result.Text)
	}
}

func TestBuildCommandContextUsesCurrentAgentICESession(t *testing.T) {
	m := newTestModel(t)
	m.iceSessionID = "startup-scope"
	m.agent.SetExecutionSessionID(42, "")

	if got, want := m.buildCommandContext().ICESessionID, "db:42"; got != want {
		t.Fatalf("ICE session ID = %q, want %q", got, want)
	}
}

func TestArtifactsCommandSeesRestoredProjectionWithoutRawReceiptText(t *testing.T) {
	m := newTestModel(t)
	const providerSentinel = "/Users/private/source.env PROVIDER_SECRET_PROSE"
	projection := testArtifactProjection("restored-stash", strings.Repeat("d", 64), true, true)
	m.toolEntries = restoreToolEntries(persistToolEntries([]ToolEntry{{
		ID:         "saved-tool",
		Name:       "fcheap_save",
		Status:     ToolStatusDone,
		Args:       providerSentinel,
		Result:     providerSentinel,
		Projection: projection,
	}}))

	result := m.cmdRegistry.Execute(m.buildCommandContext(), "artifacts", nil)
	if result.Error != "" || !strings.Contains(result.Text, "fcheap://stash/restored-stash") {
		t.Fatalf("restored /artifacts = %#v", result)
	}
	if strings.Contains(result.Text, providerSentinel) || strings.Contains(result.Text, "/Users/private") {
		t.Fatalf("restored /artifacts leaked persisted receipt text:\n%s", result.Text)
	}
}

func TestCommandArtifactInfosIsBounded(t *testing.T) {
	entries := make([]ToolEntry, 0, command.MaxContextArtifacts+1)
	for i := 0; i < command.MaxContextArtifacts+1; i++ {
		entries = append(entries, ToolEntry{
			ID:         fmt.Sprintf("tool-%d", i),
			Name:       "fcheap_save",
			Status:     ToolStatusDone,
			Projection: testArtifactProjection(fmt.Sprintf("stash-%d", i), strings.Repeat("e", 64), false, false),
		})
	}
	artifacts, truncated := commandArtifactInfos(entries)
	if len(artifacts) != command.MaxContextArtifacts {
		t.Fatalf("bounded artifacts = %d, want %d", len(artifacts), command.MaxContextArtifacts)
	}
	if !truncated {
		t.Fatal("bounded artifact projection did not report omitted receipts")
	}
	if got, want := artifacts[0].URI, "fcheap://stash/stash-0"; got != want {
		t.Fatalf("first bounded artifact = %q, want %q", got, want)
	}
	if got, want := artifacts[len(artifacts)-1].URI, fmt.Sprintf("fcheap://stash/stash-%d", command.MaxContextArtifacts-1); got != want {
		t.Fatalf("last bounded artifact = %q, want %q", got, want)
	}
}

func TestArtifactsCommandIsInheritedByHelpAndCompletion(t *testing.T) {
	m := newTestModel(t)
	help := ansi.Strip(m.buildHelpContent(m.helpContentWidth()))
	if !strings.Contains(help, "/artifacts") {
		t.Fatalf("help omitted /artifacts:\n%s", help)
	}
	completions := m.completer.Complete("/art")
	if len(completions) != 1 || completions[0].Label != "/artifacts" || completions[0].Insert != "/artifacts " {
		t.Fatalf("/art completion = %#v", completions)
	}
}

func testArtifactProjection(id, sha string, secretsWarning, indexingFailed bool) ecosystem.ToolProjection {
	return ecosystem.ToolProjection{
		Specialist: "filecheap",
		Operation:  "fcheap_save",
		Role:       ecosystem.RoleArtifact,
		Transport:  ecosystem.TransportSucceeded,
		Domain:     ecosystem.DomainSucceeded,
		Evidence:   ecosystem.EvidenceSupported,
		Artifact: &ecosystem.ArtifactDigest{
			Kind:           ecosystem.ArtifactDigestFileCheapStash,
			ID:             id,
			URI:            "https://provider.invalid/ignored",
			SchemaVersion:  "1.0",
			ContentSHA256:  sha,
			FileCount:      2,
			TotalSize:      512,
			CreatedAt:      "2026-07-13T12:30:00Z",
			SecretsWarning: secretsWarning,
			IndexingFailed: indexingFailed,
		},
	}.Normalize()
}
