package ui

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// terminalInputResumeQuietPeriod is a short host-owned quiet window. Resize and
// terminal input arrive from independent Bubble Tea sources, so their delivery
// order is not evidence of when a key was typed. Every terminal event observed
// during this window moves one shared deadline; the single pending Tick either
// advances at that deadline or reschedules itself for the remainder.
const terminalInputResumeQuietPeriod = 200 * time.Millisecond

type terminalInputResumePhase uint8

const (
	terminalInputResumeIdle terminalInputResumePhase = iota
	terminalInputResumeInitialQuiet
	terminalInputResumeAwaitGesture
	terminalInputResumeConfirmationQuiet
)

type terminalInputResumeMsg struct {
	Token uint64
	At    time.Time
}

// pauseTerminalOriginatedInput is the single boundary between the parent
// model and terminal input while an interactive surface is hidden. Internal
// receipts continue through Update normally. Key release, bracketed-paste
// delimiters, and actionable pointer variants count as activity because they
// can be delivered independently of the press/content event that preceded
// resize. Cell motion is intentionally excluded: it owns no action and must
// not keep a restored terminal quarantined while the pointer moves.
func (m *Model) pauseTerminalOriginatedInput(message tea.Msg) (bool, tea.Cmd) {
	if m == nil || !m.terminalInteractionPaused() {
		return false, nil
	}
	switch typed := message.(type) {
	case tea.KeyPressMsg:
		if key.Matches(typed, m.keys.Quit) {
			return false, nil
		}
		if m.terminalInputResumePhase == terminalInputResumeAwaitGesture && typed.Code == tea.KeyEnter && typed.Mod == 0 {
			return true, m.confirmTerminalInputResume()
		}
	case tea.KeyReleaseMsg, tea.PasteStartMsg, tea.PasteEndMsg, tea.PasteMsg,
		tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseWheelMsg:
	default:
		return false, nil
	}
	return true, m.extendTerminalInputResume()
}

func (m *Model) armTerminalInputResume() tea.Cmd {
	if m == nil {
		return nil
	}
	if m.terminalInputResumeActive() {
		// WindowSizeMsg can arrive in bursts while the user drags a terminal
		// edge. Reuse the receipt already in flight instead of allocating one
		// timer per frame; its observed timestamp will reschedule the remaining
		// portion of this sliding deadline when necessary.
		m.terminalInputResumePhase = terminalInputResumeInitialQuiet
		m.resetHiddenApprovalChoice()
		next := time.Now().Add(terminalInputResumeQuietPeriod)
		if !next.After(m.terminalInputResumeAt) {
			next = m.terminalInputResumeAt.Add(time.Nanosecond)
		}
		m.terminalInputResumeAt = next
		if m.terminalInputTickPending {
			return nil
		}
		m.terminalInputTickPending = true
		return terminalInputResumeTick(m.terminalInputResumeToken, terminalInputResumeQuietPeriod)
	}
	m.terminalInputResumeToken++
	m.terminalInputResumePhase = terminalInputResumeInitialQuiet
	m.resetHiddenApprovalChoice()
	m.terminalInputResumeAt = time.Now().Add(terminalInputResumeQuietPeriod)
	m.terminalInputTickPending = true
	return terminalInputResumeTick(m.terminalInputResumeToken, terminalInputResumeQuietPeriod)
}

func (m *Model) extendTerminalInputResume() tea.Cmd {
	if m == nil || (m.terminalInputResumePhase != terminalInputResumeInitialQuiet &&
		m.terminalInputResumePhase != terminalInputResumeConfirmationQuiet) {
		return nil
	}
	next := time.Now().Add(terminalInputResumeQuietPeriod)
	if !next.After(m.terminalInputResumeAt) {
		next = m.terminalInputResumeAt.Add(time.Nanosecond)
	}
	m.terminalInputResumeAt = next
	if m.terminalInputTickPending {
		return nil
	}
	m.terminalInputTickPending = true
	return terminalInputResumeTick(m.terminalInputResumeToken, terminalInputResumeQuietPeriod)
}

func (m *Model) finishTerminalInputResume(message terminalInputResumeMsg) tea.Cmd {
	if m == nil || !m.terminalInputResumeActive() || message.Token != m.terminalInputResumeToken {
		return nil
	}
	if m.terminalInputResumePhase == terminalInputResumeAwaitGesture {
		return nil
	}
	if m.narrowTerminalHint() != "" || m.shuttingDown {
		return nil
	}
	m.terminalInputTickPending = false
	observedAt := message.At
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if remaining := m.terminalInputResumeAt.Sub(observedAt); remaining > 0 {
		m.terminalInputTickPending = true
		return terminalInputResumeTick(m.terminalInputResumeToken, remaining)
	}
	m.terminalInputResumeAt = time.Time{}
	switch m.terminalInputResumePhase {
	case terminalInputResumeInitialQuiet:
		m.terminalInputResumePhase = terminalInputResumeAwaitGesture
	case terminalInputResumeConfirmationQuiet:
		m.terminalInputResumePhase = terminalInputResumeIdle
	}
	return nil
}

func (m *Model) confirmTerminalInputResume() tea.Cmd {
	if m == nil || m.terminalInputResumePhase != terminalInputResumeAwaitGesture {
		return nil
	}
	m.terminalInputResumeToken++
	m.terminalInputResumePhase = terminalInputResumeConfirmationQuiet
	m.terminalInputResumeAt = time.Now().Add(terminalInputResumeQuietPeriod)
	m.terminalInputTickPending = true
	return terminalInputResumeTick(m.terminalInputResumeToken, terminalInputResumeQuietPeriod)
}

func (m *Model) terminalInputResumeActive() bool {
	return m != nil && m.terminalInputResumePhase != terminalInputResumeIdle
}

func (m *Model) cancelTerminalInputResume() {
	if m == nil {
		return
	}
	// Invalidate any receipt already scheduled by tea.Tick.
	m.terminalInputResumeToken++
	m.terminalInputResumePhase = terminalInputResumeIdle
	m.terminalInputResumeAt = time.Time{}
	m.terminalInputTickPending = false
}

func terminalInputResumeTick(token uint64, wait time.Duration) tea.Cmd {
	return tea.Tick(max(wait, time.Nanosecond), func(now time.Time) tea.Msg {
		return terminalInputResumeMsg{Token: token, At: now}
	})
}
