package ui

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// checkpointTranscriptFromMessages reconstructs only the bounded presentation
// facts that a checkpoint can prove. Tool result text, arguments, provider
// reasoning, and raw MCP StructuredContent never cross into UI/session state.
// A stored tool response proves that the invocation reached a receipt, but not
// that its domain operation succeeded or that evidence was verified.
func checkpointTranscriptFromMessages(msgs []llm.Message) ([]ChatEntry, []ToolEntry) {
	var entries []ChatEntry
	var tools []ToolEntry
	for messageIndex, message := range msgs {
		switch message.Role {
		case "user":
			if message.Content != "" {
				entries = append(entries, ChatEntry{
					Kind:        "user",
					Content:     message.Content,
					Attachments: imageRefsFromMessages(message.Images),
				})
			}
		case "assistant":
			if message.Content != "" {
				entries = append(entries, ChatEntry{Kind: "assistant", Content: message.Content})
			}
		case "tool":
			name := sanitizeTerminalSingleLine(strings.TrimSpace(message.ToolName))
			name = truncateDisplay(name, 96)
			if name == "" {
				name = "tool"
			}
			callID := strings.TrimSpace(message.ToolCallID)
			if !validTranscriptID(callID) {
				callID = fmt.Sprintf("checkpoint-tool-%d", messageIndex)
			}
			toolIndex := len(tools)
			tools = append(tools, ToolEntry{
				ID:        callID,
				Name:      name,
				Summary:   "restored receipt · outcome requires review",
				Status:    ToolStatusDone,
				Collapsed: true,
				Projection: ecosystem.ToolProjection{
					Operation: name,
					Transport: ecosystem.TransportSucceeded,
					Domain:    ecosystem.DomainUnknown,
					Evidence:  ecosystem.EvidenceNone,
				}.Normalize(),
			})
			entries = append(entries, ChatEntry{Kind: "tool_group", ToolIndex: toolIndex})
		}
	}
	return entries, tools
}
