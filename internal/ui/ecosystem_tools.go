package ui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const compactToolFailureWidth = 160

// ecosystemToolSummary returns a compact allowlisted description of a
// specialist call. It deliberately ignores arbitrary argument values: MCP
// payloads can contain source, credentials, or large manifests that do not
// belong in a collapsed receipt.
func ecosystemToolSummary(name string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	key := remoteToolKey(name)
	if key == "mcphub_call_tool" {
		return gatewayToolSummary(args)
	}
	identity := ecosystemIdentityFromTool(name)
	if identity == "" {
		return ""
	}
	return ecosystemArgumentAnchor(key, args)
}

func gatewayToolSummary(args map[string]any) string {
	server := stringArgument(args, "server")
	tool := stringArgument(args, "tool")
	if server == "" {
		if before, after, ok := strings.Cut(tool, "__"); ok {
			server, tool = before, after
		}
	} else {
		tool = strings.TrimPrefix(tool, server+"__")
	}

	parts := make([]string, 0, 3)
	if server != "" {
		parts = append(parts, describeEcosystemServer(server).label)
	}
	if tool != "" {
		parts = append(parts, friendlyRemoteAction(tool))
	}
	if len(parts) == 0 {
		return ""
	}
	return truncateDisplay(strings.Join(parts, " · "), maxToolCardSummaryWidth)
}

func ecosystemIdentityFromTool(name string) string {
	canonical := canonicalToolKey(name)
	for _, part := range strings.Split(canonical, "__") {
		if identity := ecosystemIdentity(part); identity != "" {
			return identity
		}
		for identity := range ecosystemDescriptors {
			if strings.HasPrefix(part, identity+"_") {
				return identity
			}
		}
	}
	return ""
}

func remoteToolKey(name string) string {
	key := canonicalToolKey(name)
	if index := strings.LastIndex(key, "__"); index >= 0 {
		return key[index+2:]
	}
	return key
}

func friendlyRemoteAction(tool string) string {
	action := remoteToolKey(tool)
	for identity := range ecosystemDescriptors {
		action = strings.TrimPrefix(action, identity+"_")
	}
	if action == "" {
		return "tool"
	}
	return strings.ToLower(humanizeToolIdentifier(action))
}

func ecosystemArgumentAnchor(tool string, args map[string]any) string {
	switch tool {
	case "bob_validate_manifest":
		if stringArgument(args, "manifest_yaml") != "" {
			return "inline manifest"
		}
	case "monitor_processes":
		parts := make([]string, 0, 2)
		if filter := stringArgument(args, "filter"); filter != "" {
			parts = append(parts, "filter "+quoteCompact(filter, 32))
		}
		if sortBy := stringArgument(args, "sort_by"); sortBy != "" {
			parts = append(parts, "by "+truncateDisplay(sortBy, 12))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " · ")
		}
	case "monitor_analyze":
		parts := make([]string, 0, 2)
		if pid, ok := numericArgument(args, "pid"); ok && pid > 0 {
			parts = append(parts, fmt.Sprintf("PID %d", pid))
		}
		if seconds, ok := numericArgument(args, "window_seconds"); ok && seconds > 0 {
			parts = append(parts, fmt.Sprintf("%ds window", seconds))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " · ")
		}
	case "monitor_record":
		parts := make([]string, 0, 2)
		if pid, ok := numericArgument(args, "pid"); ok && pid > 0 {
			parts = append(parts, fmt.Sprintf("PID %d", pid))
		}
		if seconds, ok := numericArgument(args, "duration_seconds"); ok && seconds > 0 {
			parts = append(parts, fmt.Sprintf("%ds", seconds))
		}
		if len(parts) > 0 {
			return strings.Join(parts, " · ")
		}
	case "monitor_kill", "monitor_profile_capture", "monitor_investigate":
		if pid, ok := numericArgument(args, "pid"); ok && pid > 0 {
			return fmt.Sprintf("PID %d", pid)
		}
	case "mcphub_get_result":
		if callID := stringArgument(args, "callId", "call_id"); callID != "" {
			return "result " + truncateDisplay(callID, 36)
		}
	}

	if taskID := stringArgument(args, "taskId", "task_id", "taskID"); taskID != "" {
		return "task " + truncateDisplay(taskID, 40)
	}
	if query := stringArgument(args, "query"); query != "" {
		return quoteCompact(query, 44)
	}
	if workspace := stringArgument(args, "workspace"); workspace != "" {
		return compactWorkspacePath(workspace, 48)
	}
	if recipe := stringArgument(args, "recipe"); recipe != "" {
		return "recipe " + truncateDisplay(recipe, 32)
	}
	if ref := stringArgument(args, "ref"); ref != "" {
		return "artifact " + truncateDisplay(ref, 36)
	}
	if path := stringArgument(args, "path"); path != "" {
		return truncateDisplay(path, 48)
	}
	return ""
}

func stringArgument(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := args[key].(string); ok {
			if value = sanitizeVisibleText(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func numericArgument(args map[string]any, key string) (int64, bool) {
	switch value := args[key].(type) {
	case int:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		return int64(value), value == float64(int64(value))
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func quoteCompact(value string, width int) string {
	value = truncateDisplay(sanitizeVisibleText(value), max(1, width-2))
	return strconv.Quote(value)
}

// compactToolFailure turns transport payloads into a stable one-line receipt.
// Expanded cards still expose the bounded raw detail for debugging.
func compactToolFailure(name, result string) string {
	message := extractToolFailureMessage(result)
	lower := strings.ToLower(message + " " + result)
	var summary string
	switch {
	case strings.Contains(lower, "outcome unknown"), strings.Contains(lower, "without a receipt"):
		summary = "Outcome unknown · inspect the target before retrying"
	case strings.Contains(lower, "unknown tool"), strings.Contains(lower, "tool not found"):
		summary = "Tool unavailable · check Runtime and verify the MCP server configuration"
	case strings.Contains(lower, "connection refused"), strings.Contains(lower, "broken pipe"), strings.Contains(lower, "disconnected"):
		summary = "Connection unavailable · check Runtime; sonar will reconnect"
	case strings.Contains(lower, "deadline exceeded"), strings.Contains(lower, "timed out"), strings.Contains(lower, "timeout"):
		summary = "Tool timed out · retry once or inspect its server in Runtime"
	case strings.Contains(lower, "approval required"), strings.Contains(lower, "permission denied"):
		if message == "" {
			message = "permission denied"
		}
		summary = message + " · review the permission request before retrying"
	case message != "":
		summary = message
	default:
		summary = "(no error details) · inspect Runtime or logs before retrying"
	}
	if identity := ecosystemIdentityFromTool(name); identity != "" {
		label := ecosystemDescriptors[identity].label
		if !strings.HasPrefix(strings.ToLower(summary), strings.ToLower(label)) {
			summary = label + " · " + summary
		}
	}
	return truncateDisplay(sanitizeVisibleText(summary), compactToolFailureWidth)
}

func extractToolFailureMessage(result string) string {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return ""
	}
	var payload any
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	if decoder.Decode(&payload) == nil {
		if message := findFailureMessage(payload, 0); message != "" {
			return truncateDisplay(sanitizeVisibleText(message), compactToolFailureWidth)
		}
	}
	firstLine := trimmed
	if before, _, ok := strings.Cut(firstLine, "\n"); ok {
		firstLine = before
	}
	return truncateDisplay(sanitizeVisibleText(firstLine), compactToolFailureWidth)
}

func findFailureMessage(value any, depth int) string {
	if depth > 4 {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		for _, key := range []string{"message", "error", "reason", "detail"} {
			if nested, ok := typed[key]; ok {
				if message := findFailureMessage(nested, depth+1); message != "" {
					return message
				}
			}
		}
	}
	return ""
}
