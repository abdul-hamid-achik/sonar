package ui

import (
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// Mode represents the conversational preset selected in the TUI. Durable goal
// execution is an explicit, separate lifecycle entered through /goal.
type Mode int

const (
	ModeNormal Mode = iota // Interactive work with approval-gated mutations.
	ModePlan               // Read-only exploration and planning.
	ModeAuto               // Proactive work with full tools and configured approvals.
)

// Legacy source aliases keep embeddings and older tests compiling while saved
// ASK/BUILD values migrate through the session-version boundary below.
const (
	ModeAsk   = ModeNormal
	ModeBuild = ModeAuto
)

// ModeConfig holds the configuration for a single mode.
type ModeConfig struct {
	Label               string
	SystemPromptPrefix  string
	ToolPolicy          agent.ToolPolicy
	PreferredCapability config.ModelCapability
	RouterMode          config.ModeContext
}

// DefaultModeConfigs returns the configuration for each mode.
func DefaultModeConfigs() [3]ModeConfig {
	return [3]ModeConfig{
		{ // ModeNormal
			Label:               "NORMAL",
			SystemPromptPrefix:  "Work interactively with the user. Use tools when useful; every mutation remains subject to the configured approval policy.",
			ToolPolicy:          agent.BuildToolPolicy(),
			PreferredCapability: config.CapabilityAdvanced,
			RouterMode:          config.ModeBuildContext,
		},
		{ // ModePlan
			Label:               "PLAN",
			SystemPromptPrefix:  "Help the user plan and design. Break down tasks into steps. Use tools to read and explore, but do not modify files.",
			ToolPolicy:          agent.PlanToolPolicy(),
			PreferredCapability: config.CapabilityComplex,
			RouterMode:          config.ModePlanContext,
		},
		{ // ModeAuto
			Label:               "AUTO",
			SystemPromptPrefix:  "Work proactively until the user's request is implemented and verified. Do not ask for progress confirmation when the objective and available evidence are clear. Workspace-confined file changes, host-catalogued local ecosystem operations, and static routine development commands for building, testing, linting, formatting, and inspection may proceed automatically. Those development commands can execute repository-owned code with the sonar process's filesystem and network access; AUTO treats the current workspace's development logic as trusted. Temporary external scopes explicitly listed in Environment may be read or changed only through the exact typed capability shown there; they never widen shell authority and expire when the turn settles. Unlisted external paths remain unavailable. Pause only for Git, destructive or dynamic shell behavior, file redirection, explicit network CLIs or endpoints, unknown commands, secrets, genuine human decisions, and uncatalogued tools.",
			ToolPolicy:          agent.BuildToolPolicy(),
			PreferredCapability: config.CapabilityAdvanced,
			RouterMode:          config.ModeBuildContext,
		},
	}
}

func agentAuthorityMode(mode Mode) agent.AuthorityMode {
	switch mode {
	case ModePlan:
		return agent.AuthorityPlan
	case ModeAuto:
		return agent.AuthorityAutoScoped
	default:
		return agent.AuthorityNormal
	}
}

// presentedMode is the authority currently communicated by the TUI. The
// ambient selector remains in m.mode so Shift+Tab can prepare a later
// conversational turn, but an attached Goal Runtime owns AUTO authority until
// the session is reset. Rendering the ambient value during that lifecycle
// would claim a PLAN/NORMAL contract that the next goal turn does not use.
func (m *Model) presentedMode() Mode {
	if m != nil && m.goalRuntime != nil {
		return ModeAuto
	}
	if m == nil || m.mode < ModeNormal || m.mode > ModeAuto {
		return ModeNormal
	}
	return m.mode
}

func (m *Model) syncComposerAuthority() {
	if m == nil {
		return
	}
	configureComposerModeWithGlyphProfile(
		&m.input,
		m.isDark,
		m.themeID,
		m.presentedMode(),
		m.reducedMotion,
		m.glyphProfile,
	)
}

// cycleMode advances through NORMAL -> PLAN -> AUTO -> NORMAL.
func (m *Model) cycleMode() {
	m.setMode((m.mode + 1) % 3)
}

// setMode commits one mode transition. Picker navigation never calls this;
// the route, model, and durable transcript change only on selection.
func (m *Model) setMode(mode Mode) {
	if mode < ModeNormal || mode > ModeAuto || mode == m.mode {
		return
	}
	m.mode = mode
	ambientConfig := m.modeConfigs[mode]
	m.syncComposerAuthority()
	// With a Goal Runtime attached, Shift+Tab only prepares the ambient mode for
	// work after the goal. The active router/model authority remains AUTO, just
	// like the rail and footer. Otherwise a visible AUTO goal could silently
	// inherit PLAN routing until its next continuation reasserted authority.
	authorityConfig := m.modeConfigs[m.presentedMode()]
	m.setRouterMode(authorityConfig.RouterMode)

	// Auto-select model via router.
	if !m.modelPinned && m.router != nil {
		newModel := m.router.GetModelForCapability(authorityConfig.PreferredCapability)
		if newModel != "" && newModel != m.model {
			if m.modelManager != nil {
				m.prepareModelSwitch()
				if err := m.modelManager.SetCurrentModel(newModel); err == nil {
					m.setCurrentModelProjection(newModel)
				}
			}
		}
	}

	if m.logger != nil {
		m.logger.Info("mode switched", "mode", ambientConfig.Label, "authority", authorityConfig.Label, "model", m.model)
	}

	// Ordinary mode switches stay quiet: session shortcuts already show
	// model · mode, so a durable "notice · Mode · PLAN · …" only noise-fills
	// the transcript. A linked goal is the exception — visible authority stays
	// AUTO while ambient mode changes, so one receipt names both.
	if m.goalRuntime != nil {
		receipt := "After goal · " + ambientConfig.Label + " · active goal · AUTO"
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: receipt})
	}
	m.maybeWarnMissingApprovalTimeout(mode)
	m.refreshTranscript()
	m.resumeFollow()
	if m.overlay == OverlaySettings && m.settingsPickerState != nil {
		// Mode picker selection returns to Settings before this transition is
		// committed. Refresh again so the visible row never reports the mode we
		// just left.
		m.refreshSettingsPicker()
	}
}

// maybeWarnMissingApprovalTimeout reminds once per process that AUTO without
// tools.approval_timeout can park forever on the first modal. Timeout only
// withholds permission — never grants it — so the notice is about wall-budget
// waste, not safety. It uses the footer so Shift+Tab does not noise-fill the
// transcript (mode already lives on the shortcuts row).
func (m *Model) maybeWarnMissingApprovalTimeout(mode Mode) {
	if m == nil || m.approvalTimeoutWarned || mode != ModeAuto {
		return
	}
	if m.agent != nil && m.agent.ApprovalWaitTimeout() > 0 {
		return
	}
	m.approvalTimeoutWarned = true
	m.footerNotice = &footerNotice{
		text: "No tools.approval_timeout set — first unanswered approval waits forever. Set approval_timeout: 2m for unattended runs.",
		severity: noticeWarning,
		expiresAt: m.nowTime().Add(12 * time.Second),
	}
}
