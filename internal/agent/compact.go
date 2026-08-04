package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/db"
	"github.com/abdul-hamid-achik/sonar/internal/ice"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const compactThreshold = 0.75 // Trigger compaction at 75% of context window
const keepMessages = 4        // Keep the last N messages intact

const (
	conversationSummaryPrefix         = "Conversation summary:\n"
	maxConversationSummaryTokens      = 1_024
	compactionPromptBudgetNumerator   = 3
	compactionPromptBudgetDenominator = 4
	compactionSystemPrompt            = "You are a conversation summarizer. Produce a concise summary of the conversation so far, capturing all key facts, decisions, tool results, and user requests. Keep it under 500 words and within the available response budget. Output only the summary, no preamble."
)

type contextCompactionOutput interface {
	ContextCompacted()
}

// contextCompactionLifecycleOutput is an optional, presentation-only progress
// contract. Compaction performs its own provider request before the ordinary
// turn can continue, so callers need a bounded lifecycle signal rather than a
// long, unexplained gap in streaming output.
type contextCompactionLifecycleOutput interface {
	ContextCompactionStarted()
	ContextCompactionFinished()
}

// shouldCompact returns true if the prompt token count exceeds 75% of the
// currently effective context window.
func (a *Agent) shouldCompact(promptTokens int) bool {
	return shouldCompactForContext(promptTokens, a.NumCtx())
}

// shouldCompactForContext evaluates a turn against its context-window
// snapshot. Keeping the snapshot explicit prevents a model switch from
// changing compaction policy halfway through a turn.
func shouldCompactForContext(promptTokens, numCtx int) bool {
	if numCtx <= 0 || promptTokens <= 0 {
		return false
	}
	return float64(promptTokens) > float64(numCtx)*compactThreshold
}

// compact summarizes older messages into a single recap, keeping the last
// keepMessages intact. Returns true if compaction was performed.
func (a *Agent) compact(ctx context.Context, out Output) bool {
	return a.compactForContext(ctx, out, a.NumCtx())
}

// compactForContext compacts using the immutable context-window snapshot for
// the active turn.
func (a *Agent) compactForContext(ctx context.Context, out Output, numCtx int) bool {
	model := ""
	if a.llmClient != nil {
		model = a.llmClient.Model()
	}
	return a.compactForContextAndModel(ctx, out, numCtx, model)
}

func (a *Agent) compactForContextAndModel(ctx context.Context, out Output, numCtx int, expectedModel string) bool {
	return a.compactForContextAndModelWithICE(ctx, out, numCtx, expectedModel, a.ICEEngine())
}

func (a *Agent) compactForContextAndModelWithICE(ctx context.Context, out Output, numCtx int, expectedModel string, iceEngine *ice.Engine) bool {
	a.mu.RLock()
	messages := make([]llm.Message, len(a.messages))
	copy(messages, a.messages)
	a.mu.RUnlock()
	msgCount := len(messages)

	if msgCount <= keepMessages+1 {
		return false // Not enough messages to compact.
	}

	splitAt := recentConversationBoundary(messages, keepMessages)
	if splitAt <= 0 {
		return false
	}
	older := append([]llm.Message(nil), messages[:splitAt]...)
	recent := append([]llm.Message(nil), messages[splitAt:]...)
	// Transient tool results are useful to the active provider turn but must not
	// enter a durable summary. Recent history remains transient in memory; once
	// it ages into the summarized side it is replaced by its bounded receipt.
	older = SanitizeMessagesForPersistence(older)
	durableRecoveryContexts, err := collectDurableRecoveryContexts(messages)
	if err != nil {
		out.Error(fmt.Sprintf("compaction preserved full history because durable recovery context is invalid: %v", err))
		return false
	}
	// Prefix text alone is never authority. Remove every prefixed system
	// message from the ordinary transcript, then reinsert only the deduplicated
	// host-owned receipts validated above.
	older = stripDurableRecoveryPrefixMessages(older)
	recent = stripDurableRecoveryPrefixMessages(recent)
	if !hasSummarizableConversationContent(older) {
		out.Error("compaction preserved full history because no older conversation content can be summarized")
		return false
	}

	summary := summarizeMessages(older)
	summary, maxSummaryTokens, ok := boundCompactionPrompt(summary, numCtx)
	if !ok {
		out.Error("compaction preserved full history because the summarizer prompt cannot reserve generation space in the active context window")
		return false
	}

	if reporter, ok := out.(contextCompactionLifecycleOutput); ok {
		reporter.ContextCompactionStarted()
		defer reporter.ContextCompactionFinished()
	}

	// Ask LLM to produce a compact summary.
	var summaryBuf strings.Builder
	err = a.chatStreamWithResolvedImages(ctx, llm.ChatOptions{
		Messages: []llm.Message{
			{Role: "user", Content: summary, HostOwned: true},
		},
		System:          compactionSystemPrompt,
		MaxEvalTokens:   maxSummaryTokens,
		ExpectedContext: numCtx,
		ExpectedModel:   expectedModel,
	}, func(chunk llm.StreamChunk) error {
		if chunk.Text != "" {
			summaryBuf.WriteString(chunk.Text)
		}
		return nil
	})

	if err != nil {
		out.Error(fmt.Sprintf("compaction failed: %v", providerBoundaryError(err)))
		return false
	}

	summaryText := strings.TrimSpace(summaryBuf.String())
	// Providers are not trusted to honor MaxEvalTokens. Keep the durable recap
	// inside both the global projection bound and the generation reserve for this
	// exact context window.
	summaryText = boundPromptTextByEstimatedTokens(summaryText, min(maxConversationSummaryTokens, maxSummaryTokens))
	if summaryText == "" {
		return false
	}

	// ICE: persist summary for cross-session retrieval.
	if iceEngine != nil {
		reportOptionalICEError(ctx, out, "summary indexing", iceEngine.IndexSummary(ctx, summaryText))
	}

	// Snapshot the full pre-compaction history first so compaction is
	// non-destructive. If persistence was configured, replacing history without
	// its recovery checkpoint would violate that contract, so fail closed.
	if a.checkpointStore != nil {
		checkpointID, err := a.CreateCheckpoint(ctx, "before compaction", db.CheckpointPreCompaction)
		if err != nil || checkpointID == 0 {
			if err == nil {
				err = fmt.Errorf("checkpoint store returned no checkpoint id")
			}
			if a.logger != nil {
				a.logger.Warn("pre-compaction checkpoint failed", "err", err)
			}
			out.Error(fmt.Sprintf("compaction preserved full history because its recovery checkpoint failed: %v", err))
			return false
		}
	}

	// Replace messages with summary + validated durable receipts + recent.
	compacted := make([]llm.Message, 0, 1+len(durableRecoveryContexts)+len(recent))
	compacted = append(compacted, llm.Message{
		Role:      "system",
		Content:   conversationSummaryPrefix + summaryText,
		HostOwned: true,
	})
	compacted = append(compacted, durableRecoveryContexts...)
	compacted = append(compacted, recent...)
	a.ReplaceMessagesWithinSession(compacted)
	a.clearContextPromptFloor()

	if reporter, ok := out.(contextCompactionOutput); ok {
		reporter.ContextCompacted()
	}
	out.SystemMessage(fmt.Sprintf("Context compacted: %d messages summarized, %d kept", len(older), len(recent)+len(durableRecoveryContexts)))
	return true
}

// boundCompactionPrompt keeps the summarizer request beneath the same 75%
// admission threshold as an ordinary provider request. The remaining context
// is an explicit generation reserve, capped because summaries are bounded host
// projections rather than open-ended assistant turns.
func boundCompactionPrompt(summary string, numCtx int) (bounded string, maxSummaryTokens int, ok bool) {
	if numCtx <= 0 || summary == "" {
		return "", 0, false
	}
	promptLimit := numCtx * compactionPromptBudgetNumerator / compactionPromptBudgetDenominator
	fixedTokens := estimateTextPromptTokens(compactionSystemPrompt) + 4 // one message frame
	inputBudget := promptLimit - fixedTokens
	if inputBudget <= 0 {
		return "", 0, false
	}
	bounded = boundPromptTextByEstimatedTokens(summary, inputBudget)
	if bounded == "" || estimateTextPromptTokens(bounded)+fixedTokens > promptLimit {
		return "", 0, false
	}
	maxSummaryTokens = numCtx - promptLimit
	if maxSummaryTokens > maxConversationSummaryTokens {
		maxSummaryTokens = maxConversationSummaryTokens
	}
	if maxSummaryTokens <= 0 {
		return "", 0, false
	}
	return bounded, maxSummaryTokens, true
}

func boundPromptTextByEstimatedTokens(text string, maxTokens int) string {
	if text == "" || maxTokens <= 0 {
		return ""
	}
	if estimateTextPromptTokens(text) <= maxTokens {
		return text
	}
	runes := []rune(text)
	low, high := 1, len(runes)
	best := ""
	for low <= high {
		mid := low + (high-low)/2
		candidate := boundPromptText(text, mid)
		if estimateTextPromptTokens(candidate) <= maxTokens {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best
}

func collectDurableRecoveryContexts(messages []llm.Message) ([]llm.Message, error) {
	contexts := make([]llm.Message, 0)
	seen := make(map[string]struct{})
	aggregateBytes := 0
	for _, message := range messages {
		if message.Role != "system" || !strings.HasPrefix(message.Content, DurableRecoveryContextPrefix) || !message.HostOwned {
			continue
		}
		if !utf8.ValidString(message.Content) || len(message.Content) == 0 || len(message.Content) > MaxDurableRecoveryContextMessageBytes {
			return nil, fmt.Errorf("receipt exceeds the %d-byte UTF-8 bound", MaxDurableRecoveryContextMessageBytes)
		}
		if _, exists := seen[message.Content]; exists {
			continue
		}
		seen[message.Content] = struct{}{}
		if len(seen) > MaxDurableRecoveryContextMessages {
			return nil, fmt.Errorf("receipt count exceeds %d", MaxDurableRecoveryContextMessages)
		}
		aggregateBytes += len(message.Content)
		if aggregateBytes > MaxDurableRecoveryContextAggregateBytes {
			return nil, fmt.Errorf("receipt content exceeds %d aggregate bytes", MaxDurableRecoveryContextAggregateBytes)
		}
		contexts = append(contexts, llm.Message{Role: "system", Content: message.Content, HostOwned: true})
	}
	return contexts, nil
}

func stripDurableRecoveryPrefixMessages(messages []llm.Message) []llm.Message {
	filtered := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, DurableRecoveryContextPrefix) {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered
}

// hasSummarizableConversationContent prevents a recovery-only prefix from
// becoming a model-authored conversation recap. Durable recovery receipts are
// reinserted separately after compaction, so once they are stripped there must
// be actual conversation state (or a prior host-owned recap) to summarize.
func hasSummarizableConversationContent(messages []llm.Message) bool {
	for _, message := range messages {
		switch message.Role {
		case "system":
			if message.HostOwned && strings.HasPrefix(message.Content, conversationSummaryPrefix) &&
				strings.TrimSpace(strings.TrimPrefix(message.Content, conversationSummaryPrefix)) != "" {
				return true
			}
		case "user":
			if strings.TrimSpace(message.Content) != "" || len(message.Images) > 0 {
				return true
			}
		case "assistant":
			if strings.TrimSpace(message.Content) != "" || len(message.ToolCalls) > 0 {
				return true
			}
		case "tool":
			// Even an empty tool result carries a call identity and outcome in the
			// conversation sequence, so it is semantic input to the recap.
			return true
		}
	}
	return false
}

// recentConversationBoundary chooses a user-turn boundary so an assistant
// tool call is never separated from its tool result. Prefer the next user turn
// after the raw keep-N split; otherwise retain the current turn in full.
func recentConversationBoundary(messages []llm.Message, keep int) int {
	if keep <= 0 || len(messages) <= keep {
		return 0
	}
	tentative := len(messages) - keep
	if messages[tentative].Role == "user" {
		return tentative
	}
	for i := tentative + 1; i < len(messages); i++ {
		if messages[i].Role == "user" {
			return i
		}
	}
	for i := tentative - 1; i > 0; i-- {
		if messages[i].Role == "user" {
			return i
		}
	}
	return 0
}

// summarizeMessages formats a slice of messages into a human-readable transcript
// for the summarization LLM call.
func summarizeMessages(msgs []llm.Message) string {
	var b strings.Builder
	b.WriteString("Summarize this conversation:\n\n")

	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			if !msg.HostOwned || !strings.HasPrefix(msg.Content, conversationSummaryPrefix) {
				continue
			}
			recap := strings.TrimSpace(strings.TrimPrefix(msg.Content, conversationSummaryPrefix))
			if recap != "" {
				fmt.Fprintf(&b, "Previous conversation summary: %s\n", boundPromptTextByEstimatedTokens(recap, maxConversationSummaryTokens))
			}
		case "user":
			fmt.Fprintf(&b, "User: %s\n", msg.Content)
			for _, image := range msg.Images {
				fmt.Fprintf(&b, "User attached image %s (%s, %dx%d).\n", image.Name, image.MediaType, image.Width, image.Height)
			}
		case "assistant":
			if msg.Content != "" {
				fmt.Fprintf(&b, "Assistant: %s\n", msg.Content)
			}
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&b, "Assistant called tool %s(%s)\n", tc.Name, FormatToolArgsForTool(tc.Name, tc.Arguments))
			}
		case "tool":
			content := msg.Content
			if len(content) > 300 {
				content = content[:297] + "..."
			}
			fmt.Fprintf(&b, "Tool %s result: %s\n", msg.ToolName, content)
		}
	}

	return b.String()
}
