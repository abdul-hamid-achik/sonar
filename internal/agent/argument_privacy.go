package agent

import (
	"strings"
	"unicode"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const hiddenToolArguments = "details hidden"

// ToolArgumentsRequirePrivacy reports whether a tool's argument payload must
// stay behind the agent boundary. Every namespaced tool is MCP-provided; its
// arbitrary values must not be copied into the transcript or a saved session.
// Security-tool aliases remain private even when a host exposes one without a
// namespace.
func ToolArgumentsRequirePrivacy(name string) bool {
	key := canonicalPrivacyKey(name)
	return strings.Contains(name, "__") ||
		strings.Contains(key, "tinyvault") || strings.Contains(key, "tvault")
}

// FormatToolArgsForTool formats only arguments that are safe for ambient UI
// and headless diagnostics. MCPHub retains its bounded route identifiers, but
// never the nested downstream arguments. Other MCP and security-tool payloads
// are intentionally opaque.
func FormatToolArgsForTool(name string, args map[string]any) string {
	if ToolArgumentsRequirePrivacy(name) {
		projection := ecosystem.ProjectToolCall(name, args)
		if projection.Route.Gateway == "mcphub" || projection.Specialist == "mcphub" {
			if safe := safeGatewayRouteArgs(projection); len(safe) > 0 {
				return FormatToolArgs(safe)
			}
		}
		return hiddenToolArguments
	}
	return FormatToolArgs(redactSensitiveArguments(args))
}

// SafeToolArgsForPersistence returns a deep copy suitable for UI state and
// session history. The returned map never aliases the provider-owned payload.
// MCP payloads retain only routing identity; a marker records that nested
// arguments existed without persisting their shape or values.
func SafeToolArgsForPersistence(name string, args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	if ToolArgumentsRequirePrivacy(name) {
		projection := ecosystem.ProjectToolCall(name, args)
		if projection.Route.Gateway != "mcphub" && projection.Specialist != "mcphub" {
			return map[string]any{"redacted": true}
		}
		safe := safeGatewayRouteArgs(projection)
		if _, exists := args["arguments"]; exists {
			safe["arguments"] = map[string]any{"redacted": true}
		}
		if len(safe) == 0 {
			return map[string]any{"redacted": true}
		}
		return safe
	}
	return redactSensitiveArguments(args)
}

// SanitizeMessagesForPersistence clones model history, replaces transient tool
// content with its bounded durable receipt, and removes sensitive tool-call
// arguments before history crosses a durable session boundary.
func SanitizeMessagesForPersistence(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	result := make([]llm.Message, len(messages))
	for index, message := range messages {
		result[index] = message
		// Image bytes are provider-only inputs. Retain a fresh slice of complete,
		// content-addressed metadata so a restored session can rehydrate it, but
		// never let a transient or internally inconsistent reference cross the
		// durable boundary.
		if message.Role == "user" {
			result[index].Images = durableImageReferences(message.Images)
		} else {
			result[index].Images = nil
		}
		if message.DurableContent != "" {
			result[index].Content = message.DurableContent
		}
		result[index].DurableContent = ""
		if len(message.ToolCalls) == 0 {
			continue
		}
		result[index].ToolCalls = make([]llm.ToolCall, len(message.ToolCalls))
		for callIndex, call := range message.ToolCalls {
			result[index].ToolCalls[callIndex] = call
			result[index].ToolCalls[callIndex].Arguments = SafeToolArgsForPersistence(call.Name, call.Arguments)
		}
	}
	return result
}

func durableImageReferences(images []llm.ImageData) []llm.ImageData {
	if len(images) == 0 {
		return nil
	}
	result := make([]llm.ImageData, 0, len(images))
	for _, image := range images {
		if err := image.ValidateReference(); err != nil {
			continue
		}
		if len(image.Data) > 0 {
			if err := image.Validate(); err != nil {
				continue
			}
		}
		image.Data = nil
		result = append(result, image)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func safeGatewayRouteArgs(projection ecosystem.ToolProjection) map[string]any {
	safe := make(map[string]any, 3)
	if projection.Route.Server != "" && projection.Route.Server != "mcphub" {
		safe["server"] = projection.Route.Server
	}
	if projection.Route.Tool != "" && projection.Route.Tool != "mcphub_call_tool" && projection.Route.Tool != "mcphub_get_result" {
		safe["tool"] = projection.Route.Tool
	}
	if projection.Route.CallID != "" {
		safe["call_id"] = projection.Route.CallID
	}
	return safe
}

func redactSensitiveArguments(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	result := make(map[string]any, len(args))
	for key, value := range args {
		if sensitiveArgumentKey(key) {
			result[key] = "[hidden]"
			continue
		}
		result[key] = cloneSafeArgumentValue(value)
	}
	return result
}

func cloneSafeArgumentValue(value any) any {
	switch typed := value.(type) {
	case nil, string, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return typed
	case map[string]any:
		return redactSensitiveArguments(typed)
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = cloneSafeArgumentValue(item)
		}
		return result
	default:
		// Tool arguments are JSON-shaped. Refuse Stringer or custom values at
		// this boundary because their formatting can disclose arbitrary data.
		return "[hidden]"
	}
}

func sensitiveArgumentKey(key string) bool {
	key = canonicalPrivacyKey(key)
	for _, fragment := range []string{
		"token", "password", "passwd", "secret", "authorization", "authentication",
		"auth", "cookie", "apikey", "credential", "manifestyaml", "content",
	} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func canonicalPrivacyKey(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, strings.TrimSpace(value))
}
