package ui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestWarmStaticRunningToolRenderDoesNotRehashOrRepublishStableHistory(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.ready = true
	m.now = func() time.Time {
		return time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	}

	const historyEntries = 320
	for index := 0; index < historyEntries; index++ {
		kind := "assistant"
		if index%2 == 0 {
			kind = "user"
		}
		m.entries = append(m.entries, ChatEntry{
			Kind: kind,
			Content: fmt.Sprintf(
				"history-%03d %s",
				index,
				strings.Repeat("semantic transcript payload ", 12),
			),
		})
	}
	m.toolEntries = []ToolEntry{{
		ID: "live-tool", Name: "bash", Status: ToolStatusRunning,
	}}
	m.entries = append(m.entries, ChatEntry{Kind: "tool_group", ToolIndex: 0})
	m.toolsPending = 1

	cold := m.renderEntries()
	if m.cachedStableCount != len(m.entries) {
		t.Fatalf("stable prefix = %d, want every event-driven entry %d", m.cachedStableCount, len(m.entries))
	}
	if len(m.transcriptLayout.Records) != len(m.entries) {
		t.Fatalf("layout records = %d, want %d", len(m.transcriptLayout.Records), len(m.entries))
	}
	if len(m.transcriptLayout.Records[0].LineMap) == 0 {
		t.Fatal("precondition: first stable block has no semantic line map")
	}
	recordsAddress := &m.transcriptLayout.Records[0]
	lineMapAddress := &m.transcriptLayout.Records[0].LineMap[0]

	probe := &transcriptRenderProbe{}
	m.transcriptRenderProbe = probe
	warm := m.renderEntries()

	if warm != cold {
		t.Fatalf("warm live-tool paint diverged from cold paint")
	}
	if probe.semanticDigestCalls != 0 {
		t.Fatalf("warm paint rehashed %d semantic entries, want 0", probe.semanticDigestCalls)
	}
	if probe.layoutRecordsMaterialized != 0 {
		t.Fatalf("warm paint materialized %d layout records, want 0", probe.layoutRecordsMaterialized)
	}
	// The immutable transcript is shared. Closing the cached layout needs to
	// compare only its final record; the running ToolCard itself is memoized.
	if probe.layoutRecordComparisons > 1 {
		t.Fatalf("warm paint compared %d layout records, want at most 1", probe.layoutRecordComparisons)
	}
	if recordsAddress != &m.transcriptLayout.Records[0] {
		t.Fatal("warm paint replaced the immutable transcript snapshot")
	}
	if lineMapAddress != &m.transcriptLayout.Records[0].LineMap[0] {
		t.Fatal("warm paint cloned the stable prefix LineMap")
	}
}

func TestActivityClocksDoNotPaintTenThousandEntryTranscript(t *testing.T) {
	// A running receipt animates with the footer rather than sitting behind a
	// frozen glyph, and it does so without paying for the transcript behind it.
	// The fixture is ten thousand entries because that is the number the naive
	// implementation — invalidate the entry cache, refresh — cannot survive: it
	// rebuilds every block's memo key on each of the spinner's twelve ticks a
	// second. The frame path swaps content into blocks already measured.
	t.Run("spinner tick animates the running receipt within viewport cost", func(t *testing.T) {
		m := largeRunningToolTranscript(t, "read_file", false)
		beforeTranscript := m.viewport.GetContent()
		beforeFooter := m.renderWorkingLine()
		probe := &transcriptRenderProbe{}
		m.transcriptRenderProbe = probe

		// A beat, not a tick: the pulse divides the activity clock down to a
		// ping a second, and the ticks in between deliberately repaint nothing.
		var next tea.Cmd
		for range sonarPulseTickDivider {
			var updated tea.Model
			updated, next = m.Update(m.spin.Tick())
			m = updated.(*Model)
		}
		if next == nil {
			t.Fatal("active tool spinner did not continue its footer clock")
			return
		}
		assertViewportBoundedRestage(t, probe, m.viewport.Height(), "activity beat")
		if after := m.viewport.GetContent(); after == beforeTranscript {
			t.Fatal("spinner tick left the running receipt on a frozen frame")
		}
		if afterFooter := m.renderWorkingLine(); afterFooter == beforeFooter {
			t.Fatal("spinner tick did not advance the footer-owned activity")
		}
		// One glyph moved. A frame that changes the receipt's height would
		// desynchronize every startRow below it, so the height is the invariant.
		if before, after := strings.Count(beforeTranscript, "\n"), strings.Count(m.viewport.GetContent(), "\n"); before != after {
			t.Fatalf("activity frame changed transcript height: %d -> %d", before, after)
		}

		probe = &transcriptRenderProbe{}
		m.transcriptRenderProbe = probe
		settled, _ := m.Update(ToolCallResultMsg{
			ID: "active-tool", Name: "read_file", Result: "ok", Duration: 2 * time.Second,
		})
		m = settled.(*Model)
		assertTranscriptPainted(t, probe, "tool result")
		if m.toolEntries[0].Status != ToolStatusDone {
			t.Fatalf("tool result left status %v", m.toolEntries[0].Status)
		}
		if after := m.viewport.GetContent(); after == beforeTranscript {
			t.Fatal("real tool result did not update transcript content")
		}
	})

	t.Run("reduced-motion heartbeat is footer-only and tool result repaints", func(t *testing.T) {
		m := largeRunningToolTranscript(t, "read_file", true)
		beforeTranscript := m.viewport.GetContent()
		if cmd := m.startActivityCmd(); cmd == nil || !m.activityHeartbeatPending {
			t.Fatal("reduced-motion tool did not start its informational heartbeat")
		}
		token := m.activityHeartbeatToken
		probe := &transcriptRenderProbe{}
		m.transcriptRenderProbe = probe

		updated, next := m.Update(activityHeartbeatMsg{Token: token})
		m = updated.(*Model)
		if next == nil || !m.activityHeartbeatPending {
			t.Fatal("active reduced-motion heartbeat did not continue")
		}
		assertNoTranscriptPaint(t, probe, "reduced-motion heartbeat")
		if after := m.viewport.GetContent(); after != beforeTranscript {
			t.Fatal("reduced-motion heartbeat changed transcript content")
		}

		probe = &transcriptRenderProbe{}
		m.transcriptRenderProbe = probe
		updated, _ = m.Update(ToolCallResultMsg{
			ID: "active-tool", Name: "read_file", Result: "ok", Duration: 2 * time.Second,
		})
		m = updated.(*Model)
		assertTranscriptPainted(t, probe, "tool result")
		if m.toolEntries[0].Status != ToolStatusDone {
			t.Fatalf("tool result left status %v", m.toolEntries[0].Status)
		}
		if after := m.viewport.GetContent(); after == beforeTranscript {
			t.Fatal("real tool result did not update transcript content")
		}
	})
}

func largeRunningToolTranscript(t *testing.T, toolName string, reducedMotion bool) *Model {
	t.Helper()
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base.Add(2500 * time.Millisecond) }
	m.state = StateStreaming
	m.reducedMotion = reducedMotion

	const historyEntries = 10_000
	m.entries = make([]ChatEntry, 0, historyEntries+1)
	for index := 0; index < historyEntries; index++ {
		m.entries = append(m.entries, ChatEntry{
			BlockID:   BlockID(fmt.Sprintf("history_%05d", index)),
			TurnID:    TurnID(fmt.Sprintf("turn_history_%05d", index)),
			Revision:  1,
			Lifecycle: BlockSettled,
			Kind:      "user",
			Content:   "history",
		})
	}
	m.toolEntries = []ToolEntry{{
		ID: "active-tool", Name: toolName, Summary: "internal/ui/model.go",
		Status: ToolStatusRunning, StartTime: base, Collapsed: true,
	}}
	m.entries = append(m.entries, ChatEntry{
		BlockID:   "active_tool_block",
		TurnID:    "active_tool_turn",
		Revision:  1,
		Lifecycle: BlockLive,
		Kind:      "tool_group",
		ToolIndex: 0,
	})
	m.toolsPending = 1
	m.invalidateEntryCache()
	m.refreshTranscript()
	m.transcriptGotoBottom()
	return m
}

// assertViewportBoundedRestage is the invariant an activity frame must hold.
//
// It is deliberately weaker than assertNoTranscriptPaint in exactly one place:
// a running receipt animates, so the visible window is restaged. Everything
// whose cost grows with the transcript stays at zero — no document rebuild, no
// re-measure, no semantic rehash, no layout record touched — which is what the
// ten-thousand-entry fixture is here to measure. Restaging is bounded by the
// viewport, and the row count proves it.
func assertViewportBoundedRestage(t *testing.T, probe *transcriptRenderProbe, viewportRows int, operation string) {
	t.Helper()
	if probe.renderEntriesCalls != 0 ||
		probe.transcriptBytesMaterialized != 0 ||
		probe.documentBuilds != 0 ||
		probe.measureBytesMaterialized != 0 ||
		probe.lineIndexRowsBuilt != 0 ||
		probe.semanticDigestCalls != 0 ||
		probe.layoutRecordsMaterialized != 0 ||
		probe.layoutRecordComparisons != 0 ||
		probe.blocksMeasured != 0 {
		t.Fatalf("%s performed transcript-length work: %#v", operation, probe)
	}
	if probe.paintRowsStaged == 0 {
		t.Fatalf("%s did not restage the visible window: %#v", operation, probe)
	}
	if limit := viewportRows + 2*transcriptPaintOverscanRows; probe.paintRowsStaged > limit {
		t.Fatalf("%s staged %d rows, want at most the viewport plus overscan (%d)",
			operation, probe.paintRowsStaged, limit)
	}
}

func assertNoTranscriptPaint(t *testing.T, probe *transcriptRenderProbe, operation string) {
	t.Helper()
	if probe.renderEntriesCalls != 0 ||
		probe.transcriptBytesMaterialized != 0 ||
		probe.documentBuilds != 0 ||
		probe.measureBytesMaterialized != 0 ||
		probe.lineIndexRowsBuilt != 0 ||
		probe.semanticDigestCalls != 0 ||
		probe.layoutRecordsMaterialized != 0 ||
		probe.layoutRecordComparisons != 0 ||
		probe.blocksMeasured != 0 ||
		probe.blocksPainted != 0 ||
		probe.paintRowsStaged != 0 ||
		probe.paintBytesStaged != 0 ||
		probe.viewportRowsStaged != 0 {
		t.Fatalf("%s performed transcript work: %#v", operation, probe)
	}
}

func assertTranscriptPainted(t *testing.T, probe *transcriptRenderProbe, operation string) {
	t.Helper()
	if probe.documentBuilds == 0 || probe.paintRowsStaged == 0 {
		t.Fatalf("%s did not paint the transcript: %#v", operation, probe)
	}
}

func TestPausedStreamingTailKeepsTransientBlockAcrossResizeAndTheme(t *testing.T) {
	m := newTestModel(t)
	m.height = 13 // sticky off: layout/search fixtures need every entry painted
	m.handleWindowSize(tea.WindowSizeMsg{Width: 82, Height: 14}, nil)
	m.entries = []ChatEntry{{Kind: "user", Content: "stream a detailed response"}}
	m.state = StateStreaming
	for index := 0; index < 70; index++ {
		fmt.Fprintf(
			&m.streamBuf,
			"stream row %02d keeps a semantic coordinate while the terminal reflows this long line\n",
			index,
		)
	}
	m.invalidateEntryCache()
	m.refreshTranscript()

	if got, want := len(m.transcriptLayout.Records), len(m.entries)+1; got != want {
		t.Fatalf("live layout records = %d, want %d (one transient live-tail block)", got, want)
	}
	live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
	if !strings.HasPrefix(string(live.BlockID), "transient_live_") || live.TurnID == "" {
		t.Fatalf("live-tail identity = block %q turn %q", live.BlockID, live.TurnID)
	}
	if len(live.LineMap) < 12 {
		t.Fatalf("live-tail LineMap has %d rows, want enough to pause inside it", len(live.LineMap))
	}

	m.setTranscriptYOffset(live.StartRow + 8)
	m.pauseFollow()
	resizeCapture := m.captureTranscriptReflowAnchor()
	if resizeCapture.Intent.Manual.BlockID != live.BlockID {
		t.Fatalf("resize anchor block = %q, want live tail %q", resizeCapture.Intent.Manual.BlockID, live.BlockID)
	}

	m.handleWindowSize(tea.WindowSizeMsg{Width: 47, Height: 14}, nil)
	assertCapturedTranscriptPosition(t, m, resizeCapture, live.BlockID, "width resize")

	current := transcriptLayoutRecordByID(t, m.transcriptLayout, live.BlockID)
	m.setTranscriptYOffset(current.StartRow + 6)
	m.pauseFollow()
	themeCapture := m.captureTranscriptReflowAnchor()
	if themeCapture.Intent.Manual.BlockID != live.BlockID {
		t.Fatalf("theme anchor block = %q, want live tail %q", themeCapture.Intent.Manual.BlockID, live.BlockID)
	}

	var background color.Color = color.White
	if !m.isDark {
		background = color.Black
	}
	m.handleThemeChange(tea.BackgroundColorMsg{Color: background})
	assertCapturedTranscriptPosition(t, m, themeCapture, live.BlockID, "theme change")
}

func TestLiveTailIdentityAvoidsPersistedCollisionAndEmptyTurnReuse(t *testing.T) {
	t.Run("persisted collision", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.ready = true
		const turnID = TurnID("turn_adversarial")
		collision := liveTailLayoutCandidate(0, turnID, 1, 0)
		m.entries = []ChatEntry{{
			BlockID:   collision,
			TurnID:    turnID,
			Revision:  1,
			Lifecycle: BlockSettled,
			Kind:      "user",
			Content:   "persisted opaque identity",
		}}
		m.state = StateStreaming
		m.streamBuf.WriteString("live response")

		m.refreshTranscript()
		live := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1]
		if live.BlockID == collision {
			t.Fatalf("transient live block reused persisted ID %q", collision)
		}
		if want := liveTailLayoutCandidate(0, turnID, 1, 1); live.BlockID != want {
			t.Fatalf("collision probe selected %q, want deterministic next candidate %q", live.BlockID, want)
		}
		if _, _, err := indexCurrentTranscriptLayout(m.transcriptLayout.Records); err != nil {
			t.Fatalf("collision-probed layout is invalid: %v", err)
		}
	})

	t.Run("empty turn starts a new episode", func(t *testing.T) {
		m := newTestModel(t)
		m.height = 13 // sticky off: layout/search fixtures need every entry painted
		m.ready = true
		m.state = StateStreaming
		m.streamBuf.WriteString("first unowned live tail")
		_ = m.renderEntries()
		first := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1].BlockID

		m.streamBuf.Reset()
		m.state = StateIdle
		_ = m.renderEntries()
		if len(m.transcriptLayout.Records) != 0 {
			t.Fatalf("settled empty transcript retained layout: %#v", m.transcriptLayout.Records)
		}

		m.state = StateStreaming
		m.streamBuf.WriteString("second unowned live tail")
		_ = m.renderEntries()
		second := m.transcriptLayout.Records[len(m.transcriptLayout.Records)-1].BlockID
		if first == second {
			t.Fatalf("empty-turn live identity %q was reused across episodes", first)
		}
	})
}

func assertCapturedTranscriptPosition(
	t *testing.T,
	m *Model,
	capture transcriptReflowAnchor,
	blockID BlockID,
	operation string,
) {
	t.Helper()
	resolution, err := ResolveTranscriptAnchor(
		capture.Intent,
		capture.Previous,
		m.transcriptLayout,
		max(1, m.viewport.Height()),
	)
	if err != nil {
		t.Fatalf("%s anchor resolution: %v", operation, err)
	}
	if resolution.BlockID != blockID {
		t.Fatalf("%s resolved block = %q, want %q", operation, resolution.BlockID, blockID)
	}
	if got := m.transcriptYOffset(); got != resolution.ViewportTop {
		t.Fatalf("%s viewport top = %d, semantic resolution = %d", operation, got, resolution.ViewportTop)
	}
	current := transcriptLayoutRecordByID(t, m.transcriptLayout, blockID)
	if top := m.transcriptYOffset(); top < current.StartRow || top >= current.StartRow+current.Height {
		t.Fatalf(
			"%s viewport top %d escaped live block rows [%d,%d)",
			operation,
			top,
			current.StartRow,
			current.StartRow+current.Height,
		)
	}
	if m.followPaused() == false {
		t.Fatalf("%s resumed follow while reader was paused in the stream", operation)
	}
}

func transcriptLayoutRecordByID(
	t *testing.T,
	snapshot TranscriptLayoutSnapshot,
	blockID BlockID,
) TranscriptLayoutRecord {
	t.Helper()
	for _, record := range snapshot.Records {
		if record.BlockID == blockID {
			return record
		}
	}
	t.Fatalf("layout block %q not found", blockID)
	return TranscriptLayoutRecord{}
}
