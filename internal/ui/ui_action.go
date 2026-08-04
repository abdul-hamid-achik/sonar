package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

const (
	maxUIActionLabelCells  = 160
	maxUIActionReasonCells = 320
	maxUIActionShortcutKey = 64
	maxUIActionHelpKey     = 64
	maxUIActionHelpDesc    = 160
	maxUIActionShortcuts   = 8
)

const (
	uiActionUnavailableReason = "Action unavailable."
	uiActionMissingReason     = "Action is no longer available."
	uiActionTargetReason      = "Action target is no longer available."
	uiActionSourceReason      = "Action request source is invalid."
)

// EntityKind identifies the scalar domain identity carried by a UI action.
// The reference deliberately excludes presentation models, raw tool payloads,
// and provider objects.
type EntityKind uint8

const (
	EntityKindNone EntityKind = iota
	EntityKindTranscriptBlock
	EntityKindToolInvocation
	EntityKindAgentInvocation
	EntityKindOverlay
	EntityKindDiffLocation
)

// Valid reports whether kind is a supported action target category.
func (kind EntityKind) Valid() bool {
	return kind <= EntityKindDiffLocation
}

// OverlayID is a bounded opaque identity for a modal surface. It is separate
// from the legacy OverlayKind enum: OverlayID identifies one instance while
// OverlayKind identifies a legacy presentation category.
type OverlayID string

// Valid reports whether id is a bounded, terminal-safe opaque identity.
func (id OverlayID) Valid() bool {
	return validOpaqueUIID(string(id))
}

// EntityRef is the complete target admitted across the UI action boundary.
// Every field is scalar so keyboard and pointer input produce the same
// inspectable request without closures or raw domain payloads.
type EntityRef struct {
	Kind         EntityKind
	BlockID      BlockID
	InvocationID string
	OverlayID    OverlayID
	FileIndex    int
	HunkIndex    int
	LineIndex    int
}

// Valid reports whether ref contains the minimum identity required by its
// category. Extra scalar ancestry is allowed (for example, a diff location
// may retain both its transcript block and tool invocation).
func (ref EntityRef) Valid() bool {
	if !ref.Kind.Valid() || ref.FileIndex < 0 || ref.HunkIndex < 0 || ref.LineIndex < 0 {
		return false
	}
	if ref.BlockID != "" && !ref.BlockID.Valid() {
		return false
	}
	if ref.InvocationID != "" && !validOpaqueUIID(ref.InvocationID) {
		return false
	}
	if ref.OverlayID != "" && !ref.OverlayID.Valid() {
		return false
	}

	switch ref.Kind {
	case EntityKindNone:
		return ref.BlockID == "" && ref.InvocationID == "" && ref.OverlayID == "" &&
			ref.FileIndex == 0 && ref.HunkIndex == 0 && ref.LineIndex == 0
	case EntityKindTranscriptBlock:
		return ref.BlockID.Valid()
	case EntityKindToolInvocation, EntityKindAgentInvocation:
		return validOpaqueUIID(ref.InvocationID)
	case EntityKindOverlay:
		return ref.OverlayID.Valid()
	case EntityKindDiffLocation:
		return ref.BlockID.Valid()
	default:
		return false
	}
}

// UIActionSpec is immutable presentation metadata for one stable action ID.
// Resolve supplies only current, scalar state and remains side-effect free.
type UIActionSpec struct {
	ID       command.ActionID
	Label    string
	Shortcut key.Binding
}

// Resolve projects a static action specification against current UI state.
func (spec UIActionSpec) Resolve(target EntityRef, enabled bool, reason string) UIAction {
	return UIAction{
		ID:       spec.ID,
		Label:    spec.Label,
		Shortcut: cloneKeyBinding(spec.Shortcut),
		Enabled:  enabled,
		Reason:   reason,
		Target:   target,
	}
}

// UIAction is the bounded presentation and dispatch state of one action. It
// contains no function or arbitrary payload; the smart parent dispatches the
// accepted ActionID with a typed EntityRef.
type UIAction struct {
	ID       command.ActionID
	Label    string
	Shortcut key.Binding
	Enabled  bool
	Reason   string
	Target   EntityRef
}

// Request creates the common request used by keyboard and pointer paths.
func (action UIAction) Request(source UIActionSource) UIActionRequest {
	return UIActionRequest{
		ActionID: action.ID,
		Target:   action.Target,
		Source:   source,
	}
}

// UIActionSource identifies the physical input path that requested an action.
// It does not affect authorization; both paths pass through ResolveRequest.
type UIActionSource uint8

const (
	UIActionSourceUnknown UIActionSource = iota
	UIActionSourceKeyboard
	UIActionSourceMouse
)

// Valid reports whether source is a supported user input path.
func (source UIActionSource) Valid() bool {
	return source == UIActionSourceKeyboard || source == UIActionSourceMouse
}

// UIActionRequest is the only dispatch request emitted by action surfaces.
type UIActionRequest struct {
	ActionID command.ActionID
	Target   EntityRef
	Source   UIActionSource
}

// UIActionRegistry keeps resolved actions in deterministic registration order.
// Re-registering an ID replaces its current state without moving its position.
// The zero value is ready to use.
type UIActionRegistry struct {
	actions map[command.ActionID]UIAction
	order   []command.ActionID
}

// NewUIActionRegistry creates a registry and registers actions in argument
// order. Invalid actions are ignored, matching Register's fail-closed policy.
func NewUIActionRegistry(actions ...UIAction) *UIActionRegistry {
	registry := &UIActionRegistry{}
	for _, action := range actions {
		registry.Register(action)
	}
	return registry
}

// Register adds or replaces one action. It returns false when the action
// cannot safely cross the UI dispatch boundary.
func (registry *UIActionRegistry) Register(action UIAction) bool {
	if registry == nil {
		return false
	}
	normalized, ok := normalizeUIAction(action)
	if !ok {
		return false
	}
	if registry.actions == nil {
		registry.actions = make(map[command.ActionID]UIAction)
	}
	if _, exists := registry.actions[normalized.ID]; !exists {
		registry.order = append(registry.order, normalized.ID)
	}
	registry.actions[normalized.ID] = normalized
	return true
}

// Action returns an isolated copy of one registered action.
func (registry *UIActionRegistry) Action(id command.ActionID) (UIAction, bool) {
	if registry == nil {
		return UIAction{}, false
	}
	action, ok := registry.actions[id]
	if !ok {
		return UIAction{}, false
	}
	return cloneUIAction(action), true
}

// Actions returns isolated action copies in stable registration order.
func (registry *UIActionRegistry) Actions() []UIAction {
	if registry == nil || len(registry.order) == 0 {
		return nil
	}
	actions := make([]UIAction, 0, len(registry.order))
	for _, id := range registry.order {
		action, exists := registry.actions[id]
		if exists {
			actions = append(actions, cloneUIAction(action))
		}
	}
	return actions
}

// ResolveRequest admits a keyboard or pointer request only when its action is
// still registered, enabled, and bound to the exact current target. The
// returned reason is suitable for a short status notice; unknown and malformed
// requests always fail closed.
func (registry *UIActionRegistry) ResolveRequest(request UIActionRequest) (UIAction, string, bool) {
	if !request.Source.Valid() {
		return UIAction{}, uiActionSourceReason, false
	}
	if !validOpaqueUIID(string(request.ActionID)) || !request.Target.Valid() {
		return UIAction{}, uiActionMissingReason, false
	}
	action, exists := registry.Action(request.ActionID)
	if !exists {
		return UIAction{}, uiActionMissingReason, false
	}
	if action.Target != request.Target {
		return UIAction{}, uiActionTargetReason, false
	}
	if !action.Enabled {
		reason := action.Reason
		if reason == "" {
			reason = uiActionUnavailableReason
		}
		return action, reason, false
	}
	return action, "", true
}

func normalizeUIAction(action UIAction) (UIAction, bool) {
	if !validOpaqueUIID(string(action.ID)) || !action.Target.Valid() {
		return UIAction{}, false
	}
	action.Label = truncateDisplay(sanitizeTerminalSingleLine(action.Label), maxUIActionLabelCells)
	if action.Label == "" || !validKeyBinding(action.Shortcut) {
		return UIAction{}, false
	}
	action.Shortcut = cloneKeyBinding(action.Shortcut)
	if action.Enabled {
		action.Reason = ""
		return action, true
	}
	action.Reason = truncateDisplay(sanitizeTerminalSingleLine(action.Reason), maxUIActionReasonCells)
	if action.Reason == "" {
		action.Reason = uiActionUnavailableReason
	}
	return action, true
}

func cloneUIAction(action UIAction) UIAction {
	action.Shortcut = cloneKeyBinding(action.Shortcut)
	return action
}

func cloneKeyBinding(binding key.Binding) key.Binding {
	keys := append([]string(nil), binding.Keys()...)
	help := binding.Help()
	options := make([]key.BindingOpt, 0, 3)
	if len(keys) > 0 {
		options = append(options, key.WithKeys(keys...))
	}
	if help.Key != "" || help.Desc != "" {
		options = append(options, key.WithHelp(
			truncateDisplay(sanitizeTerminalSingleLine(help.Key), maxUIActionHelpKey),
			truncateDisplay(sanitizeTerminalSingleLine(help.Desc), maxUIActionHelpDesc),
		))
	}
	cloned := key.NewBinding(options...)
	if !binding.Enabled() && len(keys) > 0 {
		cloned.SetEnabled(false)
	}
	return cloned
}

func validKeyBinding(binding key.Binding) bool {
	keys := binding.Keys()
	if len(keys) > maxUIActionShortcuts {
		return false
	}
	for _, shortcut := range keys {
		if shortcut == "" || len(shortcut) > maxUIActionShortcutKey ||
			sanitizeTerminalSingleLine(shortcut) != shortcut {
			return false
		}
	}
	return true
}

func validOpaqueUIID(id string) bool {
	return id != "" && len(id) <= maxTranscriptIDBytes &&
		strings.TrimSpace(id) == id && validTranscriptID(id)
}

// HitRegion is one exact half-open cell rectangle in a rendered action
// projection. order is assigned by HitRegionSet and models paint order.
type HitRegion struct {
	Rect     CellRect
	Z        int
	ActionID command.ActionID
	Target   EntityRef
	order    uint64
}

// Order returns the deterministic insertion order used to break equal-Z hits.
func (region HitRegion) Order() uint64 {
	return region.order
}

// Request returns the same typed request used by keyboard activation.
func (region HitRegion) Request() UIActionRequest {
	return UIActionRequest{
		ActionID: region.ActionID,
		Target:   region.Target,
		Source:   UIActionSourceMouse,
	}
}

// HitRegionSet owns the pointer projection for one rendered frame. The zero
// value is ready to use; callers should rebuild it whenever layout changes.
type HitRegionSet struct {
	regions   []HitRegion
	nextOrder uint64
}

// Add appends an exact hit rectangle. Empty or malformed regions fail closed.
func (set *HitRegionSet) Add(rect CellRect, z int, actionID command.ActionID, target EntityRef) bool {
	if set == nil || !validOpaqueUIID(string(actionID)) || !target.Valid() {
		return false
	}
	rect = rect.canonical()
	if rect.Empty() {
		return false
	}
	set.nextOrder++
	set.regions = append(set.regions, HitRegion{
		Rect:     rect,
		Z:        z,
		ActionID: actionID,
		Target:   target,
		order:    set.nextOrder,
	})
	return true
}

// Regions returns a copy in paint order.
func (set *HitRegionSet) Regions() []HitRegion {
	if set == nil || len(set.regions) == 0 {
		return nil
	}
	return append([]HitRegion(nil), set.regions...)
}

// Hit resolves one terminal cell to the visually top-most request. Higher Z
// wins; within the same Z the most recently painted region wins.
func (set *HitRegionSet) Hit(x, y int) (UIActionRequest, bool) {
	if set == nil {
		return UIActionRequest{}, false
	}
	best := -1
	for index := range set.regions {
		region := set.regions[index]
		if !region.Rect.Contains(x, y) {
			continue
		}
		if best < 0 || region.Z > set.regions[best].Z ||
			(region.Z == set.regions[best].Z && region.order > set.regions[best].order) {
			best = index
		}
	}
	if best < 0 {
		return UIActionRequest{}, false
	}
	return set.regions[best].Request(), true
}

// Reset removes all regions and restarts paint order for a fresh frame.
func (set *HitRegionSet) Reset() {
	if set == nil {
		return
	}
	set.regions = nil
	set.nextOrder = 0
}
