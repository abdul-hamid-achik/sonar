package ui

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	maxExpertProgressItems      = 16
	maxExpertProgressNameBytes  = 128
	maxExpertProgressModelBytes = 256
	maxExpertProgressTokens     = 10_000_000
	maxExpertProgressDetailRows = 6
)

// ExpertProgressItem is the bounded, host-owned UI projection for one expert.
// It deliberately has no prompt, objective, report, provider error, reasoning,
// path, or arbitrary metadata field.
type ExpertProgressItem struct {
	Index       int                      `json:"index"`
	Expert      string                   `json:"expert"`
	Model       string                   `json:"model"`
	Location    llm.OllamaModelLocation  `json:"location"`
	Phase       expertteam.ProgressPhase `json:"phase"`
	Status      expertteam.ExpertStatus  `json:"status,omitempty"`
	FailureCode string                   `json:"failure_code,omitempty"`
	EvalTokens  int                      `json:"eval_tokens,omitempty"`
}

// ExpertProgressState is the complete bounded consultation projection. The
// slice is fixed to Total (at most sixteen) and replaces an unbounded map.
type ExpertProgressState struct {
	Sequence    uint64                  `json:"sequence"`
	Strategy    expertselector.Strategy `json:"strategy"`
	Total       int                     `json:"total"`
	Parallelism int                     `json:"parallelism"`
	Running     int                     `json:"running"`
	Queued      int                     `json:"queued"`
	Completed   int                     `json:"completed"`
	Failed      int                     `json:"failed"`
	Experts     []ExpertProgressItem    `json:"experts"`
}

func isExpertConsultTool(name string) bool {
	// The progress contract belongs only to sonar's exact built-in. A
	// similarly named routed tool must not inherit its presentation authority.
	return name == "consult_experts"
}

func normalizeExpertProgressEvent(event expertteam.ProgressEvent) (expertteam.ProgressEvent, bool) {
	if event.Sequence == 0 || event.Total < 1 || event.Total > maxExpertProgressItems ||
		event.Sequence > uint64(1+2*event.Total) ||
		event.Parallelism < 1 || event.Parallelism > event.Total ||
		!validExpertProgressCount(event.Running, event.Total) ||
		!validExpertProgressCount(event.Queued, event.Total) ||
		!validExpertProgressCount(event.Completed, event.Total) ||
		!validExpertProgressCount(event.Failed, event.Total) ||
		event.Running+event.Queued+event.Completed+event.Failed != event.Total ||
		event.EvalTokens < 0 || event.EvalTokens > maxExpertProgressTokens {
		return expertteam.ProgressEvent{}, false
	}
	if event.Strategy != expertselector.StrategyTeam && event.Strategy != expertselector.StrategySwarm && event.Strategy != expertselector.StrategyMoE {
		return expertteam.ProgressEvent{}, false
	}

	switch event.Phase {
	case expertteam.ProgressPlanned:
		if event.ExpertIndex != -1 || event.Expert != "" || event.Model != "" || event.Status != "" ||
			event.ErrorCode != "" || event.EvalTokens != 0 || event.Running != 0 ||
			event.Queued != event.Total || event.Completed != 0 || event.Failed != 0 {
			return expertteam.ProgressEvent{}, false
		}
		return event, true
	case expertteam.ProgressStarted, expertteam.ProgressCompleted, expertteam.ProgressFailed:
		if event.ExpertIndex < 0 || event.ExpertIndex >= event.Total ||
			!boundedExpertProgressIdentifier(event.Expert, maxExpertProgressNameBytes) ||
			!boundedExpertProgressIdentifier(event.Model, maxExpertProgressModelBytes) ||
			!validExpertProgressLocation(event.Location) {
			return expertteam.ProgressEvent{}, false
		}
		event.Expert = sanitizeTerminalSingleLine(event.Expert)
		event.Model = sanitizeTerminalSingleLine(event.Model)
		if !boundedExpertProgressIdentifier(event.Expert, maxExpertProgressNameBytes) ||
			!boundedExpertProgressIdentifier(event.Model, maxExpertProgressModelBytes) {
			return expertteam.ProgressEvent{}, false
		}
	default:
		return expertteam.ProgressEvent{}, false
	}

	switch event.Phase {
	case expertteam.ProgressStarted:
		if event.Status != "" || event.ErrorCode != "" || event.EvalTokens != 0 {
			return expertteam.ProgressEvent{}, false
		}
	case expertteam.ProgressCompleted:
		if event.Status != expertteam.ExpertCompleted || event.ErrorCode != "" {
			return expertteam.ProgressEvent{}, false
		}
	case expertteam.ProgressFailed:
		if event.Status != expertteam.ExpertFailed || !validExpertFailureCode(event.ErrorCode) {
			return expertteam.ProgressEvent{}, false
		}
	}
	return event, true
}

func validExpertProgressCount(value, total int) bool { return value >= 0 && value <= total }

func boundedExpertProgressIdentifier(value string, limit int) bool {
	if !utf8.ValidString(value) || value == "" || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validExpertProgressLocation(location llm.OllamaModelLocation) bool {
	switch location {
	case llm.OllamaModelLocationUnknown, llm.OllamaModelLocationLocal,
		llm.OllamaModelLocationCloud, llm.OllamaModelLocationRemote:
		return true
	default:
		return false
	}
}

func validExpertFailureCode(code string) bool {
	switch code {
	case "cancelled", "timed_out", "model_unavailable", "budget_exceeded", "inference_failed",
		"missing_usage_receipt", "no_visible_report":
		return true
	default:
		return false
	}
}

func cloneExpertProgressState(state *ExpertProgressState) *ExpertProgressState {
	if state == nil {
		return nil
	}
	copy := *state
	copy.Experts = append([]ExpertProgressItem(nil), state.Experts...)
	return &copy
}

func (state *ExpertProgressState) apply(event expertteam.ProgressEvent) bool {
	if state == nil || event.Sequence != state.Sequence+1 || event.Total != state.Total ||
		event.Strategy != state.Strategy || event.Parallelism != state.Parallelism ||
		event.Completed < state.Completed || event.Failed < state.Failed {
		return false
	}
	item := state.Experts[event.ExpertIndex]
	if item.Expert != "" && (item.Expert != event.Expert || item.Model != event.Model || item.Location != event.Location) {
		return false
	}

	switch event.Phase {
	case expertteam.ProgressStarted:
		if item.Phase != "" || event.Running != state.Running+1 || event.Queued != state.Queued-1 ||
			event.Completed != state.Completed || event.Failed != state.Failed {
			return false
		}
	case expertteam.ProgressCompleted:
		if item.Phase != expertteam.ProgressStarted || event.Running != state.Running-1 || event.Queued != state.Queued ||
			event.Completed != state.Completed+1 || event.Failed != state.Failed {
			return false
		}
	case expertteam.ProgressFailed:
		switch item.Phase {
		case expertteam.ProgressStarted:
			if event.Running != state.Running-1 || event.Queued != state.Queued {
				return false
			}
		case "":
			if event.Running != state.Running || event.Queued != state.Queued-1 {
				return false
			}
		default:
			return false
		}
		if event.Completed != state.Completed || event.Failed != state.Failed+1 {
			return false
		}
	default:
		return false
	}

	state.Sequence = event.Sequence
	state.Running = event.Running
	state.Queued = event.Queued
	state.Completed = event.Completed
	state.Failed = event.Failed
	state.Experts[event.ExpertIndex] = ExpertProgressItem{
		Index: event.ExpertIndex, Expert: event.Expert, Model: event.Model, Location: event.Location,
		Phase: event.Phase, Status: event.Status, FailureCode: event.ErrorCode, EvalTokens: event.EvalTokens,
	}
	return true
}

func newExpertProgressState(event expertteam.ProgressEvent) *ExpertProgressState {
	if event.Phase != expertteam.ProgressPlanned || event.Sequence != 1 {
		return nil
	}
	return &ExpertProgressState{
		Sequence: event.Sequence, Strategy: event.Strategy, Total: event.Total, Parallelism: event.Parallelism,
		Running: event.Running, Queued: event.Queued, Completed: event.Completed, Failed: event.Failed,
		Experts: make([]ExpertProgressItem, event.Total),
	}
}

func sanitizeExpertProgressState(state *ExpertProgressState, requireSettled bool) *ExpertProgressState {
	state = cloneExpertProgressState(state)
	if state == nil || state.Sequence == 0 || state.Total < 1 || state.Total > maxExpertProgressItems ||
		state.Sequence > uint64(1+2*state.Total) ||
		state.Parallelism < 1 || state.Parallelism > state.Total || len(state.Experts) != state.Total ||
		!validExpertProgressCount(state.Running, state.Total) || !validExpertProgressCount(state.Queued, state.Total) ||
		!validExpertProgressCount(state.Completed, state.Total) || !validExpertProgressCount(state.Failed, state.Total) ||
		state.Running+state.Queued+state.Completed+state.Failed != state.Total ||
		(state.Strategy != expertselector.StrategyTeam && state.Strategy != expertselector.StrategySwarm && state.Strategy != expertselector.StrategyMoE) ||
		(requireSettled && (state.Running != 0 || state.Queued != 0)) {
		return nil
	}
	counts := struct{ running, queued, completed, failed int }{}
	for index := range state.Experts {
		item := &state.Experts[index]
		if item.Expert == "" && item.Model == "" && item.Phase == "" {
			if item.Index != 0 || item.Location != llm.OllamaModelLocationUnknown || item.Status != "" ||
				item.FailureCode != "" || item.EvalTokens != 0 {
				return nil
			}
			counts.queued++
			continue
		}
		if item.Index != index || !boundedExpertProgressIdentifier(item.Expert, maxExpertProgressNameBytes) ||
			!boundedExpertProgressIdentifier(item.Model, maxExpertProgressModelBytes) || !validExpertProgressLocation(item.Location) ||
			item.EvalTokens < 0 || item.EvalTokens > maxExpertProgressTokens {
			return nil
		}
		item.Expert = sanitizeTerminalSingleLine(item.Expert)
		item.Model = sanitizeTerminalSingleLine(item.Model)
		if !boundedExpertProgressIdentifier(item.Expert, maxExpertProgressNameBytes) ||
			!boundedExpertProgressIdentifier(item.Model, maxExpertProgressModelBytes) {
			return nil
		}
		switch item.Phase {
		case expertteam.ProgressStarted:
			if item.Status != "" || item.FailureCode != "" || item.EvalTokens != 0 {
				return nil
			}
			counts.running++
		case expertteam.ProgressCompleted:
			if item.Status != expertteam.ExpertCompleted || item.FailureCode != "" {
				return nil
			}
			counts.completed++
		case expertteam.ProgressFailed:
			if item.Status != expertteam.ExpertFailed || !validExpertFailureCode(item.FailureCode) {
				return nil
			}
			counts.failed++
		default:
			return nil
		}
	}
	if counts.running != state.Running || counts.queued != state.Queued || counts.completed != state.Completed || counts.failed != state.Failed {
		return nil
	}
	return state
}

func (state *ExpertProgressState) summary() string {
	if state == nil || state.Total == 0 {
		return ""
	}
	if state.Completed+state.Failed == state.Total {
		parts := []string{fmt.Sprintf("%d completed", state.Completed)}
		if state.Failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", state.Failed))
		}
		return strings.Join(parts, " · ")
	}
	parts := []string{fmt.Sprintf("%d/%d finished", state.Completed+state.Failed, state.Total)}
	if state.Running > 0 {
		parts = append(parts, fmt.Sprintf("%d active", state.Running))
	}
	if state.Queued > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", state.Queued))
	}
	return strings.Join(parts, " · ")
}

// projectionWithExpertProgressOutcome folds the exact bounded host-owned
// consultation result into the durable semantic projection. This removes the
// old need for ToolCard to carry an independent state override.
func projectionWithExpertProgressOutcome(
	projection ecosystem.ToolProjection,
	state *ExpertProgressState,
) ecosystem.ToolProjection {
	projection = projection.Normalize()
	if state == nil || state.Running > 0 || state.Queued > 0 {
		return projection
	}
	switch {
	case state.Completed == 0 && state.Failed > 0:
		projection.Domain = ecosystem.DomainFailed
		projection.DomainTyped = true
		projection.Evidence = ecosystem.EvidenceNone
	case state.Failed > 0:
		projection.Domain = ecosystem.DomainAttention
		projection.DomainTyped = true
		projection.Evidence = ecosystem.EvidenceNone
	}
	return projection.Normalize()
}

func (state *ExpertProgressState) renderDetails(width int, styles ToolCardStyles, profiles ...GlyphProfile) string {
	if state == nil {
		return ""
	}
	nodes, ok := workNodesFromExpertProgress(state)
	if !ok {
		return ""
	}
	nodes = presentedWorkNodes(nodes)
	width = max(1, width)
	profile := resolveGlyphProfile(profiles...)
	separator := glyphSeparator(profile)
	truncate := func(value string) string {
		return truncateDisplayWithGlyphProfile(value, width, profile)
	}
	lines := make([]string, 0, maxExpertProgressDetailRows+1)
	known, queued := 0, 0
	for _, node := range nodes {
		if node.Status == WorkNodeQueued {
			queued++
			continue
		}
		known++
	}
	queueRendered := false
	hidden := 0
	for _, node := range nodes {
		if node.Status == WorkNodeQueued {
			if queueRendered {
				continue
			}
			queueRendered = true
			label := fmt.Sprintf("%d more queued", queued)
			if known == 0 {
				label = fmt.Sprintf("%d experts queued%s%d at a time", queued, separator, state.Parallelism)
			}
			lines = append(lines, styles.Dimmed.Render(truncate(label)))
			continue
		}
		nodeLines := renderExpertProgressNode(node, width, styles, profile)
		reservedRows := 0
		if queued > 0 && !queueRendered {
			// The aggregate represents every untouched queue slot. Reserve its
			// stable position instead of letting earlier completed nodes crowd it
			// out of the bounded inline surface.
			reservedRows = 1
		}
		if len(lines)+len(nodeLines) > maxExpertProgressDetailRows-reservedRows {
			hidden++
			continue
		}
		lines = append(lines, nodeLines...)
	}
	if hidden > 0 {
		label := fmt.Sprintf("+%d more%sCtrl+G Agents", hidden, separator)
		lines = append(lines, styles.Dimmed.Render(truncate(label)))
	}
	return strings.Join(lines, "\n")
}

func renderExpertProgressNode(node WorkNode, width int, styles ToolCardStyles, profiles ...GlyphProfile) []string {
	width = max(1, width)
	profile := resolveGlyphProfile(profiles...)
	glyphs := glyphSet(profile)
	separator := glyphSeparator(profile)
	truncate := func(value string) string {
		return truncateDisplayWithGlyphProfile(value, width, profile)
	}
	glyph, status, style := "…", "running", styles.TitleRunning
	if profile == GlyphASCII {
		glyph = glyphs.Running
	}
	switch node.Status {
	case WorkNodeWaiting:
		glyph, status, style = glyphs.Waiting, "waiting", styles.TitleAttention
	case WorkNodeCompleted:
		glyph, status, style = glyphs.Success, "completed", styles.TitleSuccess
	case WorkNodeAttention:
		glyph, status, style = "!", expertFailureLabel(node.FailureCode), styles.TitleAttention
	case WorkNodeFailed:
		glyph, status, style = glyphs.Error, expertFailureLabel(node.FailureCode), styles.TitleError
	case WorkNodeCancelled:
		glyph, status, style = glyphs.Cancelled, "cancelled", styles.Dimmed
	}
	tokens := ""
	if node.EvalTokens > 0 {
		tokens = fmt.Sprintf("%s%d tok", separator, node.EvalTokens)
	}
	roleLine := glyph + " " + node.Label + separator + status + tokens
	modelLine := node.Model + separator + string(node.Location)
	activityLine := workNodeActivitySummary(node, profile)
	if width >= 54 {
		line := roleLine
		if activityLine != "" {
			line += separator + activityLine
		}
		line += separator + modelLine
		return []string{style.Render(truncate(line))}
	}
	lines := []string{style.Render(truncate(roleLine))}
	if width >= 12 {
		detailLine := activityLine
		if detailLine != "" {
			detailLine += separator
		}
		detailLine += modelLine
		lines = append(lines, styles.Dimmed.Render(truncate("  "+detailLine)))
	}
	return lines
}

func expertFailureLabel(code string) string {
	switch code {
	case "no_visible_report":
		return "no visible report"
	case "model_unavailable":
		return "model unavailable"
	case "budget_exceeded":
		return "budget exceeded"
	case "missing_usage_receipt":
		return "usage missing"
	case "timed_out":
		return "timed out"
	case "cancelled":
		return "cancelled"
	default:
		return "failed"
	}
}

func (card *ToolCard) setExpertProgress(state *ExpertProgressState, width int) {
	if card == nil {
		return
	}
	card.ExpertProgress = state
	card.expertProgressCache = ""
	card.expertProgressCacheWidth = 0
	card.expertProgressCacheSequence = 0
	card.expertProgressCacheProfile = resolveGlyphProfile(card.GlyphProfile)
	if state == nil {
		return
	}
	width = max(1, width)
	card.expertProgressCache = state.renderDetails(width, card.Styles, card.GlyphProfile)
	card.expertProgressCacheWidth = width
	card.expertProgressCacheSequence = state.Sequence
	card.expertProgressCacheProfile = resolveGlyphProfile(card.GlyphProfile)
}

func (card ToolCard) expertProgressDetails(width int) string {
	if card.ExpertProgress == nil {
		return ""
	}
	width = max(1, width)
	if card.expertProgressCacheWidth == width &&
		card.expertProgressCacheSequence == card.ExpertProgress.Sequence &&
		card.expertProgressCacheProfile == resolveGlyphProfile(card.GlyphProfile) {
		return card.expertProgressCache
	}
	return card.ExpertProgress.renderDetails(width, card.Styles, card.GlyphProfile)
}

func (m *Model) handleExpertProgress(msg ExpertProgressMsg) tea.Cmd {
	event, ok := normalizeExpertProgressEvent(msg.Event)
	if !ok || msg.CallID == "" {
		return nil
	}
	for index := len(m.toolEntries) - 1; index >= 0; index-- {
		entry := &m.toolEntries[index]
		if entry.ID != msg.CallID || !isExpertConsultTool(entry.Name) || entry.Status != ToolStatusRunning {
			continue
		}
		var next *ExpertProgressState
		if entry.ExpertProgress == nil {
			next = newExpertProgressState(event)
			if next == nil {
				return nil
			}
		} else {
			next = cloneExpertProgressState(entry.ExpertProgress)
			if !next.apply(event) {
				return nil
			}
		}
		entry.ExpertProgress = next
		entry.Summary = boundedToolCardSummary(next.summary())
		// The live expert surface starts open so keyboard-only users do not need
		// a running-state-only shortcut to discover role/model status.
		entry.Collapsed = false
		m.invalidateEntryCache()
		m.refreshTranscript()
		cmd := m.refreshAgentHub()
		m.gotoBottomIfFollowing()
		return cmd
	}
	return nil
}

func (m *Model) runningExpertActivity() (workingActivity, bool) {
	start := min(max(0, m.turnToolStartIndex), len(m.toolEntries))
	for index := len(m.toolEntries) - 1; index >= start; index-- {
		entry := m.toolEntries[index]
		if entry.Status != ToolStatusRunning || !isExpertConsultTool(entry.Name) {
			continue
		}
		activity := workingActivity{label: "Consulting experts", compactLabel: "Experts", cancellable: true, static: true}
		if entry.ExpertProgress != nil {
			activity.detail = entry.ExpertProgress.summary()
		}
		return activity, true
	}
	return workingActivity{}, false
}
