package ui

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

const (
	maxToolViewOperationBytes = 256
	maxToolViewOperationCells = 96
	maxToolViewTargetBytes    = 128
	maxToolViewDuration       = 30 * 24 * time.Hour
)

// ToolKind is the provider-neutral semantic family of one invocation. It is
// deliberately independent from card styling and transcript geometry.
type ToolKind uint8

const (
	ToolKindUnknown ToolKind = iota
	ToolKindFile
	ToolKindShell
	ToolKindSearch
	ToolKindGit
	ToolKindGeneric
)

func (kind ToolKind) Valid() bool {
	return kind >= ToolKindFile && kind <= ToolKindGeneric
}

// ToolLifecycle describes the user-visible lifecycle without conflating a
// successful transport with a successful domain operation.
type ToolLifecycle uint8

const (
	ToolLifecycleUnknown ToolLifecycle = iota
	ToolLifecyclePending
	ToolLifecycleRunning
	ToolLifecycleSucceeded
	ToolLifecycleAttention
	ToolLifecycleFailed
	ToolLifecycleCancelled
)

func (lifecycle ToolLifecycle) Valid() bool {
	return lifecycle >= ToolLifecyclePending && lifecycle <= ToolLifecycleCancelled
}

func (lifecycle ToolLifecycle) Terminal() bool {
	return lifecycle == ToolLifecycleSucceeded ||
		lifecycle == ToolLifecycleAttention ||
		lifecycle == ToolLifecycleFailed ||
		lifecycle == ToolLifecycleCancelled
}

// ToolViewModel is the narrow, terminal-safe projection admitted to tool UI.
// Raw arguments, results, MCP StructuredContent, provider prose, and layout
// state have no representation here.
type ToolViewModel struct {
	InvocationID string
	BlockID      BlockID
	ToolName     string
	Kind         ToolKind
	Operation    string
	Target       string
	Lifecycle    ToolLifecycle
	Transport    ecosystem.TransportState
	Domain       ecosystem.DomainState
	Evidence     ecosystem.EvidenceState
	Summary      string
	Duration     time.Duration
	Revision     uint64
	Artifact     *ecosystem.ArtifactDigest
	// Projection is already normalized by internal/ecosystem and contains only
	// bounded routing, digest, artifact, and semantic-state facts. It is kept so
	// the dumb ToolCard can render specialist receipts without reaching back
	// into ToolEntry or raw provider output.
	Projection ecosystem.ToolProjection
}

// ToolPreview is the bounded, terminal-safe body admitted to a tool card.
// RawArgs, BeforeContent, provider StructuredContent, and arbitrary metadata
// intentionally have no representation here.
type ToolPreview struct {
	Mode            ToolPreviewMode
	Arguments       string
	Result          string
	ResultLanguage  string
	OutputDigest    OutputDetailDigest
	OutputAvailable bool
	StartedAt       time.Time
	Expanded        bool
	DiffLines       []DiffLine
	DiffPending     bool
	ExpertProgress  *ExpertProgressState
	ansiResultLines [][]ansiRemapSegment
	ansiHiddenLines int
}

// ToolPreviewMode selects a bounded result-body policy without exposing raw
// arguments or asking the visual component to infer semantics from a
// provider-controlled name.
type ToolPreviewMode uint8

const (
	ToolPreviewUnknown ToolPreviewMode = iota
	ToolPreviewRead
	ToolPreviewExec
	ToolPreviewSearch
	ToolPreviewEdit
	ToolPreviewGeneric
)

func (mode ToolPreviewMode) Valid() bool {
	return mode >= ToolPreviewRead && mode <= ToolPreviewGeneric
}

// ToolRenderModel is the only production input to tool rendering. ToolEntry
// remains the durable lifecycle state; this strict projection is rebuilt at
// the transcript boundary and ToolCard stays a dumb, ephemeral component.
type ToolRenderModel struct {
	ToolViewModel
	Preview ToolPreview
}

// Validate fails closed before a projected invocation enters transcript UI.
func (view ToolViewModel) Validate() error {
	if !validTranscriptID(view.InvocationID) {
		return errors.New("tool invocation ID is required and must be canonical")
	}
	if !view.BlockID.Valid() {
		return errors.New("tool block ID is required and must be canonical")
	}
	if view.Revision == 0 {
		return errors.New("tool revision must be positive")
	}
	return view.validatePresentation()
}

func (view ToolViewModel) validatePresentation() error {
	if !view.Kind.Valid() {
		return errors.New("tool kind is invalid")
	}
	if !view.Lifecycle.Valid() {
		return errors.New("tool lifecycle is invalid")
	}
	if view.Operation == "" || view.Operation != boundedToolViewOperation(view.Operation) {
		return errors.New("tool operation is empty, unsafe, or exceeds its bound")
	}
	if view.Target != boundedToolViewTarget(view.Target) {
		return errors.New("tool target is unsafe or exceeds its bound")
	}
	if view.Summary != boundedToolCardSummary(view.Summary) {
		return errors.New("tool summary is unsafe or exceeds its bound")
	}
	if view.Duration < 0 || view.Duration > maxToolViewDuration {
		return fmt.Errorf("tool duration must be between zero and %s", maxToolViewDuration)
	}
	if !validToolProjectionStates(view.Transport, view.Domain, view.Evidence) {
		return errors.New("tool ecosystem state is invalid")
	}
	if !toolViewLifecycleMatchesProjection(view.Lifecycle, ecosystem.ToolProjection{
		Transport: view.Transport,
		Domain:    view.Domain,
		Evidence:  view.Evidence,
	}) {
		return errors.New("tool lifecycle contradicts its semantic projection")
	}
	if view.Artifact != nil {
		candidate := ecosystem.ToolProjection{
			Specialist: view.Target,
			Operation:  view.Operation,
			Role:       ecosystem.RoleArtifact,
			Transport:  view.Transport,
			Domain:     view.Domain,
			Evidence:   view.Evidence,
			Artifact:   view.Artifact,
		}.Normalize()
		if candidate.Artifact == nil || !reflect.DeepEqual(*candidate.Artifact, *view.Artifact) {
			return errors.New("tool artifact reference is invalid for its typed projection")
		}
	}
	projection := view.Projection.Normalize()
	if !reflect.DeepEqual(projection, view.Projection) {
		return errors.New("tool projection is not normalized")
	}
	if view.Lifecycle == ToolLifecycleCancelled {
		if err := validateCanonicalCancelledToolProjection(projection); err != nil {
			return err
		}
	}
	if projection.Transport != view.Transport || projection.Domain != view.Domain || projection.Evidence != view.Evidence {
		return errors.New("tool projection contradicts its semantic states")
	}
	if projection.Artifact != nil {
		if view.Artifact == nil || !reflect.DeepEqual(*projection.Artifact, *view.Artifact) {
			return errors.New("tool projection contradicts its artifact reference")
		}
	} else if view.Artifact != nil {
		return errors.New("tool artifact has no typed projection")
	}
	return nil
}

// Validate rejects any preview that did not pass through the strict bounded
// constructor. It intentionally checks values rather than trusting callers in
// the ui package because restored state is attacker-controlled input.
func (model ToolRenderModel) Validate() error {
	if err := model.ToolViewModel.Validate(); err != nil {
		return err
	}
	if model.ToolName == "" || model.ToolName != safeToolIdentifier(model.ToolName) {
		return errors.New("tool name is empty or unsafe")
	}
	preview := model.Preview
	if !preview.Mode.Valid() {
		return errors.New("tool preview mode is invalid")
	}
	if expected := previewModeForProjectedTool(model.ToolName, model.Projection); preview.Mode != expected {
		return errors.New("tool preview mode contradicts its typed operation")
	}
	if preview.Arguments != boundedToolPreviewArguments(model.ToolName, preview.Arguments, model.Projection) {
		return errors.New("tool preview arguments are unsafe or exceed their bound")
	}
	if preview.Result != boundedToolCardResult(preview.Result) {
		return errors.New("tool preview result is unsafe or exceeds its bound")
	}
	if preview.ResultLanguage != normalizeTrustedResultLanguage(preview.ResultLanguage) {
		return errors.New("tool preview language is not trusted")
	}
	if preview.OutputDigest != (OutputDetailDigest{}) && !preview.OutputDigest.Valid() {
		return errors.New("tool output digest is invalid")
	}
	if preview.OutputAvailable && preview.OutputDigest == (OutputDetailDigest{}) {
		return errors.New("tool output availability has no digest")
	}
	if isExpertConsultTool(model.ToolName) &&
		(preview.OutputAvailable || preview.OutputDigest != (OutputDetailDigest{})) {
		return errors.New("expert output cannot enter the tool preview")
	}
	if !reflect.DeepEqual(preview.DiffLines, boundedToolPreviewDiff(preview.DiffLines)) {
		return errors.New("tool preview diff is unsafe or exceeds its bound")
	}
	requireSettled := model.Lifecycle.Terminal()
	if !reflect.DeepEqual(preview.ExpertProgress, sanitizeExpertProgressState(preview.ExpertProgress, requireSettled)) {
		return errors.New("tool expert preview is invalid")
	}
	if err := validateANSIResultPreview(preview.ansiResultLines, preview.ansiHiddenLines); err != nil {
		return err
	}
	return nil
}

// ToolViewModelFromToolEntry projects one transcript tool block without
// carrying ToolEntry.Args, RawArgs, Result, ResultDisplay, or diff snapshots.
func ToolViewModelFromToolEntry(chat ChatEntry, entry ToolEntry) (ToolViewModel, error) {
	projection := entry.Projection.Normalize()
	if err := validateToolEntryProjectionConsistency(entry, projection); err != nil {
		return ToolViewModel{}, err
	}
	invocationID := entry.ID
	if invocationID == "" && chat.BlockID.Valid() {
		// Older local-only tools did not always receive a provider call ID. The
		// transcript block is already unique and durable, so derive an explicit
		// invocation identity instead of falling back to name correlation.
		invocationID = "invocation-" + string(chat.BlockID)
	}
	name := entry.Name
	if name == "" {
		name = "tool"
	}
	view := newToolViewModel(
		chat,
		invocationID,
		toolKindFromCardKind(toolCardKindForProjectedTool(entry.Name, projection)),
		name,
		entry.Summary,
		entry.Duration,
		toolLifecycleFromEntry(entry, projection),
		projection,
	)
	if err := view.Validate(); err != nil {
		return ToolViewModel{}, err
	}
	return view, nil
}

// ToolRenderModelFromEntry is the strict production adapter from transcript
// state into tool UI. Every copied body field is bounded and sanitized here;
// ephemeral arguments/snapshots and raw MCP StructuredContent cannot cross
// because ToolPreview has no fields capable of retaining them.
func ToolRenderModelFromEntry(chat ChatEntry, entry ToolEntry) (ToolRenderModel, error) {
	view, err := ToolViewModelFromToolEntry(chat, entry)
	if err != nil {
		return ToolRenderModel{}, err
	}
	ansiLines, ansiHidden := sanitizedANSIResultPreview(entry.ResultDisplay)
	preview := ToolPreview{
		Mode:            previewModeForProjectedTool(entry.Name, entry.Projection),
		Arguments:       boundedToolPreviewArguments(entry.Name, entry.Args, entry.Projection),
		Result:          boundedToolCardResult(entry.Result),
		ResultLanguage:  normalizeTrustedResultLanguage(entry.ResultLanguage),
		OutputDigest:    entry.OutputDetail.Digest,
		OutputAvailable: entry.OutputDetail.Ref.Valid() && entry.OutputDetail.Digest.Valid(),
		StartedAt:       entry.StartTime,
		Expanded:        !entry.Collapsed,
		DiffLines:       boundedToolPreviewDiff(entry.DiffLines),
		DiffPending:     entry.DiffPending,
		ExpertProgress:  sanitizeExpertProgressState(entry.ExpertProgress, view.Lifecycle.Terminal()),
		ansiResultLines: ansiLines,
		ansiHiddenLines: ansiHidden,
	}
	if isExpertConsultTool(entry.Name) {
		preview.OutputDigest = OutputDetailDigest{}
		preview.OutputAvailable = false
	}
	model := ToolRenderModel{ToolViewModel: view, Preview: preview}
	if err := model.Validate(); err != nil {
		return ToolRenderModel{}, err
	}
	return model, nil
}

// ToolViewModelFromToolCard projects the card compatibility model through the
// same narrow boundary used by transcript tool entries.
func ToolViewModelFromToolCard(chat ChatEntry, card ToolCard) (ToolViewModel, error) {
	projection := card.Projection.Normalize()
	if err := validateToolCardProjectionConsistency(card, projection); err != nil {
		return ToolViewModel{}, err
	}
	view := newToolViewModel(
		chat,
		card.ID,
		toolKindFromCardKind(card.Kind),
		card.Name,
		card.Summary,
		card.Duration,
		toolLifecycleFromCard(card, projection),
		projection,
	)
	if err := view.Validate(); err != nil {
		return ToolViewModel{}, err
	}
	return view, nil
}

func newToolViewModel(
	chat ChatEntry,
	invocationID string,
	kind ToolKind,
	name string,
	summary string,
	duration time.Duration,
	lifecycle ToolLifecycle,
	projection ecosystem.ToolProjection,
) ToolViewModel {
	projection = projection.Normalize()
	operation := projection.Operation
	if operation == "" {
		operation = name
	}
	if projected := projection.SummaryText(); projected != "" &&
		lifecycle != ToolLifecycleRunning && lifecycle != ToolLifecycleCancelled {
		summary = projected
	}
	var artifact *ecosystem.ArtifactDigest
	if projection.Artifact != nil {
		copy := *projection.Artifact
		artifact = &copy
	}
	return ToolViewModel{
		InvocationID: invocationID,
		BlockID:      chat.BlockID,
		ToolName:     boundedToolViewOperation(name),
		Kind:         kind,
		Operation:    boundedToolViewOperation(operation),
		Target:       projectedToolTarget(projection),
		Lifecycle:    lifecycle,
		Transport:    projection.Transport,
		Domain:       projection.Domain,
		Evidence:     projection.Evidence,
		Summary:      boundedToolCardSummary(summary),
		Duration:     duration,
		Revision:     chat.Revision,
		Artifact:     artifact,
		Projection:   projection,
	}
}

func boundedToolPreviewArguments(
	name, arguments string,
	projection ecosystem.ToolProjection,
) string {
	if agent.ToolArgumentsRequirePrivacy(name) {
		projection = projection.Normalize()
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
		arguments = agent.FormatToolArgsForTool(name, routeArgs)
	}
	arguments = sanitizeTerminalSingleLine(arguments)
	if len(arguments) > maxPersistedToolArgsBytes {
		arguments = truncateUTF8Bytes(arguments, maxPersistedToolArgsBytes)
	}
	return arguments
}

func boundedToolPreviewDiff(lines []DiffLine) []DiffLine {
	lines = persistDiffLines(lines)
	if len(lines) == 0 {
		return nil
	}
	result := make([]DiffLine, len(lines))
	for index, line := range lines {
		result[index] = line
		result[index].Content = sanitizeTerminalLine(line.Content)
		if line.Hunk != nil {
			hunk := *line.Hunk
			result[index].Hunk = &hunk
		}
	}
	return result
}

func sanitizedANSIResultPreview(result string) ([][]ansiRemapSegment, int) {
	result = boundedToolCardResultDisplay(result)
	if result == "" {
		return nil, 0
	}
	rawLines := strings.Split(strings.TrimRight(result, "\n"), "\n")
	hidden := 0
	if len(rawLines) > maxToolResultPreviewLines {
		hidden = len(rawLines) - maxToolResultPreviewLines
		rawLines = rawLines[:maxToolResultPreviewLines]
	}
	lines := make([][]ansiRemapSegment, 0, len(rawLines))
	for _, rawLine := range rawLines {
		parsed := parseANSI16Segments(rawLine)
		safe := make([]ansiRemapSegment, 0, len(parsed))
		for _, segment := range parsed {
			text := strings.ReplaceAll(sanitizeTerminalLine(segment.text), "\t", "    ")
			if text == "" {
				continue
			}
			safe = append(safe, ansiRemapSegment{text: text, bold: segment.bold, fg: segment.fg})
		}
		lines = append(lines, safe)
	}
	return lines, hidden
}

func validateANSIResultPreview(lines [][]ansiRemapSegment, hidden int) error {
	if hidden < 0 || hidden > maxToolCardResultDisplayBytes || len(lines) > maxToolResultPreviewLines {
		return errors.New("tool ANSI preview exceeds its row bound")
	}
	total := 0
	for _, line := range lines {
		for _, segment := range line {
			if segment.fg < ansiRemapDefaultFg || segment.fg > 7 ||
				segment.text == "" ||
				segment.text != strings.ReplaceAll(sanitizeTerminalLine(segment.text), "\t", "    ") {
				return errors.New("tool ANSI preview contains an unsafe segment")
			}
			total += len(segment.text)
			if total > maxToolCardResultDisplayBytes {
				return errors.New("tool ANSI preview exceeds its byte bound")
			}
		}
	}
	return nil
}

func boundedToolViewOperation(operation string) string {
	operation = safeToolIdentifier(operation)
	if len(operation) > maxToolViewOperationBytes {
		operation = truncateUTF8Bytes(operation, maxToolViewOperationBytes)
	}
	// truncateDisplay appends a typographic ellipsis. Tool operation identity
	// deliberately accepts only safe identifier characters, so canonicalize
	// once more after truncation to keep the constructor idempotent with
	// Validate instead of manufacturing an object that rejects itself.
	return safeToolIdentifier(truncateDisplay(operation, maxToolViewOperationCells))
}

func boundedToolViewTarget(target string) string {
	target = strings.TrimSpace(target)
	if len(target) > maxToolViewTargetBytes {
		target = truncateUTF8Bytes(target, maxToolViewTargetBytes)
	}
	if target != safeToolIdentifier(target) {
		return ""
	}
	return target
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func projectedToolTarget(projection ecosystem.ToolProjection) string {
	projection = projection.Normalize()
	switch {
	case projection.Digest != nil && projection.Digest.Target != "":
		return boundedToolViewTarget(projection.Digest.Target)
	case projection.Route.Server != "":
		return boundedToolViewTarget(projection.Route.Server)
	default:
		return boundedToolViewTarget(projection.Specialist)
	}
}

func toolKindFromCardKind(kind ToolCardKind) ToolKind {
	switch kind {
	case ToolCardFile:
		return ToolKindFile
	case ToolCardBash:
		return ToolKindShell
	case ToolCardSearch:
		return ToolKindSearch
	case ToolCardGit:
		return ToolKindGit
	default:
		return ToolKindGeneric
	}
}

func toolLifecycleFromEntry(entry ToolEntry, projection ecosystem.ToolProjection) ToolLifecycle {
	if entry.Status == ToolStatusCancelled {
		return ToolLifecycleCancelled
	}
	if entry.IsError || entry.Status == ToolStatusError {
		return ToolLifecycleFailed
	}
	if toolProjectionHasSemanticState(projection) {
		return toolLifecycleFromProjection(projection)
	}
	if entry.Status == ToolStatusRunning {
		return ToolLifecycleRunning
	}
	return ToolLifecycleSucceeded
}

func validateToolEntryProjectionConsistency(entry ToolEntry, projection ecosystem.ToolProjection) error {
	if entry.Status != ToolStatusRunning && entry.Status != ToolStatusDone &&
		entry.Status != ToolStatusError && entry.Status != ToolStatusCancelled {
		return errors.New("tool entry status is invalid")
	}
	if entry.Status == ToolStatusCancelled {
		return validateCanonicalCancelledToolProjection(projection)
	}
	if !toolProjectionHasSemanticState(projection) {
		return nil
	}
	projected := toolLifecycleFromProjection(projection)
	switch entry.Status {
	case ToolStatusRunning:
		if projected != ToolLifecycleRunning {
			return errors.New("running tool entry has a terminal semantic projection")
		}
	case ToolStatusDone:
		if projected == ToolLifecycleRunning {
			return errors.New("settled tool entry has a running semantic projection")
		}
	case ToolStatusError:
		if projected != ToolLifecycleFailed {
			return errors.New("failed tool entry has a non-failing semantic projection")
		}
	}
	if entry.IsError && projected != ToolLifecycleFailed {
		return errors.New("errored tool entry has a non-failing semantic projection")
	}
	return nil
}

func validateToolCardProjectionConsistency(card ToolCard, projection ecosystem.ToolProjection) error {
	if card.Lifecycle == ToolLifecycleCancelled {
		if card.State != ToolCardAttention {
			return errors.New("cancelled tool card is not in the attention state")
		}
		return validateCanonicalCancelledToolProjection(projection)
	}
	if !toolProjectionHasSemanticState(projection) {
		return nil
	}
	projected := toolLifecycleFromProjection(projection)
	var cardLifecycle ToolLifecycle
	switch card.State {
	case ToolCardRunning:
		cardLifecycle = ToolLifecycleRunning
	case ToolCardSuccess:
		cardLifecycle = ToolLifecycleSucceeded
	case ToolCardAttention:
		cardLifecycle = ToolLifecycleAttention
	case ToolCardError:
		cardLifecycle = ToolLifecycleFailed
	default:
		return errors.New("tool card state is invalid")
	}
	if cardLifecycle != projected {
		return errors.New("tool card state contradicts its semantic projection")
	}
	return nil
}

func toolProjectionHasSemanticState(projection ecosystem.ToolProjection) bool {
	return projection.Transport != "" ||
		projection.Domain != "" ||
		projection.Evidence != ecosystem.EvidenceNone
}

// validateCanonicalCancelledToolProjection enforces the sole semantic shape
// produced when the host cancels an in-flight tool. Routing identity may remain
// so the receipt can still identify the invocation, but cancellation can never
// carry a successful domain result, evidence, or a result/artifact digest.
func validateCanonicalCancelledToolProjection(projection ecosystem.ToolProjection) error {
	if projection.Digest != nil || projection.Artifact != nil {
		return errors.New("cancelled tool projection cannot contain a digest or artifact")
	}
	if normalized := projection.Normalize(); !reflect.DeepEqual(normalized, projection) {
		return errors.New("cancelled tool projection is not normalized")
	}
	if projection.Transport != ecosystem.TransportFailed ||
		projection.Domain != ecosystem.DomainUnknown ||
		projection.DomainTyped ||
		projection.Evidence != ecosystem.EvidenceNone {
		return errors.New("cancelled tool projection must be transport failed, domain unknown, untyped, and evidence none")
	}
	return nil
}

func toolViewLifecycleMatchesProjection(lifecycle ToolLifecycle, projection ecosystem.ToolProjection) bool {
	if lifecycle == ToolLifecycleCancelled {
		return projection.Transport == ecosystem.TransportFailed &&
			projection.Domain == ecosystem.DomainUnknown &&
			projection.Evidence == ecosystem.EvidenceNone
	}
	if !toolProjectionHasSemanticState(projection) {
		return true
	}
	return lifecycle == toolLifecycleFromProjection(projection)
}

func toolLifecycleFromCard(card ToolCard, projection ecosystem.ToolProjection) ToolLifecycle {
	if card.Lifecycle == ToolLifecycleCancelled {
		return ToolLifecycleCancelled
	}
	if toolProjectionHasSemanticState(projection) {
		return toolLifecycleFromProjection(projection)
	}
	switch card.State {
	case ToolCardRunning:
		return ToolLifecycleRunning
	case ToolCardError:
		return ToolLifecycleFailed
	case ToolCardAttention:
		return ToolLifecycleAttention
	default:
		return ToolLifecycleSucceeded
	}
}

func toolLifecycleFromProjection(projection ecosystem.ToolProjection) ToolLifecycle {
	switch toolCardStateFromProjection(projection) {
	case ToolCardRunning:
		return ToolLifecycleRunning
	case ToolCardError:
		return ToolLifecycleFailed
	case ToolCardAttention:
		return ToolLifecycleAttention
	default:
		return ToolLifecycleSucceeded
	}
}

func validToolProjectionStates(transport ecosystem.TransportState, domain ecosystem.DomainState, evidence ecosystem.EvidenceState) bool {
	switch transport {
	case "", ecosystem.TransportRunning, ecosystem.TransportSucceeded, ecosystem.TransportFailed:
	default:
		return false
	}
	switch domain {
	case "", ecosystem.DomainPending, ecosystem.DomainUnknown, ecosystem.DomainSucceeded,
		ecosystem.DomainAttention, ecosystem.DomainFailed, ecosystem.DomainBlocked,
		ecosystem.DomainConflict, ecosystem.DomainDrift:
	default:
		return false
	}
	switch evidence {
	case ecosystem.EvidenceNone, ecosystem.EvidenceCandidate, ecosystem.EvidenceSupported,
		ecosystem.EvidenceVerified, ecosystem.EvidenceContradicted, ecosystem.EvidenceStale:
	default:
		return false
	}
	return true
}

// toolHeaderCellBudget is a pure, style-independent projection of the compact
// header. Cell budgets use terminal display width, never bytes or rune counts.
type toolHeaderCellBudget struct {
	NameCells    int
	SummaryCells int
	ShowDuration bool
}

func projectToolHeaderCellBudget(inner, glyphCells, nameCells, summaryCells, durationCells int, wantSummary bool) toolHeaderCellBudget {
	inner = max(0, inner)
	glyphCells = max(0, glyphCells)
	nameCells = max(0, nameCells)
	summaryCells = max(0, summaryCells)
	durationCells = max(0, durationCells)

	// One cell separates the lifecycle glyph from the operation.
	textCells := max(0, inner-glyphCells-1)
	if textCells == 0 || nameCells == 0 {
		return toolHeaderCellBudget{}
	}

	// Verb and object are joined by a single space (1 cell), not a middle-dot
	// separator. Thresholds below keep at least one operation cell when a
	// summary is shown.
	const summarySepCells = 1

	summaryCap := 0
	if wantSummary && summaryCells > 0 && textCells >= 5 {
		summaryCap = min(summaryCells, textCells/2)
		if textCells-summaryCap-summarySepCells < 1 {
			summaryCap = 0
		}
	}

	// Duration is tertiary metadata: retain it only when the complete operation
	// and the normal summary allocation already fit. It is therefore the first
	// field to disappear as width contracts.
	showDuration := durationCells > 0
	required := nameCells
	if summaryCap > 0 {
		required += summarySepCells + summaryCap
	}
	if showDuration {
		required += 1 + durationCells
		if required > textCells {
			showDuration = false
		}
	}

	available := textCells
	if showDuration {
		available -= durationCells + 1
	}
	summaryBudget := 0
	if wantSummary && summaryCells > 0 && available >= 5 {
		summaryBudget = min(summaryCells, available/2)
		if available-summaryBudget-summarySepCells < 1 {
			summaryBudget = 0
		}
	}
	nameBudget := available
	if summaryBudget > 0 {
		nameBudget -= summaryBudget + summarySepCells
	}
	nameBudget = min(nameCells, max(0, nameBudget))
	if summaryBudget > 0 {
		// A short operation may donate cells it cannot use. The summary's base
		// allocation still tops out at half; borrowing only consumes otherwise
		// empty cells and never displaces operation identity.
		unusedNameCells := available - summaryBudget - summarySepCells - nameBudget
		if unusedNameCells > 0 {
			summaryBudget = min(summaryCells, summaryBudget+unusedNameCells)
		}
	}

	return toolHeaderCellBudget{
		NameCells:    nameBudget,
		SummaryCells: summaryBudget,
		ShowDuration: showDuration,
	}
}
