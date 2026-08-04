package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestPasteMsg_SmallPaste(t *testing.T) {
	m := newTestModel(t)
	m.state = StateIdle

	content := "short paste"
	updated, _ := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)

	if m.pendingPaste != nil {
		t.Error("small paste should not trigger pending paste")
	}
	if got := strings.Count(m.input.Value(), content); got != 1 {
		t.Fatalf("small paste inserted %d times, want exactly once: %q", got, m.input.Value())
	}
}

func TestPasteMsgCanonicalizesPlatformNewlinesBeforeReviewAndInsertion(t *testing.T) {
	m := newTestModel(t)
	raw := "one\r\ntwo\rthree\tfour"
	want := "one\ntwo\nthree    four"

	updated, _ := m.Update(tea.PasteMsg{Content: raw})
	m = updated.(*Model)
	if m.pendingPaste != nil {
		t.Fatalf("small canonical paste unexpectedly requires review: %#v", m.pendingPaste)
	}
	if got := m.input.Value(); got != want {
		t.Fatalf("canonical paste = %q, want %q", got, want)
	}

	assessment := assessPaste(strings.Repeat("line\r\n", 10), pasteCursorAtEnd(""), 0, 1, m.input.CharLimit)
	if assessment.Lines != 11 || !assessment.NeedsReview {
		t.Fatalf("CRLF assessment = %d lines, review=%v", assessment.Lines, assessment.NeedsReview)
	}
}

func TestPasteMsg_LargePaste(t *testing.T) {
	m := newTestModel(t)
	m.state = StateIdle

	// Create paste with >10 lines.
	content := strings.Repeat("line\n", 15)
	updated, _ := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)

	if m.pendingPaste == nil {
		t.Error("large paste should trigger pending paste")
	}
	if m.input.Value() != "" {
		t.Fatalf("large paste reached the composer before consent: %q", m.input.Value())
	}
}

func TestCtrlVPasteCannotBypassParentReview(t *testing.T) {
	m := newTestModel(t)
	content := strings.Repeat("line\n", 15)
	m.clipboardRead = func() (string, error) { return content, nil }

	updated, cmd := m.Update(ctrlKey('v'))
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("Ctrl+V did not schedule the parent clipboard read")
	}
	message := cmd()
	paste, ok := message.(tea.PasteMsg)
	if !ok || paste.Content != content {
		t.Fatalf("Ctrl+V command returned %#v, want public PasteMsg", message)
	}
	updated, _ = m.Update(paste)
	m = updated.(*Model)
	if m.pendingPaste == nil || !m.pendingPaste.NeedsReview || m.input.Value() != "" {
		t.Fatalf("Ctrl+V bypassed review: pending=%#v input=%q", m.pendingPaste, m.input.Value())
	}
}

func TestPasteMsg_LargePasteDuringOrdinaryTurnRequiresReview(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming

	content := strings.Repeat("line\n", 15)
	updated, _ := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)

	if m.pendingPaste == nil || !m.pendingPaste.NeedsReview {
		t.Error("large follow-up paste bypassed review during streaming")
	}
	if m.input.Value() != "" {
		t.Fatalf("reviewed follow-up paste mutated composer: %q", m.input.Value())
	}
}

func TestPasteMsg_SmallPasteDraftsFollowUpDuringOrdinaryTurn(t *testing.T) {
	m := newTestModel(t)
	m.state = StateWaiting

	updated, _ := m.Update(tea.PasteMsg{Content: "check this next"})
	m = updated.(*Model)
	if got := m.input.Value(); got != "check this next" {
		t.Fatalf("running follow-up paste = %q", got)
	}
	if m.pendingPaste != nil {
		t.Fatal("small running paste unexpectedly required review")
	}
}

func TestPasteMsgCannotCreateHiddenSecondFollowUp(t *testing.T) {
	m := newTestModel(t)
	m.state = StateStreaming
	m.queuedFollowUp = &queuedFollowUp{Prompt: "first follow-up"}

	updated, _ := m.Update(tea.PasteMsg{Content: "hidden second follow-up"})
	m = updated.(*Model)
	if got := m.input.Value(); got != "" {
		t.Fatalf("queued slot accepted hidden paste: %q", got)
	}
	if m.pendingPaste != nil {
		t.Fatal("queued slot opened an unreachable paste decision")
	}
}

func TestPendingPaste_AcceptY(t *testing.T) {
	m := newTestModel(t)
	m.pendingPaste = assessPaste("line1\nline2\nline3", pasteCursorAtEnd(m.input.Value()), m.input.Length(), m.input.LineCount(), m.input.CharLimit)

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'Y'})
	m = updated.(*Model)

	if m.pendingPaste != nil {
		t.Error("pressing Y should clear pending paste")
	}
}

func TestPendingPasteAcceptanceKeepsClosingFenceVisible(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 32})
	m = updated.(*Model)
	base := "Review the first receipt.\nKeep the second line while I check Settings.\n"
	m.input.SetValue(base)
	m.syncInputHeight()
	paste := strings.Join([]string{
		"fixture line 01", "fixture line 02", "fixture line 03", "fixture line 04",
		"fixture line 05", "fixture line 06", "fixture line 07", "fixture line 08",
		"fixture line 09", "fixture line 10", "fixture line 11",
	}, "\n")
	updated, _ = m.Update(tea.PasteMsg{Content: paste})
	m = updated.(*Model)
	if m.pendingPaste == nil || m.pendingPaste.Content != paste {
		t.Fatal("large paste did not remain pending for an explicit decision")
	}
	if got := m.input.Value(); got != base {
		t.Fatalf("large paste mutated the draft before consent: %q", got)
	}

	updated, _ = m.Update(charKey('y'))
	m = updated.(*Model)
	if got := strings.Count(m.input.Value(), "fixture line 01"); got != 1 {
		t.Fatalf("accepted paste inserted %d copies, want exactly one", got)
	}
	view := m.View().Content
	for _, want := range []string{"fixture line 11", "```"} {
		if !strings.Contains(view, want) {
			t.Fatalf("accepted paste clipped %q:\n%s", want, view)
		}
	}
}

func TestPendingPasteAtMidLineKeepsBothFencesOnOwnLines(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("abcXYZ")
	m.input.CursorStart()
	for range 3 {
		updated, _ := m.Update(rightKey())
		m = updated.(*Model)
	}
	if m.input.Line() != 0 || m.input.Column() != 3 {
		t.Fatalf("cursor = %d:%d, want 0:3", m.input.Line(), m.input.Column())
	}

	paste := strings.Join([]string{
		"line 01", "line 02", "line 03", "line 04", "line 05", "line 06",
		"line 07", "line 08", "line 09", "line 10", "line 11",
	}, "\n")
	updated, _ := m.Update(tea.PasteMsg{Content: paste})
	m = updated.(*Model)
	if m.pendingPaste == nil {
		t.Fatal("large mid-line paste did not require review")
	}
	wantInsertion := "\n```\n" + paste + "\n```\n"
	if got := m.pendingPaste.Fenced; got != wantInsertion {
		t.Fatalf("fenced mid-line insertion = %q, want %q", got, wantInsertion)
	}

	updated, _ = m.Update(charKey('y'))
	m = updated.(*Model)
	want := "abc" + wantInsertion + "XYZ"
	if got := m.input.Value(); got != want {
		t.Fatalf("accepted mid-line paste = %q, want %q", got, want)
	}
}

func TestPendingPaste_RejectN(t *testing.T) {
	m := newTestModel(t)
	m.pendingPaste = assessPaste("line1\nline2\nline3", pasteCursorAtEnd(m.input.Value()), m.input.Length(), m.input.LineCount(), m.input.CharLimit)

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'n'})
	m = updated.(*Model)

	if m.pendingPaste != nil {
		t.Error("pressing n should clear pending paste")
	}
	if got := strings.Count(m.input.Value(), "line1"); got != 1 {
		t.Fatalf("plain paste inserted %d copies, want exactly one", got)
	}
}

func TestPendingPaste_CancelEsc(t *testing.T) {
	m := newTestModel(t)
	m.pendingPaste = assessPaste("line1\nline2\nline3", pasteCursorAtEnd(m.input.Value()), m.input.Length(), m.input.LineCount(), m.input.CharLimit)

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(*Model)

	if m.pendingPaste != nil {
		t.Error("pressing esc should clear pending paste")
	}
}

func TestPasteMsg_HugeSingleLineRequiresReview(t *testing.T) {
	m := newTestModel(t)
	content := strings.Repeat("x", pasteReviewByteThreshold)

	updated, _ := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if m.pendingPaste == nil || !m.pendingPaste.NeedsReview {
		t.Fatal("large one-line payload bypassed paste review")
	}
	if m.input.Value() != "" {
		t.Fatal("reviewed one-line payload mutated the composer before consent")
	}
	if prompt := ansi.Strip(m.renderStatusLine()); !strings.Contains(prompt, "1 line") || !strings.Contains(prompt, "4.0 KiB") {
		t.Fatalf("paste prompt omitted truthful size metadata:\n%s", prompt)
	}
}

func TestPasteMsg_OverCapacityIsRefusedWithoutClipping(t *testing.T) {
	m := newTestModel(t)
	content := strings.Repeat("x", m.input.CharLimit+1)

	updated, _ := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if m.pendingPaste == nil || m.pendingPaste.PlainFits {
		t.Fatal("over-capacity payload was not refused")
	}
	prompt := ansi.Strip(m.renderStatusLine())
	for _, want := range []string{"Paste too large", "@file", "/load", "esc dismiss"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("oversize prompt missing %q:\n%s", want, prompt)
		}
	}

	updated, _ = m.Update(charKey('y'))
	m = updated.(*Model)
	if m.pendingPaste == nil || m.input.Value() != "" {
		t.Fatal("oversize payload was inserted or dismissed by an unsupported choice")
	}
}

func TestPasteMsg_DoubleWidthDraftUsesBubblesRemainingCapacity(t *testing.T) {
	m := newTestModel(t)
	draft := strings.Repeat("界", m.input.CharLimit/2)
	m.input.SetValue(draft)
	if got := m.input.Length(); got != m.input.CharLimit {
		t.Fatalf("double-width fixture length = %d, want %d", got, m.input.CharLimit)
	}

	updated, _ := m.Update(tea.PasteMsg{Content: "x"})
	m = updated.(*Model)
	if m.pendingPaste == nil || m.pendingPaste.PlainFits {
		t.Fatal("double-width draft allowed a paste that Bubbles would silently clip")
	}
	if m.input.Value() != draft {
		t.Fatal("refused paste changed the double-width draft")
	}
}

func TestPasteMsg_TabExpansionCannotSilentlyClip(t *testing.T) {
	m := newTestModel(t)
	draft := strings.Repeat("d", m.input.CharLimit-8)
	m.input.SetValue(draft)

	updated, _ := m.Update(tea.PasteMsg{Content: "\t\t\t"})
	m = updated.(*Model)
	if m.pendingPaste == nil || m.pendingPaste.PlainFits {
		t.Fatal("three tabs were admitted into eight remaining cells despite expanding to twelve spaces")
	}
	if m.input.Value() != draft {
		t.Fatal("refused tab-heavy paste changed the draft")
	}
}

func TestPasteMsg_TextareaLineLimitCannotSilentlyClip(t *testing.T) {
	m := newTestModel(t)
	content := strings.Repeat("\n", pasteTextareaMaxLines)

	updated, _ := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if m.pendingPaste == nil || m.pendingPaste.PlainFits {
		t.Fatal("payload beyond the textarea logical-row limit was admitted")
	}
	if m.input.Value() != "" || m.input.LineCount() != 1 {
		t.Fatal("refused high-line paste partially changed the composer")
	}
}

func TestPendingPaste_HeavilyWrappedLogicalLimitPayloadIsByteExact(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	m = updated.(*Model)
	// Stay below both sonar admission ceilings while exceeding 10,000
	// wrapped visual rows. Bubbles' content limit must not silently reinterpret
	// the logical-line contract and clip the accepted tail.
	content := strings.Repeat("x\n", pasteTextareaMaxLines-3) + strings.Repeat("tail ", 2_500)
	if len(content) >= m.input.CharLimit || strings.Count(content, "\n")+1 >= pasteTextareaMaxLines {
		t.Fatalf("invalid near-limit fixture: bytes=%d lines=%d", len(content), strings.Count(content, "\n")+1)
	}

	updated, _ = m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if m.pendingPaste == nil || !m.pendingPaste.PlainFits {
		t.Fatalf("near-limit payload was not offered for exact acceptance: %#v", m.pendingPaste)
	}
	updated, _ = m.Update(charKey('n'))
	m = updated.(*Model)
	if got := m.input.Value(); got != content {
		t.Fatalf("accepted near-limit payload changed: got %d bytes, want %d", len(got), len(content))
	}
	if !strings.HasSuffix(m.input.Value(), "tail ") {
		t.Fatal("accepted near-limit payload lost its closing bytes")
	}
}

func TestPendingPaste_EmbeddedFenceUsesLongerMarkdownFence(t *testing.T) {
	m := newTestModel(t)
	content := strings.Join([]string{
		"line 01", "line 02", "line 03", "```", "line 05", "line 06",
		"line 07", "line 08", "line 09", "line 10", "line 11",
	}, "\n")
	updated, _ := m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)
	if m.pendingPaste == nil || !strings.HasPrefix(m.pendingPaste.Fenced, "````\n") {
		t.Fatalf("embedded backticks did not select a safe fence: %#v", m.pendingPaste)
	}

	updated, _ = m.Update(charKey('y'))
	m = updated.(*Model)
	if got := strings.Count(m.input.Value(), content); got != 1 {
		t.Fatalf("fenced payload was not preserved exactly once: %d", got)
	}
	if got := strings.Count(m.input.Value(), "````"); got != 2 {
		t.Fatalf("safe wrapper has %d four-backtick fences, want two", got)
	}
}

func TestPendingPaste_OffersPlainOnlyWhenFenceExceedsCapacity(t *testing.T) {
	m := newTestModel(t)
	content := strings.Repeat("p", pasteReviewByteThreshold)
	draft := strings.Repeat("d", m.input.CharLimit-len(content))
	m.input.SetValue(draft)
	m.pendingPaste = assessPaste(content, pasteCursorAtEnd(draft), m.input.Length(), m.input.LineCount(), m.input.CharLimit)
	if !m.pendingPaste.PlainFits || m.pendingPaste.FencedFits {
		t.Fatalf("invalid plain-only fixture: %#v", m.pendingPaste)
	}
	if prompt := ansi.Strip(m.renderStatusLine()); !strings.Contains(prompt, "plain only") || strings.Contains(prompt, "n plain") {
		t.Fatalf("plain-only decision is misleading:\n%s", prompt)
	}

	updated, _ := m.Update(charKey('y'))
	m = updated.(*Model)
	if m.pendingPaste != nil || m.input.Value() != draft+content {
		t.Fatal("plain-only acceptance did not preserve the exact payload")
	}
}

func TestPasteDecisionKeepsAllActionsAtMinimumWidth(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = updated.(*Model)
	content := strings.Repeat("line\n", 11)
	updated, _ = m.Update(tea.PasteMsg{Content: content})
	m = updated.(*Model)

	prompt := ansi.Strip(m.renderStatusLine())
	for _, want := range []string{"esc cancel", "y code", "n plain"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("minimum paste prompt lost %q:\n%s", want, prompt)
		}
	}
	if got := lipgloss.Height(m.View().Content); got > m.height {
		t.Fatalf("minimum paste decision height = %d, want <= %d", got, m.height)
	}
}
