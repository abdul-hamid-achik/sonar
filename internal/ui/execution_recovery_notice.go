package ui

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/execution"
)

func executionRecoveryNotice(unresolved *agent.UnresolvedExecutionError) string {
	if unresolved == nil {
		return ""
	}
	detail := "The execution state requires reconciliation."
	switch unresolved.EventType {
	case execution.EventOutcomeUnknown:
		detail = "Dispatch occurred, but the host cannot verify whether its effect happened."
	case execution.EventStarted:
		detail = "Dispatch started, but its terminal receipt is missing."
	}
	if command := unresolved.RecoveryInspectCommand(); command != "" {
		backlog := ""
		if unresolved.PendingReconciliations > 1 {
			backlog = fmt.Sprintf(
				"\n%d executions are pending reconciliation in this session; this is the oldest. List them all (read-only): sonar execution recover %d --all",
				unresolved.PendingReconciliations, unresolved.SessionID,
			)
		}
		return fmt.Sprintf(
			"Recovery paused · %s\n%s Automatic retry is disabled.\n\nRun /recover to inspect the exact execution and record typed evidence. Your draft stays in the composer.\nExecution %s · %s\nCLI (read-only): %s%s\n\nNo tool is retried; after evidence commits, the next prompt rechecks durable state. /new starts a separate session and does not reconcile this execution.",
			unresolved.ToolName, detail, unresolved.ExecutionID, unresolved.EventType, command, backlog,
		)
	}
	return fmt.Sprintf(
		"Recovery paused · %s\n%s Automatic retry is disabled. This state needs session projection repair: the effect is recorded in the durable ledger but is newer than the saved transcript, so /recover cannot reconcile it.\nCLI (close this session first): sonar session repair %d\n/new starts a separate session without reconciling it.",
		unresolved.ToolName, detail, unresolved.SessionID,
	)
}

func appendExecutionRecoveryNotice(entries []ChatEntry, unresolved *agent.UnresolvedExecutionError) ([]ChatEntry, bool) {
	notice := executionRecoveryNotice(unresolved)
	if notice == "" {
		return entries, false
	}
	command := unresolved.RecoveryInspectCommand()
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Kind != "error" {
			continue
		}
		if entry.Content == notice || (command != "" && strings.Contains(entry.Content, command)) {
			return entries, false
		}
	}
	return append(entries, ChatEntry{Kind: "error", Content: notice}), true
}

// settledExecutionProjectionNotice replaces a projection-repair instruction
// once the effect it names is provably in the saved snapshot.
func settledExecutionProjectionNotice(unresolved *agent.UnresolvedExecutionError) string {
	if unresolved == nil {
		return ""
	}
	return fmt.Sprintf(
		"Recovery resolved · %s\nThe %s effect is now projected into the saved session: settlement confirmed its receipt in the transcript and advanced the snapshot cursor past it. No repair is needed and the next prompt continues normally.\nExecution %s",
		unresolved.ToolName, unresolved.EventType, unresolved.ExecutionID,
	)
}

// downgradeExecutionRecoveryNotice withdraws a projection-repair notice that
// the same settlement pass has already made moot. `sonar session repair` would
// answer "already current" for that state, so leaving the instruction standing
// tells the user to run a no-op while their session looks blocked.
//
// The stale error is removed rather than rewritten in place: transcript block
// lifecycles are monotonic and a failed block may not become a settled one.
func downgradeExecutionRecoveryNotice(
	entries []ChatEntry,
	unresolved *agent.UnresolvedExecutionError,
) ([]ChatEntry, bool) {
	notice := executionRecoveryNotice(unresolved)
	settled := settledExecutionProjectionNotice(unresolved)
	if notice == "" || settled == "" {
		return entries, false
	}
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].Kind != "error" || entries[index].Content != notice {
			continue
		}
		entries = append(entries[:index], entries[index+1:]...)
		return append(entries, ChatEntry{Kind: "system", Content: settled}), true
	}
	return entries, false
}
