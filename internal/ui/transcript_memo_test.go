package ui

import (
	"strings"
	"testing"
	"time"
)

// mixedMemoTranscript installs one settled transcript covering every entry
// kind the per-entry memo must reproduce exactly.
func mixedMemoTranscript(m *Model) {
	m.entries = []ChatEntry{
		{Kind: "user", Content: "please run the build"},
		{Kind: "assistant", Content: "Running it now.", ThinkingContent: "the user wants a build", ThinkingCollapsed: true},
		{Kind: "tool_group", ToolIndex: 0},
		{Kind: "error", Content: "network unreachable"},
		{Kind: "system", Content: "Model · test-model"},
	}
	m.toolEntries = []ToolEntry{{
		ID:        "t1",
		Name:      "bash",
		Summary:   "task build",
		Args:      "cmd=task build",
		Result:    "ok",
		Status:    ToolStatusDone,
		Collapsed: true,
		Duration:  1500 * time.Millisecond,
	}}
}

func TestEntryMemoWarmRenderIsByteIdentical(t *testing.T) {
	m := newTestModel(t)
	m.ready = true
	// Short frame: sticky user chrome is off so every entry is body-memoized.
	m.height = 13
	mixedMemoTranscript(m)

	cold := m.renderEntries()
	coldTools := append([]toolHitRegion(nil), m.toolHitRegions...)
	coldThinking := append([]thinkingHitRegion(nil), m.thinkingHitRegions...)
	if len(m.entryMemo) != len(m.entries) {
		t.Fatalf("memo holds %d entries, want %d (every settled entry memoized)", len(m.entryMemo), len(m.entries))
	}

	// invalidateEntryCache forces a full walk but must keep the memo, so the
	// warm walk replays cached chunks and rebuilds identical hit regions.
	m.invalidateEntryCache()
	warm := m.renderEntries()
	if warm != cold {
		t.Fatalf("memo-warm render diverged from cold render:\n--- cold ---\n%s\n--- warm ---\n%s", cold, warm)
	}
	if len(coldTools) != len(m.toolHitRegions) {
		t.Fatalf("tool hit regions diverged: cold %d warm %d", len(coldTools), len(m.toolHitRegions))
	}
	for i := range coldTools {
		if coldTools[i] != m.toolHitRegions[i] {
			t.Fatalf("tool hit region %d diverged: cold %#v warm %#v", i, coldTools[i], m.toolHitRegions[i])
		}
	}
	if len(coldThinking) == 0 {
		t.Fatal("precondition: transcript should expose a completed thinking hit region")
	}
	if len(coldThinking) != len(m.thinkingHitRegions) {
		t.Fatalf("thinking hit regions diverged: cold %d warm %d", len(coldThinking), len(m.thinkingHitRegions))
	}
	for i := range coldThinking {
		if coldThinking[i] != m.thinkingHitRegions[i] {
			t.Fatalf("thinking hit region %d diverged: cold %#v warm %#v", i, coldThinking[i], m.thinkingHitRegions[i])
		}
	}

	// Prove the warm walk consumed the memo rather than re-rendering: a
	// tampered chunk under an unchanged key must surface verbatim.
	systemID := m.entries[4].BlockID
	memo := m.entryMemo[systemID]
	memo.chunk = strings.Replace(memo.chunk, "test-model", "memo-sentinel", 1)
	m.entryMemo[systemID] = memo
	m.invalidateEntryCache()
	if tampered := m.renderEntries(); !strings.Contains(tampered, "memo-sentinel") {
		t.Fatal("full walk re-rendered a settled entry instead of using its memo")
	}
}

func TestEntryMemoKeyIncludesReadableProseMeasure(t *testing.T) {
	m := newTestModel(t)
	m.entries = []ChatEntry{{
		BlockID:  "memo-prose",
		Revision: 1,
		Kind:     "user",
		Content:  strings.Repeat("readable prose ", 20),
	}}
	wide := m.entryMemoKey(0, 160, 96)
	narrow := m.entryMemoKey(0, 160, 72)
	if wide == "" || narrow == "" || wide == narrow {
		t.Fatalf("memo key ignored prose geometry: wide=%q narrow=%q", wide, narrow)
	}
}

func TestEntryMemoToolMutationChangesKeyAndRerenders(t *testing.T) {
	m := newTestModel(t)
	m.ready = true
	mixedMemoTranscript(m)

	collapsed := m.renderEntries()
	toolID := m.entries[2].BlockID
	before, ok := m.entryMemo[toolID]
	if !ok {
		t.Fatal("settled tool group was not memoized")
	}

	m.toolEntries[0].Collapsed = false
	m.invalidateEntryCache()
	expanded := m.renderEntries()
	afterCollapse, ok := m.entryMemo[toolID]
	if !ok {
		t.Fatal("expanded tool group was not memoized")
	}
	if afterCollapse.key == before.key {
		t.Fatal("toggling Collapsed did not change the tool group memo key")
	}
	if expanded == collapsed {
		t.Fatal("toggling Collapsed did not re-render the transcript")
	}

	// Terminal transcript lifecycles are monotonic, so exercise the error
	// variant on a fresh entry instead of rewriting a settled receipt.
	errorModel := newTestModel(t)
	errorModel.ready = true
	mixedMemoTranscript(errorModel)
	errorModel.toolEntries[0].Collapsed = false
	errorModel.toolEntries[0].Status = ToolStatusError
	errored := errorModel.renderEntries()
	afterStatus, ok := errorModel.entryMemo[errorModel.entries[2].BlockID]
	if !ok {
		t.Fatal("errored tool group was not memoized")
	}
	if afterStatus.key == afterCollapse.key {
		t.Fatal("changing Status did not change the tool group memo key")
	}
	if errored == expanded {
		t.Fatal("changing Status did not re-render the transcript")
	}
}

func TestEntryMemoCachesEventDrivenRunningToolAndClearsOnShrink(t *testing.T) {
	m := newTestModel(t)
	m.ready = true
	// Keep sticky off so user + assistant both remain body-memoized after shrink.
	m.height = 13
	mixedMemoTranscript(m)
	m.toolEntries[0].Status = ToolStatusRunning
	m.toolsPending = 1

	_ = m.renderEntries()
	toolID := m.entries[2].BlockID
	running, ok := m.entryMemo[toolID]
	if !ok {
		t.Fatal("event-driven running ToolCard was not memoized")
	}

	m.toolEntries[0].Status = ToolStatusDone
	m.toolsPending = 0
	m.invalidateEntryCache()
	_ = m.renderEntries()
	settled, ok := m.entryMemo[toolID]
	if !ok {
		t.Fatal("settled tool group was not memoized after the turn completed")
	}
	if settled.key == running.key {
		t.Fatal("running-to-settled lifecycle event did not change the memo key")
	}

	// A shrunken entries slice prunes only identities that no longer survive.
	m.entries = m.entries[:2]
	m.invalidateEntryCache()
	_ = m.renderEntries()
	if len(m.entryMemo) != 2 {
		t.Fatalf("memo holds %d entries after shrink, want 2", len(m.entryMemo))
	}
	if _, ok := m.entryMemo[toolID]; ok {
		t.Fatal("stale memo entry survived a transcript shrink")
	}
}
