package command

import "strings"

// ActionID is a stable identity shared by slash completion, Help, and
// transient action surfaces. It deliberately describes user intent rather
// than a key binding or presentation component.
type ActionID string

const (
	GoalActionNew               ActionID = "goal.new"
	GoalActionInspect           ActionID = "goal.inspect"
	GoalActionPause             ActionID = "goal.pause"
	GoalActionResume            ActionID = "goal.resume"
	GoalActionBudget            ActionID = "goal.budget"
	GoalActionDrop              ActionID = "goal.drop"
	ScopeActionAddRead          ActionID = "scope.add-read"
	ScopeActionRemoveRead       ActionID = "scope.remove-read"
	ScopeActionClearRead        ActionID = "scope.clear-read"
	ImageActionList             ActionID = "image.list"
	ImageActionClear            ActionID = "image.clear"
	ImageActionForgetHistory    ActionID = "image.forget-history"
	PermissionsActionClear      ActionID = "permissions.clear"
	PermissionsActionRevoke     ActionID = "permissions.revoke"
	PermissionsActionAllowBash  ActionID = "permissions.allow-bash"
	PermissionsActionAllowMCP   ActionID = "permissions.allow-mcp"
	PermissionsActionAllowPath  ActionID = "permissions.allow-path"
	PermissionsActionForgetBash ActionID = "permissions.forget-bash"
	PermissionsActionForgetMCP  ActionID = "permissions.forget-mcp"
	PermissionsActionForgetPath ActionID = "permissions.forget-path"
	PermissionsActionExport     ActionID = "permissions.export"
	PermissionsActionImport     ActionID = "permissions.import"
	PermissionsActionClearRules ActionID = "permissions.clear-rules"
	PermissionsActionPanel      ActionID = "permissions.panel"
)

// ActionSpec is the bounded metadata needed to expose one command action in
// multiple UI surfaces. The parent Bubble Tea model remains the sole authority
// that executes Action.
type ActionSpec struct {
	ID          ActionID
	Command     string
	Argument    string
	Aliases     []string
	Title       string
	Description string
	Action      Action
	Destructive bool
}

// ActionState is ActionSpec resolved against the current command context.
// DisabledReason is user-facing copy shared by commands and UI surfaces.
type ActionState struct {
	Spec           ActionSpec
	Enabled        bool
	DisabledReason string
}

func (s ActionSpec) CommandText() string {
	parts := []string{"/" + strings.TrimSpace(s.Command)}
	if argument := strings.TrimSpace(s.Argument); argument != "" {
		parts = append(parts, argument)
	}
	return strings.Join(parts, " ")
}

func (s ActionSpec) MatchesArgument(argument string) bool {
	argument = strings.ToLower(strings.TrimSpace(argument))
	if argument == strings.ToLower(s.Argument) {
		return true
	}
	for _, alias := range s.Aliases {
		if argument == strings.ToLower(alias) {
			return true
		}
	}
	return false
}

// RegisterAction adds or replaces one action without changing its stable
// display position. This lets built-ins remain deterministic in Help and
// completion while still permitting focused overrides.
func (r *Registry) RegisterAction(spec ActionSpec) {
	if r == nil || spec.ID == "" || strings.TrimSpace(spec.Command) == "" || strings.TrimSpace(spec.Argument) == "" {
		return
	}
	if r.actions == nil {
		r.actions = make(map[ActionID]ActionSpec)
	}
	if _, exists := r.actions[spec.ID]; !exists {
		r.actionOrder = append(r.actionOrder, spec.ID)
	}
	r.actions[spec.ID] = spec
}

func (r *Registry) Action(id ActionID) (ActionSpec, bool) {
	if r == nil {
		return ActionSpec{}, false
	}
	spec, ok := r.actions[id]
	return spec, ok
}

func (r *Registry) MatchAction(commandName, argument string) (ActionSpec, bool) {
	for _, state := range r.Actions(commandName, nil) {
		if state.Spec.MatchesArgument(argument) {
			return state.Spec, true
		}
	}
	return ActionSpec{}, false
}

// Actions returns the registered actions for one canonical command in stable
// order, resolved against the current state when context is available.
func (r *Registry) Actions(commandName string, ctx *Context) []ActionState {
	if r == nil {
		return nil
	}
	commandName = strings.ToLower(strings.TrimSpace(commandName))
	states := make([]ActionState, 0, len(r.actionOrder))
	for _, id := range r.actionOrder {
		spec := r.actions[id]
		if strings.ToLower(spec.Command) != commandName {
			continue
		}
		states = append(states, resolveActionState(spec, ctx))
	}
	return states
}

func resolveActionState(spec ActionSpec, ctx *Context) ActionState {
	state := ActionState{Spec: spec, Enabled: true}
	if ctx == nil || spec.Command != "goal" {
		return state
	}

	status := strings.ToLower(strings.TrimSpace(ctx.GoalStatus))
	terminal := status == "completed" || status == "dropped"
	switch spec.ID {
	case GoalActionNew:
		if ctx.GoalConfigured && !terminal {
			state.Enabled = false
			state.DisabledReason = "Drop or complete the current goal first."
		}
	case GoalActionInspect:
		if !ctx.GoalConfigured {
			state.Enabled = false
			state.DisabledReason = "No goal is configured."
		}
	case GoalActionPause:
		state = resolveGoalPauseState(state, ctx, status)
	case GoalActionResume:
		state = resolveGoalResumeState(state, ctx, status, terminal)
	case GoalActionBudget:
		state = resolveGoalMutableState(state, ctx, status, terminal)
	case GoalActionDrop:
		state = resolveGoalDropState(state, ctx, status, terminal)
	}
	return state
}

func resolveGoalPauseState(state ActionState, ctx *Context, status string) ActionState {
	switch {
	case !ctx.GoalConfigured:
		return disableAction(state, "No goal is configured.")
	case ctx.GoalBusy:
		return disableAction(state, "Wait for the current goal operation to settle.")
	case ctx.GoalPersistenceDirty:
		return disableAction(state, "Recover goal persistence before changing state.")
	case ctx.GoalPending:
		return disableAction(state, "Settle or reconcile the admitted turn first.")
	case status != "active":
		return disableAction(state, "Only an active goal can be paused.")
	default:
		return state
	}
}

func resolveGoalResumeState(state ActionState, ctx *Context, status string, terminal bool) ActionState {
	switch {
	case !ctx.GoalConfigured:
		return disableAction(state, "No goal is configured.")
	case ctx.GoalBusy:
		return disableAction(state, "Wait for the current goal operation to settle.")
	case ctx.GoalPersistenceDirty:
		return disableAction(state, "Recover goal persistence before resuming.")
	case terminal:
		return disableAction(state, "The goal is already "+status+".")
	case ctx.GoalPending:
		return disableAction(state, "Settle or reconcile the admitted turn first.")
	case ctx.GoalBlocker == "outcome_unknown":
		return disableAction(state, "Reconcile the unknown external outcome first.")
	case status == "exhausted" && ctx.GoalExhausted:
		return disableAction(state, "Increase the exhausted budget before resuming.")
	default:
		return state
	}
}

func resolveGoalMutableState(state ActionState, ctx *Context, status string, terminal bool) ActionState {
	switch {
	case !ctx.GoalConfigured:
		return disableAction(state, "No goal is configured.")
	case ctx.GoalBusy:
		return disableAction(state, "Wait for the current goal operation to settle.")
	case terminal:
		return disableAction(state, "The goal is already "+status+".")
	default:
		return state
	}
}

func resolveGoalDropState(state ActionState, ctx *Context, status string, terminal bool) ActionState {
	state = resolveGoalMutableState(state, ctx, status, terminal)
	if !state.Enabled {
		return state
	}
	switch {
	case ctx.GoalPersistenceDirty:
		return disableAction(state, "Recover goal persistence before dropping it.")
	case ctx.GoalPending:
		return disableAction(state, "Settle or reconcile the admitted turn first.")
	default:
		return state
	}
}

func disableAction(state ActionState, reason string) ActionState {
	state.Enabled = false
	state.DisabledReason = reason
	return state
}

func registerGoalActions(r *Registry) {
	for _, spec := range []ActionSpec{
		{
			ID: GoalActionNew, Command: "goal", Argument: "new", Aliases: []string{"set"},
			Title: "New goal", Description: "Define a durable objective, acceptance, and budget", Action: ActionOpenGoal,
		},
		{
			ID: GoalActionInspect, Command: "goal", Argument: "show", Aliases: []string{"status"},
			Title: "Inspect goal", Description: "Review progress, evidence, blockers, and controls", Action: ActionShowGoal,
		},
		{
			ID: GoalActionPause, Command: "goal", Argument: "pause",
			Title: "Pause", Description: "Stop automatic continuation after settled work", Action: ActionPauseGoal,
		},
		{
			ID: GoalActionResume, Command: "goal", Argument: "resume", Aliases: []string{"retry"},
			Title: "Resume", Description: "Resume or safely retry goal evaluation", Action: ActionResumeGoal,
		},
		{
			ID: GoalActionBudget, Command: "goal", Argument: "budget", Aliases: []string{"edit"},
			Title: "Budget", Description: "Adjust limits without changing goal definition", Action: ActionEditGoalBudget,
		},
		{
			ID: GoalActionDrop, Command: "goal", Argument: "drop",
			Title: "Drop", Description: "Abandon without claiming completion", Action: ActionDropGoal, Destructive: true,
		},
	} {
		r.RegisterAction(spec)
	}
}

func registerScopeActions(r *Registry) {
	for _, spec := range []ActionSpec{
		{
			ID: ScopeActionAddRead, Command: "scope", Argument: "add-read", Aliases: []string{"add", "mount"},
			Title: "Add read root", Description: "Grant process-local read-only access to an external directory", Action: ActionAddReadRoot,
		},
		{
			ID: ScopeActionRemoveRead, Command: "scope", Argument: "remove-read", Aliases: []string{"remove", "unmount"},
			Title: "Remove read grant", Description: "Revoke one external directory or exact-file read grant", Action: ActionRemoveReadRoot,
		},
		{
			ID: ScopeActionClearRead, Command: "scope", Argument: "clear-read", Aliases: []string{"clear"},
			Title: "Clear read grants", Description: "Revoke every temporary external read-only grant", Action: ActionClearReadRoots,
		},
	} {
		r.RegisterAction(spec)
	}
}

func registerImageActions(r *Registry) {
	for _, spec := range []ActionSpec{
		{
			ID: ImageActionList, Command: "image", Argument: "list", Aliases: []string{"ls"},
			Title: "List images", Description: "Show images attached to the pending prompt", Action: ActionListImages,
		},
		{
			ID: ImageActionClear, Command: "image", Argument: "clear", Aliases: []string{"remove-all"},
			Title: "Clear images", Description: "Remove every image from the pending prompt", Action: ActionClearImages, Destructive: true,
		},
		{
			ID: ImageActionForgetHistory, Command: "image", Argument: "forget-history", Aliases: []string{"drop-history"},
			Title: "Forget image history", Description: "Remove active conversation image references; checkpoints and cached objects remain", Action: ActionForgetImageHistory, Destructive: true,
		},
	} {
		r.RegisterAction(spec)
	}
}

func registerPermissionsActions(r *Registry) {
	for _, spec := range []ActionSpec{
		{
			ID: PermissionsActionClear, Command: "permissions", Argument: "clear",
			Title: "Clear session approvals", Description: "Drop every process-local session approval grant", Action: ActionPermissionsClear, Destructive: true,
		},
		{
			ID: PermissionsActionRevoke, Command: "permissions", Argument: "revoke",
			Title: "Revoke session approvals", Description: "Drop process-local session grants for one tool, or all when omitted", Action: ActionPermissionsRevoke, Destructive: true,
		},
		{
			ID: PermissionsActionAllowBash, Command: "permissions", Argument: "allow-bash",
			Title: "Allow bash prefix", Description: "Save a durable bash command prefix for this workspace", Action: ActionPermissionsAllowBash,
		},
		{
			ID: PermissionsActionAllowMCP, Command: "permissions", Argument: "allow-mcp",
			Title: "Allow MCP tool", Description: "Save a durable exact MCP tool allow for this workspace", Action: ActionPermissionsAllowMCP,
		},
		{
			ID: PermissionsActionAllowPath, Command: "permissions", Argument: "allow-path",
			Title: "Allow write path", Description: "Save a durable write/edit/mkdir path for this workspace", Action: ActionPermissionsAllowPath,
		},
		{
			ID: PermissionsActionForgetBash, Command: "permissions", Argument: "forget-bash",
			Title: "Forget bash prefix", Description: "Remove a durable bash prefix from this workspace", Action: ActionPermissionsForgetBash, Destructive: true,
		},
		{
			ID: PermissionsActionForgetMCP, Command: "permissions", Argument: "forget-mcp",
			Title: "Forget MCP tool", Description: "Remove a durable MCP tool allow from this workspace", Action: ActionPermissionsForgetMCP, Destructive: true,
		},
		{
			ID: PermissionsActionForgetPath, Command: "permissions", Argument: "forget-path",
			Title: "Forget write path", Description: "Remove a durable write path from this workspace", Action: ActionPermissionsForgetPath, Destructive: true,
		},
		{
			ID: PermissionsActionExport, Command: "permissions", Argument: "export",
			Title: "Export rules", Description: "Write durable workspace rules to a portable JSON file", Action: ActionPermissionsExport,
		},
		{
			ID: PermissionsActionImport, Command: "permissions", Argument: "import",
			Title: "Import rules", Description: "Merge durable rules from a portable JSON file", Action: ActionPermissionsImport,
		},
		{
			ID: PermissionsActionClearRules, Command: "permissions", Argument: "clear-rules",
			Title: "Clear workspace rules", Description: "Remove every durable bash/MCP/path rule for this workspace", Action: ActionPermissionsClearRules, Destructive: true,
		},
		{
			ID: PermissionsActionPanel, Command: "permissions", Argument: "panel", Aliases: []string{"ui", "manage"},
			Title: "Permissions panel", Description: "Open the interactive permissions manager", Action: ActionPermissionsPanel,
		},
	} {
		r.RegisterAction(spec)
	}
}
