package ice

// BudgetConfig controls how the context window is divided among sources.
type BudgetConfig struct {
	NumCtx          int
	SystemReserve   int     // tokens reserved for system prompt
	RecentReserve   int     // tokens reserved for recent conversation
	ConversationPct float64 // fraction of remaining budget for past conversations
	MemoryPct       float64 // fraction of remaining budget for memories
}

// DefaultBudgetConfig returns sensible defaults for a given context window.
func DefaultBudgetConfig(numCtx int) BudgetConfig {
	return BudgetConfig{
		NumCtx:          numCtx,
		SystemReserve:   1500,
		RecentReserve:   2000,
		ConversationPct: 0.65,
		MemoryPct:       0.35,
	}
}

// Calculate allocates token budgets given how many tokens the current prompt uses.
func (bc BudgetConfig) Calculate(promptTokens int) Budget {
	// Use 75% of numCtx as total available.
	available := int(float64(bc.NumCtx) * 0.75)
	available -= bc.SystemReserve
	available -= bc.RecentReserve
	if promptTokens < 0 {
		promptTokens = 0
	}
	available -= promptTokens

	return bc.allocate(available)
}

// CalculatePromptRemainder allocates ICE's optional context from an
// authoritative count of the prompt that has already been admitted. Unlike
// Calculate, promptTokens is expected to include the system prompt, tool
// schemas, and recent/history messages, so their configured reserves are not
// subtracted a second time.
func (bc BudgetConfig) CalculatePromptRemainder(promptTokens int) Budget {
	if promptTokens < 0 {
		promptTokens = 0
	}
	available := int(float64(bc.NumCtx)*0.75) - promptTokens
	return bc.allocate(available)
}

func (bc BudgetConfig) allocate(available int) Budget {
	if available < 0 {
		available = 0
	}

	return Budget{
		Total:        available,
		System:       bc.SystemReserve,
		Recent:       bc.RecentReserve,
		Conversation: int(float64(available) * bc.ConversationPct),
		Memory:       int(float64(available) * bc.MemoryPct),
	}
}

// estimateTokens returns a rough token count for a string (chars / 4).
func estimateTokens(s string) int {
	n := len(s) / 4
	if n == 0 && len(s) > 0 {
		n = 1
	}
	return n
}
