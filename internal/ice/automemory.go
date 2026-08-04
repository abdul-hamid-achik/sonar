package ice

import (
	"context"
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	"github.com/abdul-hamid-achik/sonar/internal/memory"
)

var autoMemorySystemPrompt = "Extract any important facts, user preferences, decisions, or action items from this exchange.\n" +
	"Output one item per line in the format: TYPE: content\n" +
	"Where TYPE is one of: FACT, DECISION, PREFERENCE, TODO\n" +
	"If there is nothing worth remembering, output exactly: NONE"

var autoMemoryUserTemplate = "User: %s\nAssistant: %s"

// AutoMemory detects facts and preferences from conversation exchanges
// and saves them to the memory store.
type AutoMemory struct {
	client   llm.Client
	memStore *memory.Store
}

// Detect analyzes a user/assistant exchange and saves any extracted facts.
// Skips short exchanges that are unlikely to contain memorable information.
func (am *AutoMemory) Detect(ctx context.Context, userMsg, assistantMsg string) error {
	if am.memStore == nil {
		return nil
	}

	// Quick heuristic: skip short exchanges.
	if len(userMsg) < 20 && len(assistantMsg) < 50 {
		return nil
	}

	prompt := fmt.Sprintf(autoMemoryUserTemplate, userMsg, assistantMsg)

	var response strings.Builder
	err := am.client.ChatStream(ctx, llm.ChatOptions{
		System: autoMemorySystemPrompt,
		Messages: []llm.Message{
			{Role: "user", Content: prompt},
		},
	}, func(chunk llm.StreamChunk) error {
		response.WriteString(chunk.Text)
		return nil
	})
	if err != nil {
		return fmt.Errorf("auto-memory LLM call: %w", err)
	}

	return am.parseAndSave(response.String())
}

// parseAndSave extracts TYPE: content lines and saves each as a memory.
func (am *AutoMemory) parseAndSave(response string) error {
	lines := strings.Split(strings.TrimSpace(response), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "NONE") {
			continue
		}

		// Parse "TYPE: content" format.
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}

		typeName := strings.TrimSpace(parts[0])
		content := strings.TrimSpace(parts[1])
		if content == "" {
			continue
		}

		// Validate type.
		tag := strings.ToLower(typeName)
		switch tag {
		case "fact", "decision", "preference", "todo":
			// Valid type.
		default:
			continue
		}

		if _, err := am.memStore.Save(content, []string{tag, "auto"}); err != nil {
			return fmt.Errorf("save auto-memory: %w", err)
		}
	}
	return nil
}
