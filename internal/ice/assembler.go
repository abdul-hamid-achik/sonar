package ice

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

// Assembler retrieves context from multiple sources and assembles it
// into a single string that fits within the token budget.
type Assembler struct {
	embedder  *Embedder
	convStore *Store
	memStore  *memory.Store
	budgetCfg BudgetConfig
	sessionID string
	projectID string
}

// Assemble retrieves relevant past conversations and memories for the query,
// ranks them, and returns a formatted context string.
func (a *Assembler) Assemble(ctx context.Context, query string) (string, error) {
	return a.assemble(ctx, query, a.budgetCfg.Calculate(0))
}

// AssembleWithPromptTokens retrieves optional context using an authoritative
// count of all prompt tokens already admitted by the host. Assemble remains as
// a compatibility path for callers that do not yet have that count.
func (a *Assembler) AssembleWithPromptTokens(ctx context.Context, query string, promptTokens int) (string, error) {
	return a.assemble(ctx, query, a.budgetCfg.CalculatePromptRemainder(promptTokens))
}

func (a *Assembler) assemble(ctx context.Context, query string, budget Budget) (string, error) {
	type convResult struct {
		chunks []ContextChunk
		err    error
	}
	type memResult struct {
		chunks []ContextChunk
	}

	convCh := make(chan convResult, 1)
	memCh := make(chan memResult, 1)

	var wg sync.WaitGroup
	wg.Add(2)

	// Retrieve past conversations via vector search.
	go func() {
		defer wg.Done()
		chunks, err := a.retrieveConversations(ctx, query, budget.Conversation)
		convCh <- convResult{chunks: chunks, err: err}
	}()

	// Retrieve memories via keyword search.
	go func() {
		defer wg.Done()
		chunks := a.retrieveMemories(query, budget.Memory)
		memCh <- memResult{chunks: chunks}
	}()

	wg.Wait()
	close(convCh)
	close(memCh)

	cr := <-convCh
	mr := <-memCh

	if cr.err != nil {
		return "", fmt.Errorf("conversation retrieval: %w", cr.err)
	}

	return formatContext(cr.chunks, mr.chunks), nil
}

// retrieveConversations embeds the query and searches the conversation store.
func (a *Assembler) retrieveConversations(ctx context.Context, query string, tokenBudget int) ([]ContextChunk, error) {
	if tokenBudget <= 0 {
		return nil, nil
	}

	queryEmb, err := a.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}

	results := a.convStore.SearchScoped(queryEmb, a.projectID, a.sessionID, 20)

	var chunks []ContextChunk
	usedTokens := 0
	for _, r := range results {
		tokens := estimateTokens(r.Entry.Content)
		if usedTokens+tokens > tokenBudget {
			continue
		}
		chunks = append(chunks, ContextChunk{
			Source:  SourceConversation,
			Content: r.Entry.Content,
			Score:   r.Score,
			Tokens:  tokens,
		})
		usedTokens += tokens
	}
	return chunks, nil
}

// retrieveMemories searches the memory store by keyword.
func (a *Assembler) retrieveMemories(query string, tokenBudget int) []ContextChunk {
	if a.memStore == nil || tokenBudget <= 0 {
		return nil
	}

	// Retrieval is best-effort context, not an answer, so an unusable store
	// contributes nothing rather than failing the turn.
	memories, err := a.memStore.Recall(query, 10)
	if err != nil {
		return nil
	}

	var chunks []ContextChunk
	usedTokens := 0
	for _, m := range memories {
		tokens := estimateTokens(m.Content)
		if usedTokens+tokens > tokenBudget {
			continue
		}
		content := m.Content
		if len(m.Tags) > 0 {
			content += " [" + strings.Join(m.Tags, ", ") + "]"
		}
		chunks = append(chunks, ContextChunk{
			Source:  SourceMemory,
			Content: content,
			Tokens:  tokens,
		})
		usedTokens += tokens
	}
	return chunks
}

// formatContext builds the final markdown context string from conversation and memory chunks.
func formatContext(convChunks, memChunks []ContextChunk) string {
	var sb strings.Builder

	if len(convChunks) > 0 {
		sb.WriteString("\n## Relevant Past Conversations\n\n")
		for _, c := range convChunks {
			sb.WriteString("- ")
			sb.WriteString(c.Content)
			sb.WriteString("\n")
		}
	}

	if len(memChunks) > 0 {
		sb.WriteString("\n## Remembered Facts\n\n")
		for _, c := range memChunks {
			sb.WriteString("- ")
			sb.WriteString(c.Content)
			sb.WriteString("\n")
		}
	}

	return sb.String()
}
