package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestFooterNoticeExpiryClearsOnlyMatchingDeadline(t *testing.T) {
	m := newTestModel(t)
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }

	_ = m.setFooterNotice(noticeSuccess, "✓ Done", 2*time.Second)
	first := m.footerNotice.expiresAt

	updated, _ := m.Update(footerNoticeExpiredMsg{deadline: first.Add(time.Second)})
	m = updated.(*Model)
	if m.footerNotice == nil {
		t.Fatal("a foreign deadline cleared the active notice")
	}

	updated, _ = m.Update(footerNoticeExpiredMsg{deadline: first})
	m = updated.(*Model)
	if m.footerNotice != nil {
		t.Fatalf("matching deadline did not clear the notice: %#v", m.footerNotice)
	}
}

func TestFooterNoticeSecondNoticeOutlivesFirstTimer(t *testing.T) {
	m := newTestModel(t)
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }

	_ = m.setFooterNotice(noticeSuccess, "✓ Done", 2*time.Second)
	first := m.footerNotice.expiresAt

	m.now = func() time.Time { return base.Add(time.Second) }
	_ = m.setFooterNotice(noticeWarning, "context is nearly full", 2*time.Second)

	updated, _ := m.Update(footerNoticeExpiredMsg{deadline: first})
	m = updated.(*Model)
	if m.footerNotice == nil || m.footerNotice.text != "context is nearly full" {
		t.Fatalf("first notice's timer cleared its replacement: %#v", m.footerNotice)
	}

	updated, _ = m.Update(footerNoticeExpiredMsg{deadline: m.footerNotice.expiresAt})
	m = updated.(*Model)
	if m.footerNotice != nil {
		t.Fatalf("replacement notice did not expire on its own deadline: %#v", m.footerNotice)
	}
}

func TestFooterNoticeDoneTextRendersByteIdentically(t *testing.T) {
	m := newTestModel(t)
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base.Add(2400 * time.Millisecond) }
	m.turnStartedAt = base
	m.state = StateStreaming

	updated, _ := m.Update(AgentDoneMsg{})
	m = updated.(*Model)
	if m.footerNotice == nil || m.footerNotice.text != "✓ Done · 2.4s" {
		t.Fatalf("completion notice text = %#v, want \"✓ Done · 2.4s\"", m.footerNotice)
	}
	if status := ansi.Strip(m.renderStatusLine()); !strings.Contains(status, "✓ Done · 2.4s") {
		t.Fatalf("status line did not render the exact done receipt: %q", status)
	}
}
