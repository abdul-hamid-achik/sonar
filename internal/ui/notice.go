package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// noticeSeverity classifies a transient footer notice for styling.
type noticeSeverity int

const (
	noticeSuccess noticeSeverity = iota
	noticeInfo
	noticeWarning
	noticeError
)

// footerNotice is the single transient status-line slot. A newer notice
// replaces the current one; expiry is driven by a matching-deadline tick so a
// stale timer from a replaced notice can never clear its successor.
type footerNotice struct {
	text      string
	severity  noticeSeverity
	expiresAt time.Time
}

// footerNoticeExpiredMsg clears the notice slot whose deadline armed it.
type footerNoticeExpiredMsg struct {
	deadline time.Time
}

// setFooterNotice stores the notice and returns the expiry tick for it.
func (m *Model) setFooterNotice(severity noticeSeverity, text string, ttl time.Duration) tea.Cmd {
	deadline := m.nowTime().Add(ttl)
	m.footerNotice = &footerNotice{text: text, severity: severity, expiresAt: deadline}
	return tea.Tick(ttl, func(time.Time) tea.Msg {
		return footerNoticeExpiredMsg{deadline: deadline}
	})
}

// handleFooterNoticeExpired settles the timer for the notice that armed it.
func (m *Model) handleFooterNoticeExpired(msg footerNoticeExpiredMsg) {
	if m.footerNotice != nil && m.footerNotice.expiresAt.Equal(msg.deadline) {
		m.footerNotice = nil
	}
}

func (m *Model) hasSuccessFooterNotice() bool {
	return m.footerNotice != nil && m.footerNotice.severity == noticeSuccess
}

func (m *Model) footerNoticeStyle(severity noticeSeverity) lipgloss.Style {
	switch severity {
	case noticeWarning:
		return m.styles.StatusWarning
	case noticeError:
		return m.styles.ErrorText.UnsetPaddingLeft()
	case noticeInfo:
		return m.styles.StatusText
	default:
		return m.styles.StatusCheck.UnsetPaddingLeft()
	}
}
