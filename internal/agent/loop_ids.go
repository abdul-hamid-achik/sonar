package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// ensureToolCallIDs gives every invocation a stable, unique correlation key.
// Small local models occasionally omit IDs (or repeat one for a batch), while
// the UI and provider transcript need an unambiguous result association.
func ensureToolCallIDs(calls []llm.ToolCall, turnID string, iteration int) {
	ensureToolCallIDsAgainst(calls, turnID, iteration, nil)
}

func (a *Agent) ensureToolCallIDs(calls []llm.ToolCall, turnID string, iteration int) {
	reserved := make(map[string]struct{})
	a.mu.RLock()
	for _, message := range a.messages {
		if id := strings.TrimSpace(message.ToolCallID); id != "" {
			reserved[id] = struct{}{}
		}
		for _, call := range message.ToolCalls {
			if id := strings.TrimSpace(call.ID); id != "" {
				reserved[id] = struct{}{}
			}
		}
	}
	a.mu.RUnlock()
	ensureToolCallIDsAgainst(calls, turnID, iteration, reserved)
}

func ensureToolCallIDsAgainst(calls []llm.ToolCall, turnID string, iteration int, reserved map[string]struct{}) {
	seen := make(map[string]struct{}, len(reserved)+len(calls))
	for id := range reserved {
		seen[id] = struct{}{}
	}
	for i := range calls {
		id := strings.TrimSpace(calls[i].ID)
		_, duplicate := seen[id]
		if id == "" || duplicate {
			base := fmt.Sprintf("%s-tool-%d-%d", turnID, iteration, i+1)
			id = base
			for suffix := 2; ; suffix++ {
				if _, exists := seen[id]; !exists {
					break
				}
				id = fmt.Sprintf("%s-%d", base, suffix)
			}
		}
		calls[i].ID = id
		seen[id] = struct{}{}
	}
}

// FormatToolArgs formats tool arguments in a human-readable way for display.
// Avoids showing raw JSON by presenting key=value pairs.
func FormatToolArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	var parts []string
	for key, value := range args {
		// Format value based on its type
		var valStr string
		switch v := value.(type) {
		case string:
			// Truncate long strings (account for quotes)
			if len(v) > 47 {
				valStr = `"` + v[:44] + `..."`
			} else {
				valStr = `"` + v + `"`
			}
		case int, float64, bool:
			valStr = fmt.Sprintf("%v", v)
		case []any:
			// Show array length
			valStr = fmt.Sprintf("[%d items]", len(v))
		case map[string]any:
			// Show object keys count
			valStr = fmt.Sprintf("{%d fields}", len(v))
		default:
			valStr = fmt.Sprintf("%v", v)
		}
		parts = append(parts, fmt.Sprintf("%s=%s", key, valStr))
	}

	// Sort parts for consistent output
	sort.Strings(parts)

	result := strings.Join(parts, " ")

	// Truncate if too long
	if len(result) > 60 {
		return result[:57] + "..."
	}
	return result
}
