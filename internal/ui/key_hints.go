package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// keyHint is presentation-only. Matching remains owned by Bubbles key.Binding;
// callers should source Key from Binding.Help whenever one exists.
type keyHint struct {
	Key    string
	Action string
}

// renderKeyHints applies one footer grammar everywhere: alternative keys use
// '/', keys and actions use one space, and peer hints use ' · '. Callers order
// hints by importance, with dismissal/safety first. Narrow layouts preserve
// every essential key with the leading action before dropping lower-priority
// controls from the right.
func (m *Model) renderKeyHints(width int, hints ...keyHint) string {
	if width <= 0 || len(hints) == 0 {
		return ""
	}
	hints = mergeKeyHintAliases(hints)
	// Compact progressively: keep as many controls as possible, then retain as
	// many leading action labels as fit. Trying every intermediate action count
	// avoids needlessly turning a clear primary hint such as "enter select" into
	// an unlabeled "enter" when only a lower-priority action needs to yield.
	for keep := len(hints); keep > 0; keep-- {
		for actionLimit := keep; actionLimit > 0; actionLimit-- {
			if rendered := m.renderKeyHintSet(hints[:keep], actionLimit); lipgloss.Width(rendered) <= width {
				return rendered
			}
		}
	}
	return truncateDisplayWithGlyphProfile(m.renderKeyHintSet(hints[:1], 0), width, m.glyphProfile)
}

// mergeKeyHintAliases folds hints that advertise the same action into one hint
// with alternative keys, which is what the '/' in this grammar already means.
//
// Surfaces build their hints from local state, so two keys can legitimately end
// up bound to the same verb — the Cloud consent footer read "esc cancel · enter
// cancel" whenever the Cancel row was selected, which looks like a bug rather
// than like two ways to do one thing. Merging here rather than at each call
// site means a surface cannot reintroduce the pairing by accident, and no key
// is hidden: "esc/enter cancel" still advertises both.
func mergeKeyHintAliases(hints []keyHint) []keyHint {
	merged := make([]keyHint, 0, len(hints))
	positionByAction := make(map[string]int, len(hints))
	for _, hint := range hints {
		action := strings.ToLower(strings.TrimSpace(hint.Action))
		key := strings.TrimSpace(hint.Key)
		if action == "" || key == "" {
			merged = append(merged, hint)
			continue
		}
		if at, seen := positionByAction[action]; seen {
			if !strings.Contains(merged[at].Key+"/", key+"/") {
				merged[at].Key += "/" + key
			}
			continue
		}
		positionByAction[action] = len(merged)
		merged = append(merged, hint)
	}
	return merged
}

// actionLimit is -1 for every action, 0 for none, or a positive count of
// leading actions. Since callers place dismissal first, 1 preserves the
// critical close/back/cancel verb while compacting lower-priority hints.
func (m *Model) renderKeyHintSet(hints []keyHint, actionLimit int) string {
	parts := make([]string, 0, len(hints))
	for index, hint := range hints {
		keyLabel := strings.ToLower(strings.TrimSpace(hint.Key))
		action := strings.ToLower(strings.TrimSpace(hint.Action))
		keyLabel = pickerTextForGlyphProfile(keyLabel, m.glyphProfile)
		action = pickerTextForGlyphProfile(action, m.glyphProfile)
		if keyLabel == "" && action == "" {
			continue
		}
		part := ""
		if keyLabel != "" {
			part = m.styles.FocusIndicator.Render(keyLabel)
		}
		if (actionLimit < 0 || index < actionLimit) && action != "" {
			if part != "" {
				part += " "
			}
			part += m.styles.OverlayDim.Render(action)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, m.styles.OverlayDim.Render(glyphSeparator(m.glyphProfile)))
}
