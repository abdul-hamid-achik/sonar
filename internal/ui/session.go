package ui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
	"github.com/abdul-hamid-achik/sonar/internal/imageasset"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/sessionref"
)

var ErrSessionStateRevisionUnknown = errors.New("session state revision is unknown; reload the durable session before saving")

// SessionListItem represents a session in the list.
type SessionListItem struct {
	ID        int64  `json:"id"`
	PublicID  string `json:"public_id,omitempty"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
}

// SessionResumeInfo is the bounded durable identity shown by the CLI after
// Bubble Tea restores the terminal. Title is sanitized display metadata only;
// Handle remains the sole input to the canonical resume command.
type SessionResumeInfo struct {
	Handle string
	Title  string
}

// SessionResumeInfo returns the active session's canonical short handle when
// the model owns a session backed by the durable store. It is read-only and is
// intended for the successful interactive-exit message.
func (m *Model) SessionResumeInfo() (SessionResumeInfo, bool) {
	if m == nil || m.sessionStore == nil || m.sessionID <= 0 {
		return SessionResumeInfo{}, false
	}
	handle := sessionref.Format(m.sessionPublicID)
	if handle == "" {
		return SessionResumeInfo{}, false
	}
	return SessionResumeInfo{
		Handle: handle,
		Title:  boundedSessionTitle(m.activeSessionTitle),
	}, true
}

type persistedChatEntry struct {
	Kind      string `json:"kind"`
	Content   string `json:"content,omitempty"`
	Name      string `json:"name,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	ToolIndex int    `json:"tool_index,omitempty"`
	// ThinkingContent is decode-only compatibility for legacy snapshots.
	// Provider reasoning is never written or restored.
	ThinkingContent   string           `json:"thinking_content,omitempty"`
	ThinkingCollapsed bool             `json:"thinking_collapsed,omitempty"`
	Attachments       []imageasset.Ref `json:"attachments,omitempty"`
	BlockID           BlockID          `json:"block_id,omitempty"`
	TurnID            TurnID           `json:"turn_id,omitempty"`
	Revision          uint64           `json:"revision,omitempty"`
	Lifecycle         BlockLifecycle   `json:"lifecycle,omitempty"`
}

// persistedToolEntry deliberately excludes RawArgs and BeforeContent. Those
// ephemeral fields may contain secrets or multi-megabyte file snapshots and
// are not needed to render a completed tool card after resume.
type persistedToolEntry struct {
	ID             string                   `json:"id"`
	Name           string                   `json:"name"`
	Summary        string                   `json:"summary,omitempty"`
	Args           string                   `json:"args,omitempty"`
	Result         string                   `json:"result,omitempty"`
	ResultLanguage string                   `json:"result_language,omitempty"`
	OutputDetail   *OutputDetailDigest      `json:"output_detail,omitempty"`
	IsError        bool                     `json:"is_error,omitempty"`
	Status         ToolStatus               `json:"status"`
	StartTime      time.Time                `json:"start_time"`
	Duration       time.Duration            `json:"duration,omitempty"`
	Collapsed      bool                     `json:"collapsed,omitempty"`
	DiffLines      []DiffLine               `json:"diff_lines,omitempty"`
	Projection     ecosystem.ToolProjection `json:"projection,omitempty"`
	ExpertProgress *ExpertProgressState     `json:"expert_progress,omitempty"`
}

const (
	maxPersistedToolArgsBytes   = 4 * 1024
	maxPersistedToolResultBytes = 16 * 1024
	maxPersistedDiffBytes       = 64 * 1024
	maxPersistedDiffLines       = 2_000
	persistedDiffOmission       = "...[remaining diff omitted from saved session]"
)

type persistedSessionState struct {
	Version               int                      `json:"version"`
	Messages              []llm.Message            `json:"messages"`
	Entries               []persistedChatEntry     `json:"entries"`
	ToolEntries           []persistedToolEntry     `json:"tool_entries,omitempty"`
	Provider              *persistedProviderRef    `json:"provider,omitempty"`
	Mode                  Mode                     `json:"mode"`
	Model                 string                   `json:"model,omitempty"`
	ModelPinned           bool                     `json:"model_pinned,omitempty"`
	AgentProfile          string                   `json:"agent_profile,omitempty"`
	LoadedFile            string                   `json:"loaded_file,omitempty"`
	ManualLoadedContext   string                   `json:"manual_loaded_context,omitempty"`
	ManualSkills          []string                 `json:"manual_skills,omitempty"`
	SessionEvalTotal      int                      `json:"session_eval_total,omitempty"`
	SessionPromptTotal    int                      `json:"session_prompt_total,omitempty"`
	SessionTurnCount      int                      `json:"session_turn_count,omitempty"`
	ContextPromptFloor    agent.ContextPromptFloor `json:"context_prompt_floor,omitempty"`
	ExecutionCursor       int64                    `json:"execution_cursor,omitempty"`
	FileChanges           map[string]int           `json:"file_changes,omitempty"`
	Goal                  *goal.Snapshot           `json:"goal,omitempty"`
	CortexDecisionAttempt *cortexDecisionAttempt   `json:"cortex_decision_attempt,omitempty"`
}

type persistedProviderRef struct {
	Profile  string `json:"profile"`
	Model    string `json:"model,omitempty"`
	Locality string `json:"locality"`
}

// SessionProviderIdentity is the bounded provider provenance attached to a
// headless session snapshot. Remote is explicit so a session created through
// an OpenAI-compatible adapter can never be restored under a same-named local
// model (or vice versa) merely because the model strings happen to match.
type SessionProviderIdentity struct {
	Profile string
	Remote  bool
}

const (
	currentPersistedSessionVersion = 3
	persistedProviderLocal         = "local"
	persistedProviderRemote        = "remote"
)

func sessionTitle(prompt string) string {
	lines := strings.Split(prompt, "\n")
	titleSource := ""
	titleLine := -1
	for index, line := range lines {
		if strings.TrimSpace(line) != "" {
			titleSource = line
			titleLine = index
			break
		}
	}
	// Guided PLAN wraps the user's task in a host-authored prompt. Name the
	// session after the reviewed task instead of the internal instruction line.
	if strings.EqualFold(strings.TrimSpace(titleSource), "Plan the following task:") {
		for _, line := range lines[titleLine+1:] {
			label, value, found := strings.Cut(line, ":")
			if found && strings.EqualFold(strings.TrimSpace(label), "Task") {
				titleSource = value
				break
			}
		}
	}
	title := sanitizeTerminalSingleLine(titleSource)
	if title == "" {
		title = "Local agent session " + time.Now().Format("2006-01-02 15:04")
	}
	return boundedSessionTitle(title)
}

func boundedSessionTitle(title string) string {
	title = sanitizeTerminalSingleLine(title)
	if len([]rune(title)) > 72 {
		runes := []rune(title)
		title = string(runes[:69]) + "..."
	}
	return title
}

func sessionDisplayLabel(publicID, title string, titleLimit int) string {
	handle := sessionref.Format(publicID)
	if handle == "" {
		return ""
	}
	title = sanitizeTerminalSingleLine(title)
	if title == "" || titleLimit <= 0 {
		return handle
	}
	return handle + " · " + truncateDisplay(title, titleLimit)
}

func boundedSessionText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	const marker = "\n...[truncated in saved session]"
	cut := limit - len(marker)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + marker
}

func persistDiffLines(lines []DiffLine) []DiffLine {
	if len(lines) == 0 {
		return nil
	}
	capacity := len(lines)
	if capacity > maxPersistedDiffLines {
		capacity = maxPersistedDiffLines
	}
	result := make([]DiffLine, 0, capacity)
	// The limit applies to the encoded JSON array, not the in-memory string
	// lengths. Quotes, backslashes, controls, and a few Unicode code points can
	// expand during encoding, so charge the exact object encoding and array
	// separators while always reserving a typed omission marker.
	encodedBytes := 2 // opening and closing JSON array brackets
	omission := DiffLine{Kind: DiffOmitted, Content: persistedDiffOmission}
	omissionBytes := encodedDiffLineBytes(omission)
	for i, line := range lines {
		if len(result) >= maxPersistedDiffLines-1 && i < len(lines)-1 {
			result = append(result, omission)
			break
		}
		content := line.Content
		if line.Kind == DiffNoNewline {
			content = diffNoNewlineContent
		}
		persisted := DiffLine{
			Kind: line.Kind, Content: content, OldLine: max(0, line.OldLine), NewLine: max(0, line.NewLine),
		}
		if line.Hunk != nil {
			hunk := *line.Hunk
			hunk.OldStart = max(0, hunk.OldStart)
			hunk.OldCount = max(0, hunk.OldCount)
			hunk.NewStart = max(0, hunk.NewStart)
			hunk.NewCount = max(0, hunk.NewCount)
			persisted.Hunk = &hunk
		}
		lineBytes := encodedDiffLineBytes(persisted)
		lineCost := lineBytes
		if len(result) > 0 {
			lineCost++ // JSON array comma
		}
		needsOmission := i < len(lines)-1
		reservedBytes := 0
		if needsOmission {
			reservedBytes = omissionBytes + 1 // comma before the omission marker
		}
		if encodedBytes+lineCost+reservedBytes > maxPersistedDiffBytes {
			omissionCost := omissionBytes
			if len(result) > 0 {
				omissionCost++
			}
			if encodedBytes+omissionCost <= maxPersistedDiffBytes {
				result = append(result, omission)
			}
			break
		}
		result = append(result, persisted)
		encodedBytes += lineCost
	}
	return result
}

func encodedDiffLineBytes(line DiffLine) int {
	encoded, err := json.Marshal(line)
	if err != nil {
		// DiffLine contains only JSON-supported scalar fields. Returning a value
		// beyond the ceiling is a fail-closed fallback if that contract changes.
		return maxPersistedDiffBytes + 1
	}
	return len(encoded)
}

func persistToolEntries(entries []ToolEntry) []persistedToolEntry {
	result := make([]persistedToolEntry, len(entries))
	for i, entry := range entries {
		args := boundedSessionText(entry.Args, maxPersistedToolArgsBytes)
		toolResult := boundedSessionText(sanitizeTerminalMultiline(entry.Result), maxPersistedToolResultBytes)
		resultLanguage := normalizeTrustedResultLanguage(entry.ResultLanguage)
		progress := sanitizeExpertProgressState(entry.ExpertProgress, entry.Status != ToolStatusRunning)
		if isExpertConsultTool(entry.Name) {
			args = ""
			toolResult = ""
			resultLanguage = ""
		}
		var outputDetail *OutputDetailDigest
		if !isExpertConsultTool(entry.Name) &&
			entry.OutputDetail.Digest != (OutputDetailDigest{}) &&
			entry.OutputDetail.Digest.Valid() {
			digest := entry.OutputDetail.Digest
			outputDetail = &digest
		}
		result[i] = persistedToolEntry{
			ID:             entry.ID,
			Name:           entry.Name,
			Summary:        boundedToolCardSummary(entry.Summary),
			Args:           args,
			Result:         toolResult,
			ResultLanguage: resultLanguage,
			OutputDetail:   outputDetail,
			IsError:        entry.IsError,
			Status:         entry.Status,
			StartTime:      entry.StartTime,
			Duration:       entry.Duration,
			Collapsed:      entry.Collapsed,
			DiffLines:      persistDiffLines(entry.DiffLines),
			Projection:     entry.Projection.Normalize(),
			ExpertProgress: progress,
		}
	}
	return result
}

func restoreToolEntries(entries []persistedToolEntry) []ToolEntry {
	result := make([]ToolEntry, len(entries))
	for i, entry := range entries {
		wasRunning := entry.Status == ToolStatusRunning
		progress := sanitizeExpertProgressState(entry.ExpertProgress, !wasRunning)
		args := entry.Args
		toolResult := boundedSessionText(sanitizeTerminalMultiline(entry.Result), maxPersistedToolResultBytes)
		resultLanguage := normalizeTrustedResultLanguage(entry.ResultLanguage)
		if isExpertConsultTool(entry.Name) {
			args = ""
			toolResult = ""
			resultLanguage = ""
		}
		var outputDetail OutputDetailReceipt
		if !isExpertConsultTool(entry.Name) && entry.OutputDetail != nil && entry.OutputDetail.Valid() {
			outputDetail.Digest = *entry.OutputDetail
		}
		restored := ToolEntry{
			ID:             entry.ID,
			Name:           entry.Name,
			Summary:        restoredToolSummary(entry),
			Args:           args,
			Result:         toolResult,
			ResultLanguage: resultLanguage,
			OutputDetail:   outputDetail,
			IsError:        entry.IsError,
			Status:         entry.Status,
			StartTime:      entry.StartTime,
			Duration:       entry.Duration,
			Collapsed:      entry.Collapsed,
			DiffLines:      persistDiffLines(entry.DiffLines),
			Projection:     entry.Projection.Normalize(),
			ExpertProgress: progress,
		}
		if isExpertConsultTool(entry.Name) {
			if progress != nil {
				restored.Summary = boundedToolCardSummary(progress.summary())
			} else {
				restored.Summary = "expert consultation"
			}
		}
		settleInterruptedToolEntry(&restored)
		if wasRunning {
			restored.ExpertProgress = nil
		}
		result[i] = restored
	}
	return result
}

// settleInterruptedToolEntry makes restore idempotent. A running card cannot
// resume provider work, and changing only its display state would leave the
// persisted semantic projection as running/pending. A later restore could then
// paint that stale projection as live again. Settle both representations at
// the same boundary and preserve only bounded routing identity.
func settleInterruptedToolEntry(entry *ToolEntry) {
	if entry == nil {
		return
	}
	projection := entry.Projection.Normalize()
	interrupted := entry.Status == ToolStatusRunning ||
		(entry.Status == ToolStatusError && (projection.Transport == ecosystem.TransportRunning || projection.Domain == ecosystem.DomainPending))
	if !interrupted {
		return
	}
	if projection.Transport == "" && projection.Domain == "" {
		projection = ecosystem.ProjectToolCall(entry.Name, nil)
	}
	projection.Transport = ecosystem.TransportFailed
	projection.Domain = ecosystem.DomainUnknown
	projection.Evidence = ecosystem.EvidenceNone
	entry.Status = ToolStatusError
	entry.IsError = true
	entry.Result = "Interrupted before session was saved"
	entry.Projection = projection.Normalize()
}

// restoredToolSummary preserves the semantic summary written by current
// snapshots. Version-1 snapshots created before summary persistence omitted
// the field, so their already-bounded display arguments become compact context
// instead of leaving a restored receipt with only a generic action label.
func restoredToolSummary(entry persistedToolEntry) string {
	if summary := boundedToolCardSummary(entry.Summary); summary != "" {
		return summary
	}
	return boundedToolCardSummary(entry.Args)
}

func encodeSessionState(m *Model) (string, error) {
	if err := m.reconcileTranscriptEntries(); err != nil {
		return "", fmt.Errorf("reconcile transcript identity: %w", err)
	}
	entries := make([]persistedChatEntry, len(m.entries))
	for i, entry := range m.entries {
		entries[i] = persistedChatEntry{
			Kind:              entry.Kind,
			Content:           entry.Content,
			Name:              entry.Name,
			IsError:           entry.IsError,
			ToolIndex:         entry.ToolIndex,
			ThinkingCollapsed: entry.ThinkingCollapsed,
			Attachments:       append([]imageasset.Ref(nil), entry.Attachments...),
			BlockID:           entry.BlockID,
			TurnID:            entry.TurnID,
			Revision:          entry.Revision,
			Lifecycle:         entry.Lifecycle,
		}
	}
	manualSkills := m.manualSkills
	if manualSkills == nil {
		manualSkills = subtractSkillNames(m.activeSkillNames(), m.profileSkills)
	}
	var goalSnapshot *goal.Snapshot
	if m.goalRuntime != nil {
		snapshot, err := m.goalRuntime.Snapshot(context.Background())
		if err != nil {
			return "", fmt.Errorf("snapshot goal runtime: %w", err)
		}
		goalSnapshot = &snapshot
	}
	state := persistedSessionState{
		Version:               currentPersistedSessionVersion,
		Messages:              m.agent.Messages(),
		Entries:               entries,
		ToolEntries:           persistToolEntries(m.toolEntries),
		Provider:              m.persistedProviderReference(),
		Mode:                  m.mode,
		Model:                 m.model,
		ModelPinned:           m.modelPinned,
		AgentProfile:          m.agentProfile,
		LoadedFile:            m.loadedFile,
		ManualLoadedContext:   m.manualLoadedContext,
		ManualSkills:          append([]string(nil), manualSkills...),
		SessionEvalTotal:      m.sessionEvalTotal,
		SessionPromptTotal:    m.sessionPromptTotal,
		SessionTurnCount:      m.sessionTurnCount,
		ContextPromptFloor:    m.agent.ContextPromptFloor(),
		ExecutionCursor:       m.executionCursor,
		FileChanges:           m.fileChanges,
		Goal:                  goalSnapshot,
		CortexDecisionAttempt: cloneCortexDecisionAttempt(m.cortexDecisionAttempt),
	}
	return marshalPersistedSessionState(state)
}

// initializeSessionStateRevision establishes the exact generation a Model may
// replace. New sessions use zero; restored and embedded sessions must supply a
// revision read from SessionStateRecord. It never consults storage or guesses a
// generation after a conflict.
func (m *Model) initializeSessionStateRevision(revision int64) error {
	if revision < 0 {
		return fmt.Errorf("invalid session state revision %d", revision)
	}
	m.sessionStateMu.Lock()
	m.sessionStateRevision = revision
	m.sessionStateRevisionKnown = true
	m.sessionStatePersistenceDirty = false
	m.sessionStateMu.Unlock()
	return nil
}

func (m *Model) resetSessionStateRevision() {
	m.sessionStateMu.Lock()
	m.sessionStateRevision = 0
	m.sessionStateRevisionKnown = false
	m.sessionStatePersistenceDirty = false
	m.sessionStateMu.Unlock()
}

func validateLoadedSessionStateRecord(sessionID int64, record db.SessionStateRecord) error {
	if sessionID <= 0 || record.SessionID != sessionID {
		return fmt.Errorf("session state record belongs to session %d, not %d", record.SessionID, sessionID)
	}
	if record.Revision < 0 {
		return fmt.Errorf("session state record has invalid revision %d", record.Revision)
	}
	return nil
}

// persistSessionState is the only production interactive session writer. A
// stale revision is never refreshed and retried with caller-owned JSON: the
// Model forgets its authority and remains dirty until an explicit durable
// session reload (or a future coordinator-result hydration) installs a record.
func (m *Model) persistSessionState(ctx context.Context) error {
	stateJSON, err := encodeSessionState(m)
	if err != nil {
		m.sessionStateMu.Lock()
		m.sessionStatePersistenceDirty = true
		m.sessionStateMu.Unlock()
		return err
	}

	m.sessionStateMu.Lock()
	defer m.sessionStateMu.Unlock()
	if m.sessionStore == nil || m.sessionID <= 0 {
		m.sessionStatePersistenceDirty = true
		return fmt.Errorf("durable session is unavailable")
	}
	if !m.sessionStateRevisionKnown {
		m.sessionStatePersistenceDirty = true
		return ErrSessionStateRevisionUnknown
	}
	expectedRevision := m.sessionStateRevision
	record, err := m.sessionStore.SaveSessionStateCAS(ctx, m.sessionID, expectedRevision, stateJSON)
	if err != nil {
		m.sessionStatePersistenceDirty = true
		if errors.Is(err, db.ErrSessionStateConflict) {
			m.sessionStateRevisionKnown = false
		}
		return err
	}
	if record.SessionID != m.sessionID || record.Revision <= expectedRevision {
		m.sessionStateRevisionKnown = false
		m.sessionStatePersistenceDirty = true
		return fmt.Errorf("session state CAS returned invalid record session=%d revision=%d", record.SessionID, record.Revision)
	}
	m.sessionStateRevision = record.Revision
	m.sessionStatePersistenceDirty = false
	return nil
}

// EncodeHeadlessSessionState creates a current-version snapshot that the interactive
// session picker can restore after a non-interactive run. Tool messages remain
// in model history, while the visible transcript stays focused on user and
// assistant text because headless mode has no persisted ToolCard state.
func EncodeHeadlessSessionState(messages []llm.Message, model, agentProfile string, modelPinned bool, executionCursor int64) (string, error) {
	return EncodeHeadlessSessionStateWithContextFloor(
		messages,
		model,
		agentProfile,
		modelPinned,
		executionCursor,
		agent.ContextPromptFloor{},
	)
}

// EncodeHeadlessSessionStateWithContextFloor preserves the exact bounded
// provider-receipt floor when a headless session may later be resumed.
func EncodeHeadlessSessionStateWithContextFloor(messages []llm.Message, model, agentProfile string, modelPinned bool, executionCursor int64, floor agent.ContextPromptFloor) (string, error) {
	return EncodeHeadlessSessionStateWithProvider(
		messages,
		model,
		agentProfile,
		modelPinned,
		executionCursor,
		floor,
		SessionProviderIdentity{Profile: config.ProviderTypeOllama},
	)
}

// EncodeHeadlessSessionStateWithProvider preserves provider provenance for a
// session created outside the interactive Model.
func EncodeHeadlessSessionStateWithProvider(
	messages []llm.Message,
	model, agentProfile string,
	modelPinned bool,
	executionCursor int64,
	floor agent.ContextPromptFloor,
	provider SessionProviderIdentity,
) (string, error) {
	if executionCursor < 0 {
		return "", fmt.Errorf("encode session state: execution cursor must not be negative")
	}
	entries := make([]persistedChatEntry, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "user", "assistant":
			if message.Content != "" {
				entries = append(entries, persistedChatEntry{
					Kind:    message.Role,
					Content: boundedSessionText(message.Content, maxPersistedToolResultBytes),
				})
			}
		}
	}
	return marshalPersistedSessionState(persistedSessionState{
		Version:            currentPersistedSessionVersion,
		Messages:           append([]llm.Message(nil), messages...),
		Entries:            entries,
		Provider:           persistedProviderReferenceForHeadless(provider, model),
		Mode:               ModeNormal,
		Model:              model,
		ModelPinned:        modelPinned,
		AgentProfile:       agentProfile,
		ContextPromptFloor: floor,
		ExecutionCursor:    executionCursor,
	})
}

// EncodeHeadlessGoalSessionState persists one headless turn together with the
// exact Goal Runtime snapshot that admitted and settled it.
func EncodeHeadlessGoalSessionState(messages []llm.Message, model, agentProfile string, modelPinned bool, executionCursor int64, snapshot goal.Snapshot) (string, error) {
	return EncodeHeadlessGoalSessionStateWithContextFloor(
		messages,
		model,
		agentProfile,
		modelPinned,
		executionCursor,
		snapshot,
		agent.ContextPromptFloor{},
	)
}

// EncodeHeadlessGoalSessionStateWithContextFloor persists a goal turn and its
// exact bounded provider-receipt floor as one session CAS payload.
func EncodeHeadlessGoalSessionStateWithContextFloor(messages []llm.Message, model, agentProfile string, modelPinned bool, executionCursor int64, snapshot goal.Snapshot, floor agent.ContextPromptFloor) (string, error) {
	return EncodeHeadlessGoalSessionStateWithProvider(
		messages,
		model,
		agentProfile,
		modelPinned,
		executionCursor,
		snapshot,
		floor,
		SessionProviderIdentity{Profile: config.ProviderTypeOllama},
	)
}

// EncodeHeadlessGoalSessionStateWithProvider is the provider-bound Goal
// Runtime variant of EncodeHeadlessSessionStateWithProvider.
func EncodeHeadlessGoalSessionStateWithProvider(
	messages []llm.Message,
	model, agentProfile string,
	modelPinned bool,
	executionCursor int64,
	snapshot goal.Snapshot,
	floor agent.ContextPromptFloor,
	provider SessionProviderIdentity,
) (string, error) {
	if executionCursor < 0 {
		return "", fmt.Errorf("encode goal session state: execution cursor must not be negative")
	}
	if snapshot.SessionID <= 0 {
		return "", fmt.Errorf("encode goal session state: goal session ID must be positive")
	}
	entries := make([]persistedChatEntry, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "user", "assistant":
			if message.Content != "" {
				entries = append(entries, persistedChatEntry{Kind: message.Role, Content: boundedSessionText(message.Content, maxPersistedToolResultBytes)})
			}
		}
	}
	copy := snapshot
	return marshalPersistedSessionState(persistedSessionState{
		Version: currentPersistedSessionVersion, Messages: append([]llm.Message(nil), messages...), Entries: entries,
		Provider: persistedProviderReferenceForHeadless(provider, model),
		Mode:     ModeAuto, Model: model, ModelPinned: modelPinned, AgentProfile: agentProfile,
		ContextPromptFloor: floor, ExecutionCursor: executionCursor, Goal: &copy,
	})
}

func persistedProviderReferenceForHeadless(identity SessionProviderIdentity, model string) *persistedProviderRef {
	locality := persistedProviderLocal
	if identity.Remote {
		locality = persistedProviderRemote
	}
	return &persistedProviderRef{
		Profile:  identity.Profile,
		Model:    model,
		Locality: locality,
	}
}

func marshalPersistedSessionState(state persistedSessionState) (string, error) {
	state.Messages = agent.SanitizeMessagesForPersistence(state.Messages)
	state.ToolEntries = sanitizePersistedToolEntryArgs(state.ToolEntries)
	state.Entries = withoutPersistedThinkingContent(state.Entries)
	if len(state.Entries) > 0 && !persistedTranscriptHasIdentity(state.Entries) {
		// Internal/headless callers construct the DTO directly. Complete their
		// identity before encoding; a decoded v3 envelope never gets this
		// compatibility path and therefore fails closed when identity is absent.
		hydrateLegacyTranscriptIdentity(&state)
	}
	if err := validatePersistedTranscriptIdentity(state.Entries); err != nil {
		return "", err
	}
	if err := validatePersistedToolTranscriptState(state); err != nil {
		return "", err
	}
	if err := validatePersistedProviderReference(state); err != nil {
		return "", err
	}
	if err := validatePersistedContextPromptFloor(state); err != nil {
		return "", err
	}
	if err := validatePersistedImageAttachments(state.Entries); err != nil {
		return "", err
	}
	if err := validatePersistedImageProjection(state.Messages, state.Entries); err != nil {
		return "", err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", fmt.Errorf("encode session state: %w", err)
	}
	return string(data), nil
}

func decodeSessionState(raw string) (persistedSessionState, error) {
	var state persistedSessionState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return state, fmt.Errorf("decode session state: %w", err)
	}
	if state.ExecutionCursor < 0 {
		return state, fmt.Errorf("invalid execution cursor %d", state.ExecutionCursor)
	}
	state, err := migratePersistedSessionState(state)
	if err != nil {
		return state, err
	}
	state.Messages = agent.SanitizeMessagesForPersistence(state.Messages)
	state.ToolEntries = sanitizePersistedToolEntryArgs(state.ToolEntries)
	state.Entries = withoutPersistedThinkingContent(state.Entries)
	if err := validatePersistedTranscriptIdentity(state.Entries); err != nil {
		return state, err
	}
	if err := validatePersistedToolTranscriptState(state); err != nil {
		return state, err
	}
	if err := validatePersistedProviderReference(state); err != nil {
		return state, err
	}
	if err := validatePersistedContextPromptFloor(state); err != nil {
		return state, err
	}
	if err := validatePersistedImageAttachments(state.Entries); err != nil {
		return state, err
	}
	if err := validatePersistedImageProjection(state.Messages, state.Entries); err != nil {
		return state, err
	}
	return state, nil
}

func validatePersistedContextPromptFloor(state persistedSessionState) error {
	if err := state.ContextPromptFloor.Validate(); err != nil {
		return fmt.Errorf("saved context prompt floor: %w", err)
	}
	if state.ContextPromptFloor.Tokens == 0 {
		return nil
	}
	if config.CanonicalModelName(state.ContextPromptFloor.Model) != config.CanonicalModelName(state.Model) {
		return fmt.Errorf("saved context prompt floor model does not match session model")
	}
	return nil
}

func validatePersistedImageAttachments(entries []persistedChatEntry) error {
	for entryIndex, entry := range entries {
		if len(entry.Attachments) > maxPendingImages {
			return fmt.Errorf("session entry %d has %d image attachments (limit %d)", entryIndex, len(entry.Attachments), maxPendingImages)
		}
		if len(entry.Attachments) > 0 && entry.Kind != "user" {
			return fmt.Errorf("session entry %d attaches images to non-user content", entryIndex)
		}
		seen := make(map[string]struct{}, len(entry.Attachments))
		for attachmentIndex, attachment := range entry.Attachments {
			if err := attachment.Validate(); err != nil {
				return fmt.Errorf("session entry %d image %d: %w", entryIndex, attachmentIndex, err)
			}
			if _, duplicate := seen[attachment.Digest]; duplicate {
				return fmt.Errorf("session entry %d repeats image %s", entryIndex, attachment.Handle())
			}
			seen[attachment.Digest] = struct{}{}
		}
	}
	return nil
}

func validatePersistedImageProjection(messages []llm.Message, entries []persistedChatEntry) error {
	messageGroups := make([]persistedImageGroup, 0)
	for messageIndex, message := range messages {
		if len(message.Images) == 0 {
			continue
		}
		if message.Role != "user" {
			return fmt.Errorf("session message %d attaches images to non-user content", messageIndex)
		}
		if len(message.Images) > maxPendingImages {
			return fmt.Errorf("session message %d has %d image attachments (limit %d)", messageIndex, len(message.Images), maxPendingImages)
		}
		group := make([]imageasset.Ref, len(message.Images))
		seen := make(map[string]struct{}, len(message.Images))
		for imageIndex, image := range message.Images {
			if err := image.ValidateReference(); err != nil {
				return fmt.Errorf("session message %d image %d: %w", messageIndex, imageIndex, err)
			}
			group[imageIndex] = imageasset.Ref{
				Digest: image.SHA256, MIMEType: image.MediaType, Name: image.Name,
				SizeBytes: image.Size, Width: image.Width, Height: image.Height,
			}
			if _, duplicate := seen[group[imageIndex].Digest]; duplicate {
				return fmt.Errorf("session message %d repeats image %s", messageIndex, group[imageIndex].Handle())
			}
			seen[group[imageIndex].Digest] = struct{}{}
		}
		messageGroups = append(messageGroups, persistedImageGroup{Content: message.Content, Refs: group})
	}

	entryGroups := make([]persistedImageGroup, 0)
	for _, entry := range entries {
		if len(entry.Attachments) > 0 {
			entryGroups = append(entryGroups, persistedImageGroup{Content: entry.Content, Refs: entry.Attachments})
		}
	}
	if len(messageGroups) != len(entryGroups) {
		return fmt.Errorf("session image transcript projection is inconsistent")
	}
	for groupIndex := range messageGroups {
		if messageGroups[groupIndex].Content != entryGroups[groupIndex].Content || len(messageGroups[groupIndex].Refs) != len(entryGroups[groupIndex].Refs) {
			return fmt.Errorf("session image transcript projection is inconsistent")
		}
		for imageIndex := range messageGroups[groupIndex].Refs {
			if messageGroups[groupIndex].Refs[imageIndex] != entryGroups[groupIndex].Refs[imageIndex] {
				return fmt.Errorf("session image transcript projection is inconsistent")
			}
		}
	}
	return nil
}

type persistedImageGroup struct {
	Content string
	Refs    []imageasset.Ref
}

// reconcileVisibleImageProjection removes image badges whose provider message
// was summarized away during context compaction. The visible prose transcript
// remains intact, while durable image metadata continues to be an exact
// projection of the agent history that will be restored on restart.
func (m *Model) reconcileVisibleImageProjection(messages []llm.Message) error {
	desired := imageReferenceGroups(messages)
	next := 0
	for index := range m.entries {
		if len(m.entries[index].Attachments) == 0 {
			continue
		}
		if next < len(desired) && m.entries[index].Content == desired[next].Content && reflect.DeepEqual(m.entries[index].Attachments, desired[next].Refs) {
			next++
			continue
		}
		m.entries[index].Attachments = nil
	}
	if next != len(desired) {
		return fmt.Errorf("retained image messages have no matching visible user entry")
	}
	m.invalidateEntryCache()
	return nil
}

func imageReferenceGroups(messages []llm.Message) []persistedImageGroup {
	groups := make([]persistedImageGroup, 0)
	for _, message := range messages {
		if message.Role != "user" || len(message.Images) == 0 {
			continue
		}
		group := make([]imageasset.Ref, len(message.Images))
		for index, image := range message.Images {
			group[index] = imageasset.Ref{
				Digest: image.SHA256, MIMEType: image.MediaType, Name: image.Name,
				SizeBytes: image.Size, Width: image.Width, Height: image.Height,
			}
		}
		groups = append(groups, persistedImageGroup{Content: message.Content, Refs: group})
	}
	return groups
}

func sanitizePersistedToolEntryArgs(entries []persistedToolEntry) []persistedToolEntry {
	if len(entries) == 0 {
		return nil
	}
	result := append([]persistedToolEntry(nil), entries...)
	for index := range result {
		// Apply the same exact serialized diff ceiling to decoded and direct
		// in-memory snapshots, not only snapshots produced by encodeSessionState.
		result[index].DiffLines = persistDiffLines(result[index].DiffLines)
		if isExpertConsultTool(result[index].Name) {
			result[index].Args = ""
			result[index].Result = ""
			result[index].ResultLanguage = ""
			result[index].ExpertProgress = sanitizeExpertProgressState(
				result[index].ExpertProgress, result[index].Status != ToolStatusRunning,
			)
			if result[index].ExpertProgress != nil {
				result[index].Summary = boundedToolCardSummary(result[index].ExpertProgress.summary())
			} else {
				result[index].Summary = "expert consultation"
			}
			continue
		}
		if !agent.ToolArgumentsRequirePrivacy(result[index].Name) {
			continue
		}
		projection := result[index].Projection.Normalize()
		routeArgs := make(map[string]any, 3)
		if projection.Route.Server != "" {
			routeArgs["server"] = projection.Route.Server
		}
		if projection.Route.Tool != "" {
			routeArgs["tool"] = projection.Route.Tool
		}
		if projection.Route.CallID != "" {
			routeArgs["call_id"] = projection.Route.CallID
		}
		result[index].Args = agent.FormatToolArgsForTool(result[index].Name, routeArgs)
	}
	return result
}

func withoutPersistedThinkingContent(entries []persistedChatEntry) []persistedChatEntry {
	if len(entries) == 0 {
		return nil
	}
	result := append([]persistedChatEntry(nil), entries...)
	for index := range result {
		result[index].ThinkingContent = ""
	}
	return result
}

func validatePersistedTranscriptIdentity(entries []persistedChatEntry) error {
	if len(entries) > 0 && !persistedTranscriptHasIdentity(entries) {
		return fmt.Errorf("session transcript identity is missing")
	}

	seen := make(map[BlockID]struct{}, len(entries))
	for index, entry := range entries {
		if entry.BlockID == "" || entry.Revision == 0 {
			return fmt.Errorf("session transcript identity is mixed or incomplete at entry %d", index)
		}
		if !entry.BlockID.Valid() {
			return fmt.Errorf("session entry %d has invalid block ID", index)
		}
		if _, duplicate := seen[entry.BlockID]; duplicate {
			return fmt.Errorf("session entry %d repeats block ID %q", index, entry.BlockID)
		}
		seen[entry.BlockID] = struct{}{}
		if !entry.Lifecycle.Valid() {
			return fmt.Errorf("session entry %d has invalid lifecycle %d", index, entry.Lifecycle)
		}
		if !validPersistedEntryLifecycle(entry.Kind, entry.Lifecycle) {
			return fmt.Errorf(
				"session entry %d kind %q has incompatible lifecycle %d",
				index,
				entry.Kind,
				entry.Lifecycle,
			)
		}
		if persistedEntryNeedsTurn(entry.Kind) && entry.TurnID == "" {
			return fmt.Errorf("session entry %d is missing a turn ID", index)
		}
		if entry.TurnID != "" && !entry.TurnID.Valid() {
			return fmt.Errorf("session entry %d has invalid turn ID", index)
		}
	}
	return nil
}

func persistedTranscriptHasIdentity(entries []persistedChatEntry) bool {
	for _, entry := range entries {
		if entry.BlockID != "" || entry.TurnID != "" || entry.Revision != 0 {
			return true
		}
	}
	return false
}

func validPersistedEntryLifecycle(kind string, lifecycle BlockLifecycle) bool {
	switch kind {
	case "user", "assistant", "system":
		return lifecycle == BlockSettled
	case "error":
		return lifecycle == BlockFailed
	case "tool_group":
		return lifecycle == BlockLive || lifecycle == BlockSettling ||
			lifecycle == BlockSettled || lifecycle == BlockFailed ||
			lifecycle == BlockCancelled
	default:
		return false
	}
}

func persistedEntryNeedsTurn(kind string) bool {
	switch kind {
	case "user", "assistant", "tool_group":
		return true
	default:
		return false
	}
}

// validatePersistedToolTranscriptState checks the cross-record invariants that
// entry-local identity validation cannot prove. A v3 tool block must point at
// an admitted ToolEntry whose status agrees with the block lifecycle; otherwise
// restore would accept the envelope and fail later in the renderer after part
// of the session had already been committed.
func validatePersistedToolTranscriptState(state persistedSessionState) error {
	for index, tool := range state.ToolEntries {
		switch tool.Status {
		case ToolStatusRunning, ToolStatusDone, ToolStatusError:
		case ToolStatusCancelled:
			if err := validatePersistedCancelledToolEntry(tool); err != nil {
				return fmt.Errorf("session tool entry %d: %w", index, err)
			}
		default:
			return fmt.Errorf("session tool entry %d has invalid status %d", index, tool.Status)
		}
		if tool.OutputDetail != nil {
			if isExpertConsultTool(tool.Name) {
				return fmt.Errorf("session expert tool entry %d contains output detail", index)
			}
			if !tool.OutputDetail.Valid() {
				return fmt.Errorf("session tool entry %d has invalid output detail digest", index)
			}
		}
	}
	for index, entry := range state.Entries {
		if entry.Kind != "tool_group" {
			continue
		}
		if entry.ToolIndex < 0 || entry.ToolIndex >= len(state.ToolEntries) {
			return fmt.Errorf(
				"session entry %d references missing tool index %d",
				index,
				entry.ToolIndex,
			)
		}
		var expected BlockLifecycle
		switch state.ToolEntries[entry.ToolIndex].Status {
		case ToolStatusRunning:
			expected = BlockLive
		case ToolStatusDone:
			expected = BlockSettled
		case ToolStatusError:
			expected = BlockFailed
		case ToolStatusCancelled:
			expected = BlockCancelled
		}
		if entry.Lifecycle != expected {
			return fmt.Errorf(
				"session entry %d tool lifecycle %d does not match status %d",
				index,
				entry.Lifecycle,
				state.ToolEntries[entry.ToolIndex].Status,
			)
		}
	}
	return nil
}

func validatePersistedCancelledToolEntry(tool persistedToolEntry) error {
	if tool.IsError {
		return errors.New("cancelled tool cannot be marked as an error")
	}
	expectedResult := cancelledToolResult
	if isExpertConsultTool(tool.Name) {
		// Expert prose never crosses the persistence boundary, including the
		// host-owned cancellation sentence used by the live card.
		expectedResult = ""
	}
	if tool.Result != expectedResult {
		return errors.New("cancelled tool has non-canonical result output")
	}
	if tool.ResultLanguage != "" {
		return errors.New("cancelled tool cannot retain a result language")
	}
	if tool.OutputDetail != nil {
		return errors.New("cancelled tool cannot retain output detail")
	}
	if len(tool.DiffLines) != 0 {
		return errors.New("cancelled tool cannot retain diff output")
	}
	if tool.ExpertProgress != nil {
		return errors.New("cancelled tool cannot retain expert progress")
	}
	if err := validateCanonicalCancelledToolProjection(tool.Projection); err != nil {
		return err
	}
	return nil
}

func hydrateLegacyTranscriptIdentity(state *persistedSessionState) {
	if state == nil {
		return
	}
	var currentTurn TurnID
	for index := range state.Entries {
		entry := &state.Entries[index]
		entry.BlockID = deterministicLegacyBlockID(index, entry.Kind)
		switch {
		case entry.Kind == "user":
			currentTurn = deterministicLegacyTurnID(index, entry.Kind)
			entry.TurnID = currentTurn
		case persistedEntryNeedsTurn(entry.Kind):
			if currentTurn == "" {
				currentTurn = deterministicLegacyTurnID(index, entry.Kind)
			}
			entry.TurnID = currentTurn
		default:
			entry.TurnID = ""
		}
		entry.Revision = 1
		entry.Lifecycle = legacyPersistedEntryLifecycle(*state, *entry)
		entry.ThinkingContent = ""
	}
}

func deterministicLegacyBlockID(ordinal int, kind string) BlockID {
	return BlockID(deterministicLegacyTranscriptID("blk_legacy_", ordinal, kind))
}

func deterministicLegacyTurnID(ordinal int, kind string) TurnID {
	return TurnID(deterministicLegacyTranscriptID("turn_legacy_", ordinal, kind))
}

func deterministicLegacyTranscriptID(prefix string, ordinal int, kind string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", ordinal, kind)))
	return fmt.Sprintf("%s%x", prefix, sum[:16])
}

func legacyPersistedEntryLifecycle(state persistedSessionState, entry persistedChatEntry) BlockLifecycle {
	switch entry.Kind {
	case "error":
		return BlockFailed
	case "tool_group":
		if entry.ToolIndex < 0 || entry.ToolIndex >= len(state.ToolEntries) {
			return BlockFailed
		}
		switch state.ToolEntries[entry.ToolIndex].Status {
		case ToolStatusRunning:
			return BlockLive
		case ToolStatusError:
			return BlockFailed
		case ToolStatusCancelled:
			return BlockCancelled
		default:
			return BlockSettled
		}
	default:
		return BlockSettled
	}
}

func (m *Model) persistedProviderReference() *persistedProviderRef {
	if m == nil {
		return nil
	}
	locality := persistedProviderLocal
	if m.modelManager != nil && m.modelManager.RemoteProvider() {
		locality = persistedProviderRemote
	}
	return &persistedProviderRef{
		Profile:  m.activeProviderName(),
		Model:    m.model,
		Locality: locality,
	}
}

func validatePersistedProviderReference(state persistedSessionState) error {
	ref := state.Provider
	if ref == nil {
		return nil
	}
	if ref.Profile == "" || len(ref.Profile) > config.MaxProviderProfileNameBytes ||
		ref.Profile != sanitizeTerminalSingleLine(ref.Profile) {
		return fmt.Errorf("saved provider profile is invalid")
	}
	if len(ref.Model) > config.MaxProviderModelNameBytes ||
		ref.Model != sanitizeTerminalSingleLine(ref.Model) {
		return fmt.Errorf("saved provider model is invalid")
	}
	if ref.Locality != persistedProviderLocal && ref.Locality != persistedProviderRemote {
		return fmt.Errorf("saved provider locality %q is invalid", ref.Locality)
	}
	if config.CanonicalModelName(ref.Model) != config.CanonicalModelName(state.Model) {
		return fmt.Errorf("saved provider model does not match session model")
	}
	return nil
}

func (m *Model) validateRestoredProviderReference(state persistedSessionState) error {
	if err := validatePersistedProviderReference(state); err != nil {
		return err
	}
	actualProfile := m.activeProviderName()
	actualLocality := persistedProviderLocal
	if m.modelManager != nil && m.modelManager.RemoteProvider() {
		actualLocality = persistedProviderRemote
	}
	ref := state.Provider
	if ref == nil {
		if actualLocality == persistedProviderRemote {
			return fmt.Errorf(
				"restore provider: legacy session has no provider identity; switch to a local provider before restoring it",
			)
		}
		return nil
	}
	if actualProfile != ref.Profile || actualLocality != ref.Locality {
		return fmt.Errorf(
			"restore provider: session requires %s provider %q; current provider is %s %q",
			ref.Locality,
			ref.Profile,
			actualLocality,
			actualProfile,
		)
	}
	// Remote profiles bind their configured model to the adapter. Refuse a
	// silent same-name fallback with a different model; local Ollama models
	// continue through the existing admission and SetCurrentModel transaction.
	if ref.Locality == persistedProviderRemote {
		if m.modelManager == nil ||
			config.CanonicalModelName(m.modelManager.Model()) != config.CanonicalModelName(ref.Model) {
			return fmt.Errorf("restore provider: current provider model does not match saved model %q", ref.Model)
		}
	}
	return nil
}

func migratePersistedSessionState(state persistedSessionState) (persistedSessionState, error) {
	state.Entries = append([]persistedChatEntry(nil), state.Entries...)
	switch state.Version {
	case 1:
		if state.Mode < ModeNormal || state.Mode > ModeAuto {
			return state, fmt.Errorf("invalid legacy saved mode %d", state.Mode)
		}
		// Legacy BUILD was an interactive authority level, not an autonomous
		// loop. Only a session carrying a real durable goal migrates to AUTO.
		if state.Goal != nil {
			state.Mode = ModeAuto
		} else if state.Mode == ModeBuild {
			state.Mode = ModeNormal
		}
		hydrateLegacyTranscriptIdentity(&state)
		state.Version = currentPersistedSessionVersion
	case 2:
		if state.Mode < ModeNormal || state.Mode > ModeAuto {
			return state, fmt.Errorf("invalid saved mode %d", state.Mode)
		}
		if state.Goal != nil {
			state.Mode = ModeAuto
		}
		hydrateLegacyTranscriptIdentity(&state)
		state.Version = currentPersistedSessionVersion
	case currentPersistedSessionVersion:
		if state.Mode < ModeNormal || state.Mode > ModeAuto {
			return state, fmt.Errorf("invalid saved mode %d", state.Mode)
		}
		if state.Goal != nil {
			state.Mode = ModeAuto
		}
	default:
		return state, fmt.Errorf("unsupported session state version %d", state.Version)
	}
	state.Entries = withoutPersistedThinkingContent(state.Entries)
	return state, nil
}

func canonicalWorkspaceID(workDir string) (string, error) {
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve workspace: %w", err)
		}
	}
	absolute, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func listPersistedSessions(ctx context.Context, store *db.Store, workspaceID string, limit int64) ([]SessionListItem, error) {
	if store == nil {
		return nil, fmt.Errorf("session persistence is unavailable")
	}
	if workspaceID == "" {
		return nil, fmt.Errorf("workspace identity is unavailable")
	}
	sessions, err := store.ListSessions(ctx, db.ListSessionsParams{WorkspaceID: workspaceID, Limit: limit})
	if err != nil {
		return nil, err
	}
	items := make([]SessionListItem, len(sessions))
	for i, session := range sessions {
		items[i] = SessionListItem{
			ID: session.ID, PublicID: session.PublicID,
			Title: session.Title, CreatedAt: session.UpdatedAt,
		}
	}
	return items, nil
}

func loadPersistedSession(ctx context.Context, store *db.Store, id int64, workspaceID string) (db.Session, persistedSessionState, db.SessionStateRecord, error) {
	if store == nil {
		return db.Session{}, persistedSessionState{}, db.SessionStateRecord{}, fmt.Errorf("session persistence is unavailable")
	}
	session, err := store.GetSession(ctx, id)
	if err != nil {
		return db.Session{}, persistedSessionState{}, db.SessionStateRecord{}, err
	}
	if workspaceID == "" || session.WorkspaceID != workspaceID {
		return db.Session{}, persistedSessionState{}, db.SessionStateRecord{}, fmt.Errorf("session %d belongs to a different workspace", id)
	}
	record, err := store.GetSessionStateRecord(ctx, id)
	if err != nil {
		return db.Session{}, persistedSessionState{}, db.SessionStateRecord{}, err
	}
	state, err := decodeSessionState(record.StateJSON)
	if err == nil && state.Goal != nil && state.Goal.SessionID != id {
		return db.Session{}, persistedSessionState{}, db.SessionStateRecord{}, fmt.Errorf("session %d contains goal state for session %d", id, state.Goal.SessionID)
	}
	return session, state, record, err
}

func (m *Model) restoreSessionState(state persistedSessionState) error {
	var err error
	state, err = migratePersistedSessionState(state)
	if err != nil {
		return err
	}
	// Restore is also callable with an in-memory snapshot, so enforce the same
	// privacy boundary as JSON decoding before messages or cards enter the UI.
	state.Messages = agent.SanitizeMessagesForPersistence(state.Messages)
	state.ToolEntries = sanitizePersistedToolEntryArgs(state.ToolEntries)
	state.Entries = withoutPersistedThinkingContent(state.Entries)
	if len(state.Entries) > 0 && !persistedTranscriptHasIdentity(state.Entries) {
		// Direct in-memory callers have not crossed the JSON decoder, so they
		// may still carry the all-zero DTO shape used by internal recovery
		// projections. Complete that shape deterministically. Decoded v3 JSON
		// remains strict in decodeSessionState and never reaches this path.
		hydrateLegacyTranscriptIdentity(&state)
	}
	if err := validatePersistedTranscriptIdentity(state.Entries); err != nil {
		return err
	}
	if err := validatePersistedToolTranscriptState(state); err != nil {
		return err
	}
	if err := m.validateRestoredProviderReference(state); err != nil {
		return err
	}
	if err := validatePersistedContextPromptFloor(state); err != nil {
		return err
	}
	if err := validatePersistedImageAttachments(state.Entries); err != nil {
		return err
	}
	if err := validatePersistedImageProjection(state.Messages, state.Entries); err != nil {
		return err
	}
	var targetGoal *goal.Runtime
	if state.Goal != nil {
		targetGoal, err = goal.Restore(*state.Goal)
		if err != nil {
			return fmt.Errorf("restore goal runtime: %w", err)
		}
	}
	targetDecisionAttempt := cloneCortexDecisionAttempt(state.CortexDecisionAttempt)
	if err := validateRestoredCortexDecisionAttempt(targetDecisionAttempt, state.Goal); err != nil {
		return fmt.Errorf("restore Cortex decision answer fence: %w", err)
	}

	targetManualSkills := uniqueSkillNames(state.ManualSkills)
	if err := m.validateSkillNames(uniqueSkillNames(m.manualSkills, m.profileSkills), "current session"); err != nil {
		return fmt.Errorf("validate current skills: %w", err)
	}
	if err := m.validateSkillNames(targetManualSkills, "saved manual"); err != nil {
		return fmt.Errorf("restore manual skills: %w", err)
	}

	var targetProfile *config.AgentProfile
	var targetProfileSkills []string
	if state.AgentProfile != "" {
		targetProfile, err = m.validateAgentProfile(state.AgentProfile)
		if err != nil {
			return fmt.Errorf("restore agent profile: %w", err)
		}
		targetProfileSkills = uniqueSkillNames(targetProfile.Skills)
	}
	if err := m.validateSkillNames(uniqueSkillNames(targetManualSkills, targetProfileSkills), "saved session"); err != nil {
		return fmt.Errorf("restore skills: %w", err)
	}

	targetModel := state.Model
	if targetModel == "" && targetProfile != nil {
		targetModel = targetProfile.Model
	}
	oldModel := m.model
	modelSwitched := false
	if targetModel != "" && targetModel != m.model {
		if err := m.validateModelAdmission(targetModel); err != nil {
			return fmt.Errorf("restore model: %w", err)
		}
		if m.modelManager != nil {
			m.prepareModelSwitch()
			if err := m.modelManager.SetCurrentModel(targetModel); err != nil {
				return fmt.Errorf("restore model: %w", err)
			}
		}
		modelSwitched = true
	}

	// Commit all prompt-affecting skill ownership only after the target model
	// has been admitted. Scope remains unchanged until every fallible operation
	// has succeeded.
	if err := m.setSkillContributions(targetManualSkills, targetProfileSkills); err != nil {
		if modelSwitched && m.modelManager != nil && oldModel != "" {
			_ = m.modelManager.SetCurrentModel(oldModel)
		}
		return fmt.Errorf("restore session skills: %w", err)
	}
	m.clearViewerModals(false)
	m.outputDetails = NewOutputDetailStore()
	m.loadedFile = state.LoadedFile
	m.manualLoadedContext = state.ManualLoadedContext
	m.agentProfile = state.AgentProfile
	m.syncLoadedContext()
	// Authority scope is the final runtime commit and is never touched on a
	// validation/model/skill failure above.
	if m.agent != nil {
		if targetProfile == nil {
			m.agent.SetMCPServerScope(nil)
		} else {
			m.agent.SetMCPServerScope(targetProfile.MCPServers)
		}
	}
	if targetModel != "" {
		m.setCurrentModelProjection(targetModel)
		for index := range m.ollamaModels {
			m.ollamaModels[index].Current = config.CanonicalModelName(m.ollamaModels[index].Name) == config.CanonicalModelName(targetModel)
		}
	}

	m.mode = state.Mode
	m.modelPinned = state.ModelPinned
	m.clearPendingImages()
	m.turnImages = nil
	m.agent.ReplaceMessages(append([]llm.Message(nil), state.Messages...))
	if err := m.agent.RestoreContextPromptFloor(state.ContextPromptFloor); err != nil {
		return fmt.Errorf("restore context prompt floor: %w", err)
	}
	m.resetEntryMemo()
	m.entries = make([]ChatEntry, len(state.Entries))
	for i, entry := range state.Entries {
		m.entries[i] = ChatEntry{
			Kind:              entry.Kind,
			Content:           entry.Content,
			Name:              entry.Name,
			IsError:           entry.IsError,
			ToolIndex:         entry.ToolIndex,
			ThinkingCollapsed: entry.ThinkingCollapsed,
			Attachments:       append([]imageasset.Ref(nil), entry.Attachments...),
			BlockID:           entry.BlockID,
			TurnID:            entry.TurnID,
			Revision:          entry.Revision,
			Lifecycle:         entry.Lifecycle,
		}
	}
	m.toolEntries = restoreToolEntries(state.ToolEntries)
	m.toolsPending = 0
	m.sessionEvalTotal = state.SessionEvalTotal
	m.sessionPromptTotal = state.SessionPromptTotal
	m.sessionTurnCount = state.SessionTurnCount
	m.executionCursor = state.ExecutionCursor
	m.fileChanges = make(map[string]int, len(state.FileChanges))
	for path, count := range state.FileChanges {
		m.fileChanges[path] = count
	}
	m.goalRuntime = targetGoal
	m.goalPersistenceDirty = false
	// Pending decision prose is never session state. A restore discards every
	// transient presentation/generation and recovers it only through fresh,
	// read-only Cortex status after the durable Goal snapshot is installed.
	m.cortexDecision = nil
	m.cortexDecisionOp = nil
	m.cortexDecisionAttempt = targetDecisionAttempt
	m.cortexDecisionGen++
	if m.overlay == OverlayCortexDecision {
		m.overlay = OverlayNone
		m.overlayParent = OverlayNone
	}
	m.syncComposerAuthority()
	m.setRouterMode(m.modeConfigs[m.presentedMode()].RouterMode)

	// Workspace context is reconstructed from a fresh exact Bob receipt. It is
	// never inherited from a durable transcript or a previously active session.
	m.clearBobWorkspaceContext()
	m.resetTurnDiagnostics()
	return nil
}

// serializeEntries converts chat entries to markdown for storage.
func serializeEntries(entries []ChatEntry) string {
	var b strings.Builder
	for _, e := range entries {
		switch e.Kind {
		case "user":
			b.WriteString("## User\n\n")
			b.WriteString(e.Content)
			for _, attachment := range e.Attachments {
				name := sanitizeTerminalSingleLine(attachment.Name)
				fmt.Fprintf(&b, "\n\n[image: %s · %dx%d · %s]", name, attachment.Width, attachment.Height, attachment.Handle())
			}
			b.WriteString("\n\n")
		case "assistant":
			b.WriteString("## Assistant\n\n")
			b.WriteString(e.Content)
			b.WriteString("\n\n")
		case "system":
			b.WriteString("## System\n\n")
			b.WriteString(e.Content)
			b.WriteString("\n\n")
		case "error":
			b.WriteString("## Error\n\n")
			b.WriteString(e.Content)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// deserializeEntries parses markdown back into chat entries.
func deserializeEntries(content string) []ChatEntry {
	if content == "" {
		return nil
	}

	var entries []ChatEntry
	sections := strings.Split(content, "## ")

	for _, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}

		nlIdx := strings.Index(section, "\n")
		if nlIdx == -1 {
			continue
		}

		header := strings.TrimSpace(section[:nlIdx])
		body := strings.TrimSpace(section[nlIdx+1:])

		var kind string
		switch header {
		case "User":
			kind = "user"
		case "Assistant":
			kind = "assistant"
		case "System":
			kind = "system"
		case "Error":
			kind = "error"
		default:
			continue
		}

		entries = append(entries, ChatEntry{
			Kind:    kind,
			Content: body,
		})
	}

	return entries
}
