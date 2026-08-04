package ui

import (
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type ModelPullPhase uint8

const (
	ModelPullEntry ModelPullPhase = iota
	ModelPullRunning
	ModelPullComplete
	ModelPullFailed
)

// OllamaModelPullRequestedMsg asks the parent to start a cancellable Ollama
// pull. No network work is performed by this presentation component.
type OllamaModelPullRequestedMsg struct{ Name string }

// OllamaModelPullCancelRequestedMsg asks the parent to cancel the active pull.
type OllamaModelPullCancelRequestedMsg struct{ Name string }

// OllamaModelPullProgressMsg is the stable projection of Ollama's NDJSON pull
// stream. Completed/Total may be zero for status-only cloud stub operations.
type OllamaModelPullProgressMsg struct {
	RequestID uint64
	Name      string
	Status    string
	Completed int64
	Total     int64
	Done      bool
	Err       error
}

type ModelPullState struct {
	Input         textinput.Model
	Progress      progress.Model
	Spinner       spinner.Model
	Phase         ModelPullPhase
	Name          string
	Status        string
	Completed     int64
	Total         int64
	Err           error
	isDark        bool
	themeID       string
	reducedMotion bool
}

const (
	maxModelPullNameCells   = 256
	maxModelPullStatusCells = 512
	maxModelPullErrorCells  = 512
)

func NewModelPullState(isDark, reducedMotion bool, themeID string) *ModelPullState {
	input := textinput.New()
	input.Prompt = "Model › "
	input.Placeholder = "qwen3-coder or gpt-oss:120b-cloud"
	input.SetStyles(semanticTextInputStyles(isDark, themeID))
	input.SetWidth(46)
	input.Focus()
	styles := input.Styles()
	styles.Cursor.Blink = !reducedMotion
	input.SetStyles(styles)

	palette := outputSemanticPalette(isDark, themeID)
	bar := progress.New(progress.WithColors(palette.Accent), progress.WithSpringOptions(20, 1))
	bar.EmptyColor = palette.Border
	bar.PercentageStyle = lipgloss.NewStyle().Foreground(palette.Muted)
	spin := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(palette.Accent)))
	return &ModelPullState{Input: input, Progress: bar, Spinner: spin, Phase: ModelPullEntry, isDark: isDark, reducedMotion: reducedMotion}
}

func (s *ModelPullState) SetTheme(isDark bool, themeID string) {
	if s == nil {
		return
	}
	themeID = resolveThemeID(themeID)
	if s.isDark == isDark && resolveThemeID(s.themeID) == themeID {
		return
	}
	s.isDark, s.themeID = isDark, themeID
	styles := semanticTextInputStyles(isDark, themeID)
	styles.Cursor.Blink = !s.reducedMotion
	s.Input.SetStyles(styles)
	palette := outputSemanticPalette(isDark, themeID)
	s.Progress.FullColor = palette.Accent
	s.Progress.EmptyColor = palette.Border
	s.Progress.PercentageStyle = lipgloss.NewStyle().Foreground(palette.Muted)
	s.Spinner.Style = lipgloss.NewStyle().Foreground(palette.Accent)
}

// Update follows the smart-parent/dumb-child boundary: it only emits a typed
// request. The parent owns cancellation, transport, and inventory refresh.
func (s *ModelPullState) Update(msg tea.Msg) tea.Cmd {
	if s == nil {
		return nil
	}
	if key, ok := msg.(tea.KeyPressMsg); ok && s.Phase == ModelPullEntry {
		switch key.String() {
		case "enter":
			name := safeModelPullText(s.Input.Value(), maxModelPullNameCells)
			if name == "" {
				return nil
			}
			s.Name, s.Status, s.Phase = name, "Starting pull", ModelPullRunning
			s.Input.Blur()
			return func() tea.Msg { return OllamaModelPullRequestedMsg{Name: name} }
		}
	}
	if key, ok := msg.(tea.KeyPressMsg); ok && s.Phase == ModelPullRunning && key.String() == "esc" {
		name := s.Name
		return func() tea.Msg { return OllamaModelPullCancelRequestedMsg{Name: name} }
	}
	if key, ok := msg.(tea.KeyPressMsg); ok && s.Phase == ModelPullFailed {
		switch key.String() {
		case "enter", "r":
			s.Status, s.Completed, s.Total, s.Err = "Starting pull", 0, 0, nil
			s.Phase = ModelPullRunning
			name := s.Name
			return func() tea.Msg { return OllamaModelPullRequestedMsg{Name: name} }
		case "e":
			s.Phase, s.Status, s.Completed, s.Total, s.Err = ModelPullEntry, "", 0, 0, nil
			s.Input.SetValue(s.Name)
			s.Input.CursorEnd()
			return s.Input.Focus()
		}
	}
	if tick, ok := msg.(spinner.TickMsg); ok && s.Phase == ModelPullRunning && !s.reducedMotion {
		var cmd tea.Cmd
		s.Spinner, cmd = s.Spinner.Update(tick)
		return cmd
	}
	if frame, ok := msg.(progress.FrameMsg); ok && !s.reducedMotion {
		updated, cmd := s.Progress.Update(frame)
		s.Progress = updated
		return cmd
	}
	if s.Phase == ModelPullEntry {
		var cmd tea.Cmd
		s.Input, cmd = s.Input.Update(msg)
		return cmd
	}
	return nil
}

func (s *ModelPullState) Apply(msg OllamaModelPullProgressMsg) tea.Cmd {
	if s == nil || (s.Name != "" && msg.Name != "" && msg.Name != s.Name) {
		return nil
	}
	if msg.Name != "" {
		s.Name = safeModelPullText(msg.Name, maxModelPullNameCells)
	}
	s.Status = safeModelPullText(msg.Status, maxModelPullStatusCells)
	s.Completed, s.Total = msg.Completed, msg.Total
	s.Err = safeModelPullError(msg.Err)
	if msg.Err != nil {
		s.Phase = ModelPullFailed
		return nil
	}
	if msg.Done {
		s.Phase = ModelPullComplete
	}
	if s.Total <= 0 {
		return nil
	}
	value := float64(s.Completed) / float64(s.Total)
	if s.reducedMotion {
		return nil
	}
	return s.Progress.SetPercent(value)
}

// View retains the standalone Bubbles virtual cursor for compatibility with
// focused component tests. The parent uses ViewWithCursor so the application
// owns one translated hardware cursor.
func (s *ModelPullState) View(width int) string {
	view, _ := s.render(width, false, false)
	return view
}

// ViewWithCursor renders a pure, sized copy of each Bubbles child. Compact
// mode bounds daemon-controlled status/error text so modal actions stay on the
// supported 30x12 canvas.
func (s *ModelPullState) ViewWithCursor(width int, compact bool) (string, *tea.Cursor) {
	return s.render(width, compact, true)
}

func (s *ModelPullState) render(width int, compact, hardwareCursor bool) (string, *tea.Cursor) {
	if s == nil {
		return "", nil
	}
	width = max(24, width)
	palette := outputSemanticPalette(s.isDark, s.themeID)
	title := lipgloss.NewStyle().Foreground(palette.Accent).Bold(true).Render("Add Ollama model")
	muted := lipgloss.NewStyle().Foreground(palette.Dim)
	var body strings.Builder
	var viewCursor *tea.Cursor
	body.WriteString(title)
	body.WriteString("\n")
	switch s.Phase {
	case ModelPullEntry:
		input := s.Input
		input.SetWidth(max(12, width-4))
		if hardwareCursor {
			input.SetVirtualCursor(false)
		}
		body.WriteString(input.View())
		if hardwareCursor {
			viewCursor = offsetCursor(input.Cursor(), 0, 1)
		}
		body.WriteString("\n")
		helper := "Local weights download to this machine. Cloud tags require Ollama sign-in."
		if compact {
			helper = boundedPullText("Local weights stay here · cloud needs sign-in", width, 2)
		}
		body.WriteString(muted.Render(helper))
	case ModelPullRunning:
		name := safeModelPullText(s.Name, maxModelPullNameCells)
		if compact {
			name = truncateDisplay(name, width)
		}
		body.WriteString(lipgloss.NewStyle().Foreground(palette.Text).Render(name))
		body.WriteString("\n")
		barWidth := max(12, width-4)
		bar := s.Progress
		bar.SetWidth(barWidth)
		if s.Total > 0 {
			value := float64(s.Completed) / float64(s.Total)
			if s.reducedMotion {
				body.WriteString(bar.ViewAs(value))
			} else {
				body.WriteString(bar.View())
			}
			body.WriteString("\n" + muted.Render(humanTransferBytes(s.Completed)+" / "+humanTransferBytes(s.Total)))
		} else {
			indicator := "…"
			if !s.reducedMotion {
				indicator = s.Spinner.View()
			}
			body.WriteString(indicator + " " + muted.Render("Connecting to Ollama…"))
		}
		if s.Status != "" {
			status := safeModelPullText(s.Status, maxModelPullStatusCells)
			if compact {
				status = boundedPullText(status, width, 1)
			}
			body.WriteString("\n" + muted.Render(status))
		}
	case ModelPullComplete:
		inventoryRefreshed := strings.EqualFold(s.Status, "Inventory refreshed")
		if inventoryRefreshed {
			// Preserve both the model identity and the stable receipt without
			// adding height or overflowing either the regular or narrow modal.
			availability, receipt := " is available", " · Inventory refreshed"
			if compact {
				availability, receipt = "", " · refreshed"
			}
			nameWidth := max(1, width-lipgloss.Width("✓ "+availability+receipt))
			message := "✓ " + truncateDisplay(safeModelPullText(s.Name, maxModelPullNameCells), nameWidth) + availability
			body.WriteString(lipgloss.NewStyle().Foreground(palette.Success).Render(message))
			body.WriteString(muted.Render(receipt))
			break
		}
		message := "✓ " + safeModelPullText(s.Name, maxModelPullNameCells) + " is available"
		if compact {
			message = truncateDisplay(message, width)
		}
		body.WriteString(lipgloss.NewStyle().Foreground(palette.Success).Render(message))
		if s.Status != "" && !strings.EqualFold(s.Status, "success") {
			status := safeModelPullText(s.Status, maxModelPullStatusCells)
			if compact {
				status = boundedPullText(status, width, 1)
			}
			body.WriteString("\n" + muted.Render(status))
		}
	case ModelPullFailed:
		message := "Pull failed"
		if s.Err != nil {
			message = safeModelPullText(s.Err.Error(), maxModelPullErrorCells)
		}
		message = "! " + message
		if compact {
			message = boundedPullText(message, width, 2)
		}
		body.WriteString(lipgloss.NewStyle().Foreground(palette.Error).Render(message))
		if s.Status != "" {
			status := safeModelPullText(s.Status, maxModelPullStatusCells)
			if compact {
				status = boundedPullText(status, width, 1)
			}
			body.WriteString("\n" + muted.Render(status))
		}
	}
	return body.String(), viewCursor
}

func safeModelPullText(value string, maxCells int) string {
	return truncateDisplay(sanitizeTerminalSingleLine(value), maxCells)
}

func safeModelPullError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(safeModelPullText(err.Error(), maxModelPullErrorCells))
}

func boundedPullText(value string, width, maxLines int) string {
	value = sanitizeTerminalSingleLine(value)
	if value == "" || width <= 0 || maxLines <= 0 {
		return ""
	}
	lines := strings.Split(wrapText(value, width), "\n")
	if len(lines) <= maxLines {
		return strings.Join(lines, "\n")
	}
	remaining := strings.Join(lines[maxLines-1:], " ")
	lines = lines[:maxLines]
	lines[maxLines-1] = truncateDisplay(remaining, width)
	return strings.Join(lines, "\n")
}

func (s *ModelPullState) ProgressText() string {
	if s == nil || s.Total <= 0 {
		return ""
	}
	return fmt.Sprintf("%d%%", int(float64(s.Completed)*100/float64(s.Total)))
}

func humanTransferBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	labels := [...]string{"KB", "MB", "GB", "TB"}
	size := float64(value)
	label := "B"
	for index := 0; index < len(labels) && size >= float64(unit); index++ {
		size /= float64(unit)
		label = labels[index]
	}
	return fmt.Sprintf("%.1f %s", size, label)
}
