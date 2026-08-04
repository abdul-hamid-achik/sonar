package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// blankRowsLike returns whitespace with the same row count as value, so a
// surface can be emptied without changing the height the layout already
// reserved for it.
func blankRowsLike(value string) string {
	return strings.Repeat("\n", strings.Count(strings.TrimRight(value, "\n"), "\n"))
}

// renderComposerChrome paints the draft surface.
//
// The draft is unframed. A rounded box around the textarea cost two rows on
// every frame and drew a second rectangle inside a layout that already bounds
// the draft: the transcript rule sits directly above it and the shortcuts row
// directly below. The textarea's own "▏❯ " prompt lands the accent in column 1
// and the text in column 4, which is exactly the content grid every other
// surface uses, so no extra indent is needed either.
//
// Important: never mutate the live textarea width/height here — reflow during
// paint desyncs inputLines from the projected footer and overflows the frame.
func (m *Model) renderComposerChrome() (string, *tea.Cursor) {
	return m.renderComposerChromeBody(false)
}

// renderInertComposerChrome paints the composer at its exact live height with
// no content. Overlays use it so the draft surface keeps its allocation —
// reflowing the transcript when a modal opens is jarring — without leaking the
// draft under the panel or showing a prompt that looks focused while a modal
// owns input.
func (m *Model) renderInertComposerChrome() string {
	view, _ := m.renderComposerChromeBody(true)
	return view
}

func (m *Model) renderComposerChromeBody(inert bool) (string, *tea.Cursor) {
	if m == nil {
		return "", nil
	}
	input := m.input
	if m.state != StateIdle {
		input.Placeholder = "Write a follow-up · enter queue"
	}
	input.SetVirtualCursor(false)

	if inert {
		return blankRowsLike(input.View()), nil
	}
	return strings.TrimRight(input.View(), "\n"), input.Cursor()
}

// renderFooterIdentityRight is the bottom-right identity on the shortcuts row.
//
// It carries only what this frame assigned to surfaceShortcuts. On a roomy
// frame that is the authority badge alone: the model name lives in the top bar,
// beside the context meter it belongs with. On frames without a top bar the
// plan falls the model through to here instead.
func (m *Model) renderFooterIdentityRight(budget int) string {
	if m == nil || budget < 8 {
		return ""
	}
	plan := m.planStatus()
	parts := make([]string, 0, 2)

	if m.currentModelIsNonLocal() {
		if plan.owns(factRemoteBoundary, surfaceShortcuts) {
			parts = append(parts, m.styles.StatusWarning.Render(
				truncateDisplayWithGlyphProfile(
					m.currentModelReachabilityLabel(budget < 28), budget, m.glyphProfile,
				),
			))
		}
	} else if plan.owns(factModel, surfaceShortcuts) {
		if model := m.currentModelReachabilityLabel(budget < 28); model != "" {
			parts = append(parts, m.styles.Dimmed.Render(
				truncateDisplayWithGlyphProfile(model, min(22, budget), m.glyphProfile),
			))
		}
	}

	presented := m.presentedMode()
	// NORMAL is the default authority and needs no badge; PLAN and AUTO change
	// what the host will do and always earn their label here.
	if plan.owns(factMode, surfaceShortcuts) && presented != ModeNormal && budget >= 6 {
		cfg := m.modeConfigs[presented]
		var style lipgloss.Style
		switch presented {
		case ModePlan:
			style = m.styles.ModePlan
		case ModeAuto:
			style = m.styles.ModeBuild
		default:
			style = m.styles.ModeAsk
		}
		parts = append(parts, style.Render(cfg.Label))
	}
	if len(parts) == 0 {
		return ""
	}
	sep := m.styles.Dimmed.Render(glyphSeparator(m.glyphProfile))
	line := strings.Join(parts, sep)
	if lipgloss.Width(line) > budget {
		// Authority is last in and first kept: it changes what the host will do.
		if len(parts) > 1 {
			line = parts[len(parts)-1]
		}
		line = truncateDisplayWithGlyphProfile(ansi.Strip(line), budget, m.glyphProfile)
		line = m.styles.Dimmed.Render(line)
	}
	return line
}
