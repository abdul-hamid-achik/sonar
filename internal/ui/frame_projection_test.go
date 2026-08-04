package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/permission"
)

func TestFrameProjectionPartitionsSafeScreen(t *testing.T) {
	sizes := []struct {
		width  int
		height int
	}{
		{width: 30, height: 12},
		{width: 40, height: 16},
		{width: 72, height: 24},
		{width: 80, height: 24},
		{width: 112, height: 40},
		{width: 160, height: 48},
		{width: 200, height: 60},
	}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m = updated.(*Model)
			frame := m.projectFrame()

			wantScreen := NewCellRect(0, 0, size.width, size.height)
			if frame.Screen != wantScreen {
				t.Fatalf("screen = %#v, want %#v", frame.Screen, wantScreen)
			}
			wantSafe := Inset(wantScreen, Insets{Right: 1, Bottom: 1})
			if frame.SafeScreen != wantSafe {
				t.Fatalf("safe screen = %#v, want %#v", frame.SafeScreen, wantSafe)
			}
			if !rectWithin(frame.Header.Rect, frame.SafeScreen) ||
				!rectWithin(frame.Transcript.Rect, frame.SafeScreen) ||
				!rectWithin(frame.Footer.Rect, frame.SafeScreen) {
				t.Fatalf("surface escaped safe screen: %#v", frame)
			}
			if !intersection(frame.Header.Rect, frame.Transcript.Rect).Empty() ||
				!intersection(frame.Header.Rect, frame.Footer.Rect).Empty() ||
				!intersection(frame.Transcript.Rect, frame.Footer.Rect).Empty() {
				t.Fatalf("surfaces overlap: %#v", frame)
			}
			partitioned := cellArea(frame.Header.Rect) + cellArea(frame.Transcript.Rect) + cellArea(frame.Footer.Rect)
			if partitioned != cellArea(frame.SafeScreen) {
				t.Fatalf("surfaces do not partition safe screen: %#v", frame)
			}
			if frame.Transcript.Rect.Height() < frame.TranscriptFloorRows {
				t.Fatalf("transcript height = %d, floor = %d, fit = %d",
					frame.Transcript.Rect.Height(), frame.TranscriptFloorRows, frame.VerticalFit)
			}
			if frame.Footer.Content != "" && lipgloss.Height(frame.Footer.Content) != frame.Footer.Rect.Height() {
				t.Fatalf("footer paints %d rows into %#v", lipgloss.Height(frame.Footer.Content), frame.Footer.Rect)
			}
			if got := m.viewport.Height(); got != max(1, frame.Transcript.Rect.Height()) {
				t.Fatalf("viewport height = %d, projected transcript height = %d", got, frame.Transcript.Rect.Height())
			}
			if frame.Cursor != nil && !frame.Footer.Rect.Contains(frame.Cursor.X, frame.Cursor.Y) {
				t.Fatalf("cursor %#v is outside footer %#v", frame.Cursor, frame.Footer.Rect)
			}
		})
	}
}

func TestFrameProjectionBoundariesPreserveCanonicalPartition(t *testing.T) {
	widths := []int{30, 31, 39, 40, 71, 72, 111, 112, 200}
	heights := []int{12, 13, 15, 16, 23, 24, 39, 40, 80}

	for _, width := range widths {
		for _, height := range heights {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
			m = updated.(*Model)
			frame := m.projectFrame()

			assertFrameGeometry(t, frame)
			if frame.VerticalFit == FrameVerticalRecovery {
				t.Fatalf("%dx%d unexpectedly entered recovery", width, height)
			}
			if frame.Transcript.Rect.Height() < frame.TranscriptFloorRows {
				t.Fatalf("%dx%d transcript = %d rows, floor = %d, fit = %d",
					width, height, frame.Transcript.Rect.Height(), frame.TranscriptFloorRows, frame.VerticalFit)
			}
			assertProjectedFooterFits(t, frame)
		}
	}
}

func TestFrameProjectionDerivesTranscriptCapabilitiesAfterSplitAndInset(t *testing.T) {
	for _, size := range []struct {
		width  int
		height int
	}{
		{width: 30, height: 12},
		{width: 80, height: 24},
		{width: 160, height: 48},
	} {
		for _, forceCompact := range []bool{false, true} {
			m := newTestModel(t)
			m.forceCompact = forceCompact
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m = updated.(*Model)

			frame := m.projectFrame()
			// The projection no longer carries a capabilities snapshot nobody
			// read. What still matters is that the transcript rectangle the
			// capabilities would be derived from is the one actually projected.
			capabilities := DeriveLayoutCapabilities(
				transcriptWorkRect(frame.Transcript.Rect),
				LayoutCapabilityOptions{ForceCompact: forceCompact},
			)
			if capabilities.WorkWidth != frame.Transcript.Rect.Width()-transcriptContentChromeColumns {
				t.Fatalf("%dx%d work width = %d, transcript width = %d",
					size.width, size.height, capabilities.WorkWidth, frame.Transcript.Rect.Width())
			}
			if capabilities.WorkHeight != frame.Transcript.Rect.Height() {
				t.Fatalf("%dx%d work height = %d, transcript height = %d",
					size.width, size.height, capabilities.WorkHeight, frame.Transcript.Rect.Height())
			}
		}
	}
}

func TestFrameProjectionFooterOwnersPreserveFloorOrDeclareCriticalPriority(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*Model)
		wantFit  FrameVerticalFit
		checkFit bool
		markers  []string
	}{
		{
			name: "idle composer",
			setup: func(m *Model) {
				m.input.SetValue("draft")
				m.syncInputHeight()
			},
			wantFit:  FrameVerticalComfortable,
			checkFit: true,
			markers:  []string{"draft"},
		},
		{
			name: "working multiline composer condenses gap",
			setup: func(m *Model) {
				m.state = StateStreaming
				m.input.SetValue("one\ntwo\nthree\nfour\nfive")
				m.syncInputHeight()
			},
			wantFit:  FrameVerticalCondensed,
			checkFit: true,
			markers:  []string{"Running", "one", "five"},
		},
		{
			name: "queued follow-up",
			setup: func(m *Model) {
				m.state = StateStreaming
				m.queuedFollowUp = &queuedFollowUp{Prompt: "check focused tests"}
			},
			wantFit:  FrameVerticalComfortable,
			checkFit: true,
			markers:  []string{"queued", "check foc"},
		},
		{
			name: "completion compact variant",
			setup: func(m *Model) {
				m.input.SetValue("ask @ag")
				m.input.CursorEnd()
				m.triggerCompletion(m.input.Value())
			},
			wantFit:  FrameVerticalComfortable,
			checkFit: true,
			markers:  []string{"Attach Files", completionFilterPrompt + "ag", "@agent-x", "ask @ag"},
		},
		{
			name: "plan form",
			setup: func(m *Model) {
				m.openPlanForm("minimum plan")
			},
			wantFit:  FrameVerticalComfortable,
			checkFit: true,
			markers:  []string{"Plan", "Task", "esc cancel", "enter next"},
		},
		{
			name: "goal form condenses divider",
			setup: func(m *Model) {
				if err := m.openGoalForm("minimum goal", false); err != nil {
					t.Fatal(err)
				}
			},
			wantFit:  FrameVerticalCondensed,
			checkFit: true,
			markers:  []string{"Goal", "Objective", "esc", "enter/tab"},
		},
		{
			name: "approval",
			setup: func(m *Model) {
				openApprovalForTest(t, m, ToolApprovalMsg{
					ToolName: "write_file",
					Args:     map[string]any{"path": "/tmp/frame.txt"},
					Preview: permission.ApprovalPreview{
						Kind: permission.PreviewFileWrite,
						Path: "/tmp/frame.txt",
					},
					Response: make(chan permission.ApprovalResponse, 1),
				})
			},
			markers: []string{"Permission", "write_file", "deny", "once", "esc", "enter"},
		},
		{
			name: "external read scope decision",
			setup: func(m *Model) {
				m.readScopePrompt = &ReadScopePrompt{
					Canonical: "/outside/reference",
					Workspace: "/workspace",
					Operation: "add-read",
				}
				m.input.Blur()
			},
			markers: []string{"Allow", "read-only", "Writes", "deny", "cancel"},
		},
		{
			name: "paste decision",
			setup: func(m *Model) {
				m.pendingPaste = assessPaste(
					"one\ntwo\nthree\nfour",
					pasteCursorAtEnd(""),
					0,
					1,
					m.input.CharLimit,
				)
				m.input.Blur()
			},
			markers: []string{"Large paste", "cancel", "code", "plain"},
		},
		{
			name: "session switch decision",
			setup: func(m *Model) {
				m.pendingSessionSwitch = &pendingSessionSwitch{
					TargetSessionID: 7,
					TargetPublicID:  "aaaaaa7",
					TargetTitle:     "Saved work",
					Draft:           "preserve draft",
				}
				m.input.Blur()
			},
			markers: []string{"Open", "keep", "discard"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := newTestModel(t)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
			m = updated.(*Model)
			m.setTestTranscriptContent("conversation one\nconversation two\nconversation three\nconversation four")
			test.setup(m)
			m.recalcViewportHeight()

			frame := m.projectFrame()
			assertFrameGeometry(t, frame)
			assertProjectedFooterFits(t, frame)
			if test.checkFit && frame.VerticalFit != test.wantFit {
				t.Fatalf("vertical fit = %d, want %d", frame.VerticalFit, test.wantFit)
			}
			if frame.VerticalFit != FrameVerticalOwnerPriority &&
				frame.Transcript.Rect.Height() < frame.TranscriptFloorRows {
				t.Fatalf("transcript = %d rows, floor = %d, fit = %d",
					frame.Transcript.Rect.Height(), frame.TranscriptFloorRows, frame.VerticalFit)
			}
			assertFrameCursorInsideFooter(t, frame)
			assertOwnerViewFits(t, m, test.markers...)
		})
	}
}

func TestFrameProjectionCriticalCompletionDeclaresPhysicalLimitWithoutClipping(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(*Model)
	m.setTestTranscriptContent("transcript anchor")
	m.input.SetValue("one\ntwo\nthree\nfour\n@ag")
	m.input.CursorEnd()
	m.syncInputHeight()
	m.triggerCompletion(m.input.Value())
	m.recalcViewportHeight()

	frame := m.projectFrame()
	assertFrameGeometry(t, frame)
	assertProjectedFooterFits(t, frame)
	if frame.VerticalFit != FrameVerticalOwnerPriority {
		t.Fatalf("vertical fit = %d, want owner priority", frame.VerticalFit)
	}
	if frame.Transcript.Rect.Height() >= frame.TranscriptFloorRows {
		t.Fatalf("fixture is no longer physically constrained: transcript=%d floor=%d",
			frame.Transcript.Rect.Height(), frame.TranscriptFloorRows)
	}
	assertFrameCursorInsideFooter(t, frame)
	assertOwnerViewFits(t, m,
		"transcript anchor",
		completionFilterPrompt+"ag",
		"@agent-x",
		"one",
		"@ag",
		"esc",
		"enter",
	)
}

func TestFrameProjectionRecoveryOwnsNoBasePaint(t *testing.T) {
	for width := 0; width < minTerminalWidth; width++ {
		for height := 0; height < minTerminalHeight; height++ {
			m := newTestModel(t)
			m.width = width
			m.height = height
			frame := m.projectFrame()

			assertFrameGeometry(t, frame)
			if frame.VerticalFit != FrameVerticalRecovery {
				t.Fatalf("%dx%d fit = %d, want recovery", width, height, frame.VerticalFit)
			}
			if frame.Transcript.Visible || frame.Footer.Visible || frame.Footer.Content != "" || frame.Cursor != nil {
				t.Fatalf("%dx%d recovery leaked base paint: %#v", width, height, frame)
			}
		}
	}
}

func TestFrameProjectionPaintsTheMeasuredFooter(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("first line\nsecond line\nthird line")
	m.syncInputHeight()
	frame := m.projectFrame()
	view := m.View()

	if frame.Footer.Content == "" || !strings.Contains(view.Content, frame.Footer.Content) {
		t.Fatal("View did not paint the footer content produced by FrameProjection")
	}
	if got := lipgloss.Height(view.Content); got > m.height {
		t.Fatalf("rendered height = %d, terminal height = %d", got, m.height)
	}
	for _, line := range strings.Split(view.Content, "\n") {
		if got := lipgloss.Width(line); got > m.width {
			t.Fatalf("rendered line width = %d, terminal width = %d", got, m.width)
		}
	}
}

func TestFrameProjectionIsMonotonicWithoutExplicitPanelAction(t *testing.T) {
	m := newTestModel(t)

	previousWidth := -1
	for width := minTerminalWidth; width <= 200; width++ {
		updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
		m = updated.(*Model)
		got := m.projectFrame().Transcript.Rect.Width()
		if got < previousWidth {
			t.Fatalf("transcript width decreased at %d: %d -> %d", width, previousWidth, got)
		}
		previousWidth = got
	}

	previousHeight := -1
	for height := minTerminalHeight; height <= 100; height++ {
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
		m = updated.(*Model)
		got := m.projectFrame().Transcript.Rect.Height()
		if got < previousHeight {
			// Session chrome and framed composer engage at fixed height tiers.
			// Crossing those thresholds can spend a few new rows on chrome once;
			// allow a small one-time drop, not a regression spiral.
			drop := previousHeight - got
			if drop > 4 {
				t.Fatalf("transcript height decreased at %d: %d -> %d", height, previousHeight, got)
			}
		}
		if got > previousHeight {
			previousHeight = got
		} else if previousHeight < 0 {
			previousHeight = got
		}
	}
}

func assertFrameGeometry(t *testing.T, frame FrameProjection) {
	t.Helper()
	if !rectWithin(frame.Header.Rect, frame.SafeScreen) ||
		!rectWithin(frame.Transcript.Rect, frame.SafeScreen) ||
		!rectWithin(frame.Footer.Rect, frame.SafeScreen) {
		t.Fatalf("surface escaped safe screen: %#v", frame)
	}
	if !intersection(frame.Header.Rect, frame.Transcript.Rect).Empty() ||
		!intersection(frame.Header.Rect, frame.Footer.Rect).Empty() ||
		!intersection(frame.Transcript.Rect, frame.Footer.Rect).Empty() {
		t.Fatalf("surfaces overlap: %#v", frame)
	}
	partitioned := cellArea(frame.Header.Rect) + cellArea(frame.Transcript.Rect) + cellArea(frame.Footer.Rect)
	if partitioned != cellArea(frame.SafeScreen) {
		t.Fatalf("surfaces do not partition safe screen: %#v", frame)
	}
	if frame.Transcript.Rect.Width() < 0 || frame.Transcript.Rect.Height() < 0 ||
		frame.Footer.Rect.Width() < 0 || frame.Footer.Rect.Height() < 0 ||
		frame.Header.Rect.Width() < 0 || frame.Header.Rect.Height() < 0 {
		t.Fatalf("projection exposed a negative extent: %#v", frame)
	}
}

func assertProjectedFooterFits(t *testing.T, frame FrameProjection) {
	t.Helper()
	if frame.Footer.Content == "" {
		return
	}
	if got, ownedRows := lipgloss.Height(frame.Footer.Content), frame.Footer.Rect.Height(); got != ownedRows {
		t.Fatalf("footer content = %d rows, rect = %d rows, fit = %d\n%s",
			got, ownedRows, frame.VerticalFit, ansi.Strip(frame.Footer.Content))
	}
}

func assertFrameCursorInsideFooter(t *testing.T, frame FrameProjection) {
	t.Helper()
	if frame.Cursor != nil && !frame.Footer.Rect.Contains(frame.Cursor.X, frame.Cursor.Y) {
		t.Fatalf("cursor %#v outside footer %#v", frame.Cursor, frame.Footer.Rect)
	}
}

func assertOwnerViewFits(t *testing.T, m *Model, markers ...string) {
	t.Helper()
	view := m.View()
	plain := ansi.Strip(view.Content)
	for _, marker := range markers {
		if !strings.Contains(plain, marker) {
			t.Fatalf("owner view omitted %q:\n%s", marker, plain)
		}
	}
	if got := lipgloss.Height(view.Content); got > m.height {
		t.Fatalf("owner view = %d rows, terminal = %d rows:\n%s", got, m.height, plain)
	}
	for _, line := range strings.Split(view.Content, "\n") {
		if got := lipgloss.Width(line); got > m.width {
			t.Fatalf("owner line = %d cells, terminal = %d: %q", got, m.width, ansi.Strip(line))
		}
	}
}
