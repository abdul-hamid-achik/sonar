package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

func TestExecutionRecoveryNoticeIsActionableAndDeduplicated(t *testing.T) {
	unresolved := &agent.UnresolvedExecutionError{
		SessionID: 17, ExecutionID: "exec_timeout", ToolName: "bash",
		EventType: execution.EventOutcomeUnknown,
		Cause:     errors.New("durable outcome is unknown and requires explicit reconciliation"),
	}
	entries, appended := appendExecutionRecoveryNotice(nil, unresolved)
	if !appended || len(entries) != 1 {
		t.Fatalf("first notice = %#v appended=%v", entries, appended)
	}
	for _, want := range []string{
		"sonar execution recover 17 exec_timeout",
		"read-only", "next prompt rechecks", "/new starts a separate session",
	} {
		if !strings.Contains(entries[0].Content, want) {
			t.Fatalf("notice missing %q: %s", want, entries[0].Content)
		}
	}
	entries = append(entries, ChatEntry{Kind: "user", Content: "can you continue?"})
	entries, appended = appendExecutionRecoveryNotice(entries, unresolved)
	if appended || len(entries) != 2 {
		t.Fatalf("duplicate notice appended: %#v appended=%v", entries, appended)
	}
}

func TestExecutionRecoveryNoticeReportsReconciliationBacklog(t *testing.T) {
	single := executionRecoveryNotice(&agent.UnresolvedExecutionError{
		SessionID: 17, ExecutionID: "exec_only", ToolName: "bash",
		EventType: execution.EventOutcomeUnknown, PendingReconciliations: 1,
	})
	if strings.Contains(single, "--all") {
		t.Fatalf("single pending execution advertised batch recovery: %q", single)
	}
	backlog := executionRecoveryNotice(&agent.UnresolvedExecutionError{
		SessionID: 17, ExecutionID: "exec_oldest", ToolName: "bash",
		EventType: execution.EventOutcomeUnknown, PendingReconciliations: 3,
	})
	for _, want := range []string{
		"3 executions are pending reconciliation",
		"this is the oldest",
		"sonar execution recover 17 --all",
	} {
		if !strings.Contains(backlog, want) {
			t.Fatalf("backlog notice missing %q: %s", want, backlog)
		}
	}
}

func TestExecutionRecoveryNoticeDistinguishesMissingAndUncertainReceipts(t *testing.T) {
	unknown := executionRecoveryNotice(&agent.UnresolvedExecutionError{
		SessionID: 2, ExecutionID: "exec_unknown", ToolName: "cortex_investigate",
		EventType: execution.EventOutcomeUnknown,
	})
	if !strings.Contains(unknown, "cannot verify whether its effect happened") || strings.Contains(unknown, "receipt is missing") {
		t.Fatalf("unknown-outcome copy = %q", unknown)
	}

	started := executionRecoveryNotice(&agent.UnresolvedExecutionError{
		SessionID: 2, ExecutionID: "exec_started", ToolName: "bash",
		EventType: execution.EventStarted,
	})
	if !strings.Contains(started, "terminal receipt is missing") || strings.Contains(started, "cannot verify whether its effect happened") {
		t.Fatalf("started-event copy = %q", started)
	}
}

func TestExecutionRecoveryNoticeDoesNotOfferEffectReconciliationForProjectionHazard(t *testing.T) {
	notice := executionRecoveryNotice(&agent.UnresolvedExecutionError{
		SessionID: 17, ExecutionID: "exec_completed", ToolName: "write",
		EventType: execution.EventCompleted,
	})
	if strings.Contains(notice, "execution recover") || !strings.Contains(notice, "projection repair") {
		t.Fatalf("projection notice = %q", notice)
	}
}

func TestAgentDoneDeduplicatesSameExecutionBlockerAcrossPrompts(t *testing.T) {
	m := newTestModel(t)
	unresolved := &agent.UnresolvedExecutionError{
		SessionID: 17, WorkspaceID: "/workspace", ExecutionID: "exec_timeout", ToolName: "bash",
		EventType: execution.EventOutcomeUnknown,
		Cause:     errors.New("durable outcome is unknown and requires explicit reconciliation"),
	}
	updated, _ := m.Update(AgentDoneMsg{Err: unresolved})
	m = updated.(*Model)
	m.entries = append(m.entries, ChatEntry{Kind: "user", Content: "continue?"})
	m.state = StateStreaming
	updated, _ = m.Update(AgentDoneMsg{Err: unresolved})
	m = updated.(*Model)

	count := 0
	for _, entry := range m.entries {
		if entry.Kind == "error" && strings.Contains(entry.Content, "sonar execution recover 17 exec_timeout") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("same blocker rendered %d times: %#v", count, m.entries)
	}
}

func TestStandaloneRecoveryIsNotAdmittedWhenGoalAuthorityExists(t *testing.T) {
	m := newTestModel(t)
	m.goalRuntime = &goal.Runtime{}
	m.rememberStandaloneRecovery(&agent.UnresolvedExecutionError{
		SessionID: 17, ExecutionID: "exec_goal", ToolName: "write",
		EventType: execution.EventOutcomeUnknown,
	})
	if m.standaloneRecovery != nil {
		t.Fatalf("goal-owned execution admitted to standalone recovery: %#v", m.standaloneRecovery)
	}
}
