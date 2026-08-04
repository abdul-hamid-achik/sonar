package ui

import (
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

// ApprovalPosture is the process-wide approval contract selected by the host.
// It is presentation state only: the permission checker remains the execution
// authority, while the TUI makes that authority visible without inferring it
// from conversational mode.
type ApprovalPosture string

const (
	ApprovalPosturePrompted             ApprovalPosture = "prompted"
	ApprovalPostureSkipApprovals        ApprovalPosture = "skip_approvals"
	ApprovalPostureAcceptWorkspaceEdits ApprovalPosture = "accept_workspace_edits"
	// ApprovalPostureYolo is retained for source compatibility.
	// Deprecated: use ApprovalPostureSkipApprovals.
	ApprovalPostureYolo = ApprovalPostureSkipApprovals
)

func (p ApprovalPosture) valid() bool {
	switch p {
	case ApprovalPosturePrompted, ApprovalPostureSkipApprovals, ApprovalPostureAcceptWorkspaceEdits:
		return true
	default:
		return false
	}
}

// SetApprovalPosture projects the host's actual permission posture into the
// TUI. Invalid values fail closed to the ordinary prompted posture. When an
// agent is attached, AcceptWorkspaceEdits is mirrored onto the agent so
// NORMAL write/edit/mkdir auto-approval stays process-local and consistent.
func (m *Model) SetApprovalPosture(posture ApprovalPosture) {
	if m == nil {
		return
	}
	if !posture.valid() {
		posture = ApprovalPosturePrompted
	}
	m.approvalPosture = posture
	if m.agent != nil {
		switch posture {
		case ApprovalPostureAcceptWorkspaceEdits:
			m.agent.SetApprovalPosture(agent.ApprovalPostureAcceptWorkspaceEdits)
		case ApprovalPostureSkipApprovals:
			// Host skip-approvals is owned by the permission checker, not the
			// agent posture field. Keep agent posture prompted so turning skip
			// off later does not leave accept-edits sticky.
			m.agent.SetApprovalPosture(agent.ApprovalPosturePrompted)
		default:
			m.agent.SetApprovalPosture(agent.ApprovalPosturePrompted)
		}
	}
	if m.settingsPickerState != nil {
		m.refreshSettingsPicker()
	}
	if m.runtimeStatusState != nil {
		m.refreshRuntimeStatus(true)
	}
}

func (m *Model) skipApprovalsEnabled() bool {
	return m != nil && m.approvalPosture == ApprovalPostureSkipApprovals
}

func (m *Model) acceptWorkspaceEditsEnabled() bool {
	return m != nil && m.approvalPosture == ApprovalPostureAcceptWorkspaceEdits
}

func (m *Model) approvalPostureRuntimeLabel() string {
	base := "Prompted for approval-gated tools"
	switch {
	case m.skipApprovalsEnabled():
		base = "Approval prompts skipped · host/tool boundaries apply"
	case m.acceptWorkspaceEditsEnabled():
		base = "Accept workspace edits · write/edit/mkdir auto in workspace"
	}
	if m == nil || m.agent == nil {
		return base
	}
	session := len(m.agent.ListSessionApprovalSummary())
	rules := m.agent.WorkspaceRulesSnapshot()
	workspaceRules := len(rules.BashPrefixes) + len(rules.MCPTools) + len(rules.WritePaths)
	switch {
	case session == 0 && workspaceRules == 0:
		return base
	case workspaceRules == 0:
		return fmt.Sprintf("%s · %d session grant%s", base, session, pluralSuffix(session))
	case session == 0:
		return fmt.Sprintf("%s · %d workspace rule%s", base, workspaceRules, pluralSuffix(workspaceRules))
	default:
		return fmt.Sprintf("%s · %d session · %d workspace", base, session, workspaceRules)
	}
}

func (m *Model) approvalPostureWelcomeLabel(compact bool) string {
	switch {
	case m.skipApprovalsEnabled():
		if compact {
			return "approval prompts skipped"
		}
		return "Approval prompts skipped · host/tool boundaries apply"
	case m.acceptWorkspaceEditsEnabled():
		if compact {
			return "accept workspace edits"
		}
		return "Accept workspace edits · write/edit/mkdir auto in workspace"
	case compact:
		return "approval prompts on"
	default:
		return "approval prompts enabled"
	}
}

func (m *Model) approvalPostureWelcomeMicroLabel() string {
	switch {
	case m.skipApprovalsEnabled():
		return "prompts skipped"
	case m.acceptWorkspaceEditsEnabled():
		return "accept edits"
	default:
		return "prompts on"
	}
}
