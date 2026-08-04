package ui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const themePreviewRows = 2

// themePickerChromeRows includes the ordinary picker chrome plus the newline
// and two-row preview surface below the list.
const themePickerChromeRows = 4 + 1 + themePreviewRows

// ThemePickerState is the transient color-scheme chooser. It holds navigation
// state only; the parent Model owns the selection and its persistence.
type ThemePickerState struct {
	List       list.Model
	ItemHeight int
}

type themeItem struct {
	id          string
	label       string
	description string
	current     bool
}

func (i themeItem) Title() string {
	title := sanitizeTerminalSingleLine(i.label)
	if i.current {
		return title + " · current"
	}
	return title
}

func (i themeItem) Description() string { return sanitizeTerminalSingleLine(i.description) }
func (i themeItem) FilterValue() string {
	return sanitizeTerminalSingleLine(i.label + " " + i.id)
}

func newThemePickerState(current string, terminalWidth, terminalHeight int, isDark bool, themeID string, profiles ...GlyphProfile) *ThemePickerState {
	profile := resolveGlyphProfile(profiles...)
	current = resolveThemeID(current)

	ids := themeIDs()
	items := make([]list.Item, 0, len(ids))
	for _, id := range ids {
		theme := resolveTheme(id)
		items = append(items, themeItem{
			id:          theme.ID,
			label:       theme.Label,
			description: theme.Description,
			current:     theme.ID == current,
		})
	}

	delegate := newPickerDelegate(isDark, false, themeID, profile)
	width := pickerListWidth(terminalWidth)
	height := pickerListHeight(terminalHeight, len(items)*delegate.Height()+2, themePickerChromeRows)
	l := list.New(items, delegate, width, height)
	configurePickerList(&l, isDark, themeID)
	configurePickerListGlyphProfile(&l, profile)
	l.Title = "Theme"
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings()

	for index, item := range items {
		if entry, ok := item.(themeItem); ok && entry.current {
			l.Select(index)
			break
		}
	}

	return &ThemePickerState{List: l, ItemHeight: delegate.Height()}
}

// SelectedThemeID returns the highlighted theme.
func (s *ThemePickerState) SelectedThemeID() string {
	if s == nil {
		return ""
	}
	item, ok := s.List.SelectedItem().(themeItem)
	if !ok {
		return ""
	}
	return item.id
}

func (m *Model) openThemePicker() {
	if m == nil {
		return
	}
	m.themePickerState = newThemePickerState(
		m.ThemeID(), m.width, m.height, m.isDark, m.themeID, m.glyphProfile,
	)
	m.overlay = OverlayThemePicker
	m.input.Blur()
	m.recalcViewportHeight()
}

func (m *Model) closeThemePicker() {
	m.themePickerState = nil
	m.closeOverlayToParent()
}

func (m *Model) renderThemePicker() string {
	if m.themePickerState == nil {
		return ""
	}
	content := strings.TrimRight(m.themePickerState.List.View(), "\n")
	content += "\n" + m.renderThemePreview(m.themePickerState.SelectedThemeID())
	return m.renderPickerFrame(
		content,
		m.pickerNavigationFooter(false),
	)
}

// renderThemePreview is deliberately small: it shows the selected scheme's
// actual surface plus its primary semantic colors without applying or
// persisting the theme. Navigation therefore remains reversible with Escape.
func (m *Model) renderThemePreview(id string) string {
	theme := resolveTheme(id)
	palette := outputSemanticPalette(m.isDark, theme.ID)
	width := pickerListWidth(m.width)
	background := lipgloss.NewStyle().Background(palette.Background)
	heading := background.
		Foreground(palette.Text).
		Bold(true).
		Width(width).
		Render(truncateDisplay(" Preview · "+theme.Label, width))
	samples := background.Foreground(palette.Text).Render(" Aa ") +
		background.Foreground(palette.Accent).Bold(true).Render(" accent ") +
		background.Foreground(palette.Success).Render(" success ") +
		background.Foreground(palette.Warning).Render(" warning ")
	samples = background.Width(width).MaxWidth(width).Render(samples)
	return heading + "\n" + samples
}

// applyTheme selects a scheme, repaints, and persists the choice. Persistence
// failures are surfaced but never block the visual change: the user asked to
// see a different palette, and being unable to remember it for next time is a
// smaller problem than refusing to apply it.
func (m *Model) applyTheme(id string) tea.Cmd {
	if m == nil {
		return nil
	}
	id = normalizeThemeID(id)
	if !m.SetTheme(id) {
		return m.setFooterNotice(noticeError, "Unknown theme: "+sanitizeTerminalSingleLine(id), 4*time.Second)
	}
	label := resolveTheme(id).Label
	if err := m.persistThemePreference(id); err != nil {
		if m.logger != nil {
			m.logger.Error("save theme preference", "error", err)
		}
		return m.setFooterNotice(noticeWarning, "Theme "+label+" applied, but not saved for next time", 4*time.Second)
	}
	return m.setFooterNotice(noticeSuccess, "Theme "+label, 2*time.Second)
}

func (m *Model) persistThemePreference(id string) error {
	if m == nil || m.modelPreferenceStore == nil {
		return nil
	}
	return m.modelPreferenceStore.SetTheme(id)
}
