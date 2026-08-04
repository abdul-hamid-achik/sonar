package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

// sessionItem implements list.DefaultItem for the sessions picker.
type sessionItem struct {
	id        int64
	publicID  string
	title     string
	createdAt string
}

func (i sessionItem) Title() string {
	title := sanitizeTerminalSingleLine(i.title)
	if handle := sessionref.Format(i.publicID); handle != "" {
		if title == "" {
			return handle
		}
		title = fmt.Sprintf("%s · %s", handle, title)
	}
	return truncateDisplay(title, 40)
}

func (i sessionItem) Description() string {
	return i.createdAt
}

func (i sessionItem) FilterValue() string {
	title := sanitizeTerminalSingleLine(i.title)
	if handle := sessionref.Format(i.publicID); handle != "" {
		return fmt.Sprintf("%s %d %s", handle, i.id, title)
	}
	return title
}

// SessionsPickerState holds state for the sessions picker overlay.
type SessionsPickerState struct {
	List     list.Model
	Sessions []SessionListItem
	Phase    sessionsPickerPhase
	Message  string
}

type sessionsPickerPhase int

const (
	sessionsLoading sessionsPickerPhase = iota
	sessionsReady
	sessionsEmpty
	sessionsFailed
)

func newSessionsLoadingState() *SessionsPickerState {
	return &SessionsPickerState{Phase: sessionsLoading}
}

// newSessionsPickerState creates a new SessionsPickerState with a bubbles list.
func newSessionsPickerState(sessions []SessionListItem, width, height int, isDark bool, themeID string, reducedMotion ...bool) *SessionsPickerState {
	items := make([]list.Item, len(sessions))
	for i, s := range sessions {
		items[i] = sessionItem{
			id:        s.ID,
			publicID:  s.PublicID,
			title:     s.Title,
			createdAt: formatSessionTimestamp(s.CreatedAt),
		}
	}

	delegate := newPickerDelegate(isDark, false, themeID)

	listW := pickerListWidth(width)

	// Height: items fit, max 20 lines
	pickerH := len(sessions)*delegate.Height() + 4 // +4 for title + filter
	pickerH = pickerListHeight(height, pickerH, 4)

	l := list.New(items, delegate, listW, pickerH)
	configurePickerList(&l, isDark, themeID, reducedMotion...)
	l.Title = "Sessions"
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()

	return &SessionsPickerState{
		List:     l,
		Sessions: sessions,
		Phase:    sessionsReady,
	}
}

// formatSessionTimestamp turns persisted RFC3339 values into compact,
// scan-friendly labels. It keeps the timestamp's recorded offset instead of
// consulting time.Local, which makes terminal snapshots deterministic across
// machines. Human-authored and otherwise unparseable labels pass through.
func formatSessionTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return value
	}

	zone := parsed.Format("-07:00")
	if zone == "+00:00" {
		zone = "UTC"
	} else {
		zone = "UTC" + zone
	}
	return parsed.Format("Jan 2, 2006 · 15:04") + " " + zone
}

func newSessionsMessageState(phase sessionsPickerPhase, message string) *SessionsPickerState {
	return &SessionsPickerState{Phase: phase, Message: strings.TrimSpace(message)}
}

func (s *SessionsPickerState) ready() bool {
	return s != nil && s.Phase == sessionsReady
}

func (m *Model) openSessionsPicker() {
	m.sessionsPickerState = newSessionsLoadingState()
	m.overlay = OverlaySessionsPicker
	m.input.Blur()
}

// renderSessionsPicker renders the sessions picker overlay.
func (m *Model) renderSessionsPicker() string {
	ps := m.sessionsPickerState
	if ps == nil {
		return ""
	}

	if ps.ready() {
		return m.renderPickerFrame(
			ps.List.View(), m.pickerNavigationFooter(true),
		)
	}

	content := m.styles.OverlayTitle.Render("Sessions") + "\n\n"
	switch ps.Phase {
	case sessionsLoading:
		content += m.spin.View() + " " + m.styles.StatusText.Render("Loading saved sessions…")
	case sessionsFailed:
		content += m.styles.ErrorText.Render("Unable to load sessions") + "\n"
		content += m.styles.OverlayDim.Render(wrapText(ps.Message, pickerListWidth(m.width)))
	default:
		content += m.styles.OverlayDim.Render("No saved sessions in this workspace.")
	}
	return m.renderPickerFrame(content, m.renderKeyHints(
		pickerListWidth(m.width),
		keyHint{Key: "esc", Action: m.overlayCloseLabel()},
	))
}

// closeSessionsPicker dismisses the sessions picker overlay.
func (m *Model) closeSessionsPicker() {
	m.sessionsPickerState = nil
	m.closeOverlayToParent()
}

// requestSessions starts the existing tokened workspace-session lookup. Both
// the slash command and Settings use this single authority path.
func (m *Model) requestSessions() tea.Cmd {
	if m.sessionListCancel != nil {
		m.sessionListCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.sessionListCancel = cancel
	m.sessionListToken++
	listToken := m.sessionListToken
	m.sessionListing = true
	m.input.Blur()
	store := m.sessionStore
	workDir := m.agent.WorkDir()
	load := func() tea.Msg {
		workspaceID, err := canonicalWorkspaceID(workDir)
		if err != nil {
			return SessionListMsg{ListToken: listToken, Err: err}
		}
		sessions, err := listPersistedSessions(ctx, store, workspaceID, 100)
		return SessionListMsg{ListToken: listToken, Sessions: sessions, Err: err}
	}
	return tea.Batch(m.startActivityCmd(), load)
}
