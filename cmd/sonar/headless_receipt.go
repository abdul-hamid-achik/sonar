package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/db"
	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
)

// maxReceiptActorBytes bounds the free-text actor label. It is a correlation
// value for the receipt, not an authority input, so shape is the only rule.
const maxReceiptActorBytes = 256

// maxReceiptErrorBytes bounds the receipt's error field so a pathological
// provider or tool error cannot turn the receipt into a log file.
const maxReceiptErrorBytes = 2048

// headlessTurnOutput is the output shape both headless sinks satisfy: the
// plain text HeadlessOutput and the --json receipt collector.
type headlessTurnOutput interface {
	agent.Output
	GoalTurnStats() (summary string, evalTokens int64, productive bool)
	TokenUsage() (promptTokens, evalTokens int64)
}

// recordHeadlessTokenUsage persists the settled turn's provider accounting
// into the same token_stats projection the TUI maintains, numbering the turn
// after the rows the session already has.
func recordHeadlessTokenUsage(ctx context.Context, store *db.Store, sessionID int64, model string, promptTokens, evalTokens int64) error {
	if promptTokens <= 0 && evalTokens <= 0 {
		return nil
	}
	existing, err := store.GetSessionTokenStats(ctx, sessionID)
	if err != nil {
		return err
	}
	_, err = store.RecordTokenUsage(ctx, db.RecordTokenUsageParams{
		SessionID:    sessionID,
		Turn:         int64(len(existing)) + 1,
		EvalCount:    evalTokens,
		PromptTokens: promptTokens,
		Model:        model,
	})
	return err
}

// resolveExternalTurnIdentity merges identity flags with their environment
// fallbacks (SONAR_RUN_ID, SONAR_TURN_ID, SONAR_ACTOR) and
// validates shape early so a supervisor gets a fast exit-2 instead of a failed
// turn. The values are correlation labels; they never grant authority.
func resolveExternalTurnIdentity(options rootOptions) (runID, turnID, actor string, err error) {
	runID = strings.TrimSpace(options.runID)
	if runID == "" {
		runID = strings.TrimSpace(os.Getenv("SONAR_RUN_ID"))
	}
	turnID = strings.TrimSpace(options.turnID)
	if turnID == "" {
		turnID = strings.TrimSpace(os.Getenv("SONAR_TURN_ID"))
	}
	actor = strings.TrimSpace(options.actor)
	if actor == "" {
		actor = strings.TrimSpace(os.Getenv("SONAR_ACTOR"))
	}
	if len(runID) > executionpkg.MaxRunIDBytes || !utf8.ValidString(runID) {
		return "", "", "", fmt.Errorf("run id must be valid UTF-8 of at most %d bytes", executionpkg.MaxRunIDBytes)
	}
	if len(turnID) > executionpkg.MaxTurnIDBytes || !utf8.ValidString(turnID) {
		return "", "", "", fmt.Errorf("turn id must be valid UTF-8 of at most %d bytes", executionpkg.MaxTurnIDBytes)
	}
	if len(actor) > maxReceiptActorBytes || !utf8.ValidString(actor) {
		return "", "", "", fmt.Errorf("actor must be valid UTF-8 of at most %d bytes", maxReceiptActorBytes)
	}
	return runID, turnID, actor, nil
}

// headlessPendingRecoveryCount reports how many execution effects past the
// session's snapshot cursor still lack a terminal receipt. The receipt carries
// the count; the execution ledger remains the authority on the effects.
func headlessPendingRecoveryCount(ctx context.Context, store *db.Store, sessionID int64, workspaceID string, sinceCursor int64) (int, error) {
	hazards, err := store.ListExecutionRecoveryHazards(ctx, sessionID, workspaceID, sinceCursor, 100)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, state := range hazards {
		if !state.Terminal() {
			count++
		}
	}
	return count, nil
}

// boundedReceiptError caps the receipt's error text on a rune boundary.
func boundedReceiptError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	if len(value) <= maxReceiptErrorBytes {
		return value
	}
	cut := maxReceiptErrorBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
