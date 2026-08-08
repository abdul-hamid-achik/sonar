package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// sessionTitleJobMsg is delivered when a background title generation finishes.
// Token + SessionID gate the apply so a late result cannot rename a new session.
type sessionTitleJobMsg struct {
	Token     uint64
	SessionID int64
	Title     string
	Err       error
}

const (
	sessionTitleMaxEvalTokens  = 48
	sessionTitleJobTimeout     = 20 * time.Second
	sessionTitleSourceMaxRunes = 600
)

var sessionTitleSystemPrompt = strings.TrimSpace(`
You name coding-agent sessions for a developer.
Reply with ONLY a short session title.
Rules:
- 3 to 8 words
- Describe the user's goal or task, not the chat medium
- No quotes, no trailing punctuation, no emojis
- No prefixes like "Title:" or "Session:"
- Prefer concrete verbs/nouns over vague words like "chat" or "help"
`)

// sessionTitleGen owns single-flight background title inference so it can
// yield the ordinary ModelManager lease to a foreground turn (same contract
// as ICE auto-memory).
type sessionTitleGen struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (g *sessionTitleGen) cancelJob() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
	}
}

func (g *sessionTitleGen) stop() {
	if g == nil {
		return
	}
	g.cancelJob()
	g.wg.Wait()
}

// cancelSessionTitleGen yields inference resources to a foreground turn.
func (m *Model) cancelSessionTitleGen() {
	if m == nil {
		return
	}
	m.sessionTitleGen.cancelJob()
}

// stopSessionTitleGen cancels and joins any background title job (model switch /
// shutdown). Must not run on the Bubble Tea Update path if a job is active and
// might block; callers use it from command goroutines or shutdown.
func (m *Model) stopSessionTitleGen() {
	if m == nil {
		return
	}
	m.sessionTitleGen.stop()
}

// scheduleSessionTitleGen starts a one-shot background title job when the
// session still holds a provisional (first-line) title. Returns a tea.Cmd that
// delivers sessionTitleJobMsg.
func (m *Model) scheduleSessionTitleGen() tea.Cmd {
	if m == nil || !m.sessionTitleNeedsAI || m.sessionTitleAIDone {
		return nil
	}
	if m.sessionID <= 0 || m.sessionStore == nil || m.modelManager == nil {
		return nil
	}
	// Offline / no model: keep the provisional title.
	if strings.TrimSpace(m.model) == "" || m.providerOffline {
		return nil
	}

	userText, assistantText := m.sessionTitleSourceTexts()
	if strings.TrimSpace(userText) == "" {
		return nil
	}

	m.sessionTitleGenToken++
	token := m.sessionTitleGenToken
	sessionID := m.sessionID
	manager := m.modelManager
	workspace := m.sessionTitleWorkspaceLabel()
	mode := m.modeConfigs[m.presentedMode()].Label
	profile := strings.TrimSpace(m.agentProfile)

	m.sessionTitleGen.mu.Lock()
	if m.sessionTitleGen.cancel != nil {
		m.sessionTitleGen.cancel()
		m.sessionTitleGen.cancel = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionTitleJobTimeout)
	m.sessionTitleGen.cancel = cancel
	m.sessionTitleGen.wg.Add(1)
	m.sessionTitleGen.mu.Unlock()

	return func() tea.Msg {
		defer m.sessionTitleGen.wg.Done()
		defer cancel()
		title, err := generateSessionTitle(ctx, manager, sessionTitleRequest{
			Workspace: workspace,
			Mode:      mode,
			Profile:   profile,
			User:      userText,
			Assistant: assistantText,
		})
		return sessionTitleJobMsg{
			Token:     token,
			SessionID: sessionID,
			Title:     title,
			Err:       err,
		}
	}
}

type sessionTitleRequest struct {
	Workspace string
	Mode      string
	Profile   string
	User      string
	Assistant string
}

func generateSessionTitle(ctx context.Context, client llm.Client, req sessionTitleRequest) (string, error) {
	if client == nil {
		return "", fmt.Errorf("llm client is unavailable")
	}
	var response strings.Builder
	err := client.ChatStream(ctx, llm.ChatOptions{
		System:           sessionTitleSystemPrompt,
		Messages:         []llm.Message{{Role: "user", Content: buildSessionTitleUserPrompt(req)}},
		MaxEvalTokens:    sessionTitleMaxEvalTokens,
		DisableReasoning: true,
	}, func(chunk llm.StreamChunk) error {
		response.WriteString(chunk.Text)
		return nil
	})
	if err != nil {
		return "", err
	}
	title := parseGeneratedSessionTitle(response.String())
	if title == "" {
		return "", fmt.Errorf("empty generated session title")
	}
	return title, nil
}

func buildSessionTitleUserPrompt(req sessionTitleRequest) string {
	var b strings.Builder
	if req.Workspace != "" {
		fmt.Fprintf(&b, "Workspace: %s\n", req.Workspace)
	}
	if req.Mode != "" {
		fmt.Fprintf(&b, "Mode: %s\n", req.Mode)
	}
	if req.Profile != "" {
		fmt.Fprintf(&b, "Profile: %s\n", req.Profile)
	}
	b.WriteString("User request:\n")
	b.WriteString(boundSessionTitleSource(req.User))
	if strings.TrimSpace(req.Assistant) != "" {
		b.WriteString("\n\nAssistant reply (context only):\n")
		b.WriteString(boundSessionTitleSource(req.Assistant))
	}
	b.WriteString("\n\nSession title:")
	return b.String()
}

func boundSessionTitleSource(text string) string {
	text = sanitizeTerminalMultiline(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > sessionTitleSourceMaxRunes {
		return string(runes[:sessionTitleSourceMaxRunes-1]) + "…"
	}
	return text
}

// parseGeneratedSessionTitle extracts a usable title from model output.
func parseGeneratedSessionTitle(raw string) string {
	raw = strings.TrimSpace(sanitizeTerminalMultiline(raw))
	if raw == "" {
		return ""
	}
	// Take the first non-empty line; models sometimes add a blank lead.
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw = line
		break
	}
	// Strip common wrappers / prefixes.
	raw = strings.Trim(raw, "`\"'“”‘’")
	for _, prefix := range []string{"Title:", "title:", "Session:", "session:", "Name:", "name:"} {
		if after, ok := strings.CutPrefix(raw, prefix); ok {
			raw = strings.TrimSpace(after)
			raw = strings.Trim(raw, "`\"'“”‘’")
		}
	}
	// Drop trailing sentence punctuation the model often adds.
	raw = strings.TrimRightFunc(raw, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == ',' || r == ';' || r == ':'
	})
	raw = strings.Join(strings.Fields(raw), " ")
	if raw == "" || !utf8.ValidString(raw) {
		return ""
	}
	// Reject obvious non-titles.
	lower := strings.ToLower(raw)
	if lower == "none" || lower == "n/a" || lower == "untitled" {
		return ""
	}
	// Require at least one letter so pure symbols/numbers do not win.
	hasLetter := false
	for _, r := range raw {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return ""
	}
	return boundedSessionTitle(raw)
}

func (m *Model) sessionTitleWorkspaceLabel() string {
	if m == nil {
		return ""
	}
	dir := m.workspaceDir()
	if dir == "" {
		return ""
	}
	base := filepath.Base(dir)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return sanitizeTerminalSingleLine(base)
}

func (m *Model) sessionTitleSourceTexts() (userText, assistantText string) {
	if m == nil {
		return "", ""
	}
	// Prefer the latest user prompt and the latest assistant content in the
	// current session — enough context for a goal-shaped title without
	// shipping the whole transcript into a side generation.
	for i := len(m.entries) - 1; i >= 0; i-- {
		if userText == "" && m.entries[i].Kind == "user" {
			userText = strings.TrimSpace(m.entries[i].Content)
		}
		if assistantText == "" && m.entries[i].Kind == "assistant" {
			assistantText = strings.TrimSpace(m.entries[i].Content)
		}
		if userText != "" && assistantText != "" {
			break
		}
	}
	return userText, assistantText
}

// handleSessionTitleJob applies a successful background title when it still
// matches the session that requested it.
func (m *Model) handleSessionTitleJob(msg sessionTitleJobMsg) tea.Cmd {
	if m == nil {
		return nil
	}
	if msg.Token != m.sessionTitleGenToken || msg.SessionID == 0 || msg.SessionID != m.sessionID {
		return nil
	}
	if msg.Err != nil || strings.TrimSpace(msg.Title) == "" {
		// Keep provisional title; allow a later successful turn to retry.
		return nil
	}
	title := boundedSessionTitle(msg.Title)
	if title == "" || title == m.activeSessionTitle {
		if title != "" {
			m.sessionTitleNeedsAI = false
			m.sessionTitleAIDone = true
		}
		return nil
	}

	// Persist first so a failed write never leaves UI ahead of durable state.
	if m.sessionStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := m.sessionStore.UpdateSessionTitle(ctx, m.sessionID, title)
		cancel()
		if err != nil {
			if m.logger != nil {
				m.logger.Info("session title update failed", "err", err)
			}
			return nil
		}
	}
	m.activeSessionTitle = title
	m.sessionTitleNeedsAI = false
	m.sessionTitleAIDone = true
	// Quiet refresh — no chat noise; picker/runtime/status pick up the field.
	return nil
}
