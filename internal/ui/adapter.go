package ui

import (
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/rivo/uniseg"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

const (
	maxAdapterErrorNoticeBytes       = 4 * 1024
	maxAdapterErrorNoticeGraphemes   = 512
	maxAdapterSystemNoticeBytes      = 16 * 1024
	maxAdapterSystemNoticeGraphemes  = 2 * 1024
	adapterNoticeInputExpansionLimit = 4
	adapterNoticeTruncatedMarker     = "\n...[notice truncated]"
)

// Adapter bridges the agent.Output interface to BubbleTea messages.
type Adapter struct {
	program       *tea.Program
	workDir       string
	outputDetails *OutputDetailStore
}

var _ agent.BobWorkspaceContextOutput = (*Adapter)(nil)
var _ agent.SemanticToolOutput = (*Adapter)(nil)
var _ agent.SemanticToolDetailOutput = (*Adapter)(nil)

// NewAdapter creates an Adapter that sends messages to the given program.
func NewAdapter(p *tea.Program, workDir ...string) *Adapter {
	return NewAdapterWithOutputDetails(p, nil, workDir...)
}

// NewAdapterWithOutputDetails creates an Adapter that can retain a bounded,
// terminal-safe, post-redaction prefix of ordinary unstructured tool output
// for the process-local viewer. Parser-private semantic payloads deliberately
// never cross this boundary.
func NewAdapterWithOutputDetails(p *tea.Program, store *OutputDetailStore, workDir ...string) *Adapter {
	dir := ""
	if len(workDir) > 0 {
		dir = workDir[0]
	}
	return &Adapter{program: p, workDir: dir, outputDetails: store}
}

func (a *Adapter) StreamText(text string) {
	sendMsg(a.program, StreamTextMsg{Text: text})
}

func (a *Adapter) StreamReasoning(text string) {
	sendMsg(a.program, StreamThinkingMsg{Text: text})
}

func (a *Adapter) StreamDone(evalCount, promptTokens int) {
	sendMsg(a.program, StreamDoneMsg{EvalCount: evalCount, PromptTokens: promptTokens})
}

func (a *Adapter) ToolCallStart(callID, name string, args map[string]any) {
	sendMsg(a.program, newToolCallStartMsg(callID, name, args, a.workDir))
}

// newToolCallStartMsg captures an optional pre-write snapshot on the agent's
// background execution path, before ToolCallStart returns and the backend can
// mutate the file. Bubble Tea Update receives immutable snapshot data and
// never performs filesystem I/O.
func newToolCallStartMsg(callID, name string, args map[string]any, workDir string) ToolCallStartMsg {
	msg := ToolCallStartMsg{ID: callID, Name: name, Args: args, StartTime: time.Now()}
	if classifyTool(name) == ToolTypeFileWrite {
		snapshot := readDiffSnapshotForArgsAt(args, workDir)
		msg.BeforeContent = snapshot.Content
		msg.BeforeSnapshotAvailable = snapshot.Available
	}
	return msg
}

func (a *Adapter) ToolCallResult(callID, name string, result string, isError bool, duration time.Duration) {
	sendMsg(a.program, a.toolCallResultMsg(
		callID,
		name,
		result,
		isError,
		duration,
		ecosystem.ToolProjection{},
		"",
		false,
	))
}

// ToolCallSemanticResult carries only the bounded host projection into the UI;
// raw StructuredContent remains inside the agent parser boundary.
func (a *Adapter) ToolCallSemanticResult(callID, name string, result string, isError bool, duration time.Duration, projection ecosystem.ToolProjection) {
	sendMsg(a.program, a.toolCallResultMsg(
		callID,
		name,
		result,
		isError,
		duration,
		projection,
		"",
		false,
	))
}

// ToolCallSemanticResultWithDetail admits only an explicit complete,
// post-redaction unstructured payload. The payload is consumed synchronously by
// the bounded process-local store; Bubble Tea receives only an opaque
// capability and scalar digest.
func (a *Adapter) ToolCallSemanticResultWithDetail(
	callID, name string,
	result string,
	isError bool,
	duration time.Duration,
	projection ecosystem.ToolProjection,
	detail *agent.ToolOutputDetail,
) {
	detailText := ""
	admitDetail := detail != nil && detail.Complete
	if admitDetail {
		detailText = detail.Text
	}
	sendMsg(a.program, a.toolCallResultMsg(
		callID,
		name,
		result,
		isError,
		duration,
		projection,
		detailText,
		admitDetail,
	))
}

func (a *Adapter) toolCallResultMsg(
	callID string,
	name string,
	result string,
	isError bool,
	duration time.Duration,
	projection ecosystem.ToolProjection,
	outputDetail string,
	admitDetail bool,
) ToolCallResultMsg {
	msg := ToolCallResultMsg{
		ID: callID, Name: name, Result: result, IsError: isError,
		Duration: duration, Projection: projection,
	}
	if a != nil && a.outputDetails != nil && admitDetail {
		if receipt, err := a.outputDetails.Admit(outputDetail); err == nil {
			msg.OutputDetail = receipt
		}
	}
	return msg
}

func (a *Adapter) SystemMessage(msg string) {
	sendMsg(a.program, SystemMessageMsg{Msg: boundedAdapterNotice(
		msg,
		maxAdapterSystemNoticeBytes,
		maxAdapterSystemNoticeGraphemes,
	)})
}

func (a *Adapter) CapabilityRoute(route agent.CapabilityRoute) {
	sendMsg(a.program, CapabilityRouteMsg{Route: route})
}

// ContinuationSuggestion forwards only the already bounded presentation. Tool
// arguments, workspace references, command strings, and raw receipt content
// never cross into Bubble Tea state.
func (a *Adapter) ContinuationSuggestion(turnID string, sequence uint64, suggestion *agent.ContinuationSuggestion) {
	var presentation *ContinuationActionPresentation
	if suggestion != nil {
		presentation = &ContinuationActionPresentation{
			Tool: suggestion.Tool, Inputs: append([]string(nil), suggestion.Inputs...),
			BlockedBy: append([]string(nil), suggestion.BlockedBy...), ReasonCode: suggestion.ReasonCode,
		}
	}
	sendMsg(a.program, ContinuationActionMsg{TurnID: turnID, Sequence: sequence, Action: presentation})
}

// BobWorkspaceContext forwards only the bounded semantic workspace digest.
// The adapter detaches the value so an agent-side cache update cannot mutate a
// message that Bubble Tea has not processed yet.
func (a *Adapter) BobWorkspaceContext(state agent.BobWorkspaceContextState) {
	var digest *ecosystem.ReceiptDigest
	if state.Digest != nil {
		copy := *state.Digest
		copy.Items = append([]string(nil), state.Digest.Items...)
		copy.Required = append([]string(nil), state.Digest.Required...)
		digest = &copy
	}
	sendMsg(a.program, BobWorkspaceContextMsg{Generation: state.Generation, Digest: digest})
}

func (a *Adapter) ContextCompacted() {
	sendMsg(a.program, ContextCompactedMsg{})
}

func (a *Adapter) ContextCompactionStarted() {
	sendMsg(a.program, ContextCompactionStartedMsg{})
}

func (a *Adapter) ContextCompactionFinished() {
	sendMsg(a.program, ContextCompactionFinishedMsg{})
}

func (a *Adapter) Error(msg string) {
	sendMsg(a.program, ErrorMsg{Msg: boundedAdapterNotice(
		msg,
		maxAdapterErrorNoticeBytes,
		maxAdapterErrorNoticeGraphemes,
	)})
}

// boundedAdapterNotice is the final defensive boundary for agent-originated
// transcript notices. It bounds the amount of untrusted input scanned, removes
// terminal/visual-order controls, and caps the resulting UTF-8 by both bytes
// and grapheme clusters so combining/ZWJ sequences are never split.
func boundedAdapterNotice(value string, byteLimit, graphemeLimit int) string {
	if byteLimit <= 0 || graphemeLimit <= 0 {
		return ""
	}
	scanLimit := byteLimit * adapterNoticeInputExpansionLimit
	if scanLimit < byteLimit {
		scanLimit = byteLimit
	}
	prefix, inputTruncated := adapterNoticeUTF8Prefix(value, scanLimit)
	safe := sanitizeTerminalMultiline(prefix)
	safeGraphemes := uniseg.GraphemeClusterCount(safe)
	if !inputTruncated && len(safe) <= byteLimit && safeGraphemes <= graphemeLimit {
		// Detach even a small substring from caller-owned backing storage before
		// Bubble Tea can retain it in transcript/session state.
		return strings.Clone(safe)
	}

	marker := adapterNoticeTruncatedMarker
	markerGraphemes := uniseg.GraphemeClusterCount(marker)
	if len(marker) > byteLimit || markerGraphemes > graphemeLimit {
		return ""
	}
	contentByteLimit := byteLimit - len(marker)
	contentGraphemeLimit := graphemeLimit - markerGraphemes
	var bounded strings.Builder
	bounded.Grow(min(len(safe), contentByteLimit) + len(marker))
	graphemes := uniseg.NewGraphemes(safe)
	usedBytes := 0
	usedGraphemes := 0
	for graphemes.Next() {
		cluster := graphemes.Str()
		if usedGraphemes >= contentGraphemeLimit ||
			usedBytes+len(cluster) > contentByteLimit {
			break
		}
		bounded.WriteString(cluster)
		usedBytes += len(cluster)
		usedGraphemes++
	}
	bounded.WriteString(marker)
	return bounded.String()
}

func adapterNoticeUTF8Prefix(value string, limit int) (string, bool) {
	if limit <= 0 {
		return "", value != ""
	}
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}
