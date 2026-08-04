package ui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// formatConversationForExport formats the current conversation as markdown.
func (m *Model) formatConversationForExport() string {
	var b strings.Builder
	b.WriteString("# Conversation Export\n\n")
	fmt.Fprintf(&b, "**Date**: %s\n", time.Now().Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "**Model**: %s\n", m.model)
	portable := portableConversationExport{Version: 2}
	for _, entry := range m.entries {
		switch entry.Kind {
		case "user", "assistant", "system":
			portable.Entries = append(portable.Entries, portableConversationEntry{Kind: entry.Kind, Content: entry.Content})
		}
	}
	if payload, err := json.Marshal(portable); err == nil {
		b.WriteString("<!-- sonar-export-v2:")
		b.WriteString(base64.RawStdEncoding.EncodeToString(payload))
		b.WriteString(" -->\n")
	}
	b.WriteString("---\n\n")

	for _, entry := range m.entries {
		switch entry.Kind {
		case "user":
			b.WriteString("## User\n\n")
			b.WriteString(entry.Content)
			b.WriteString("\n\n---\n\n")
		case "assistant":
			b.WriteString("## Assistant\n\n")
			b.WriteString(entry.Content)
			b.WriteString("\n\n---\n\n")
		case "system":
			b.WriteString("## System\n\n")
			b.WriteString(entry.Content)
			b.WriteString("\n\n---\n\n")
		case "tool_group":
			if entry.ToolIndex >= 0 && entry.ToolIndex < len(m.toolEntries) {
				te := m.toolEntries[entry.ToolIndex]
				fmt.Fprintf(&b, "## Tool: %s\n\n", te.Name)
				b.WriteString("```\n")
				b.WriteString(te.Args)
				b.WriteString("\n```\n\n")
				if te.Result != "" {
					b.WriteString("**Result**:\n\n")
					b.WriteString("```\n")
					b.WriteString(te.Result)
					b.WriteString("\n```\n\n")
				}
				b.WriteString("---\n\n")
			}
		}
	}

	return b.String()
}

type portableConversationExport struct {
	Version int                         `json:"version"`
	Entries []portableConversationEntry `json:"entries"`
}

type portableConversationEntry struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

const maxPortableConversationEntries = 10_000

// parseImportedConversation reads only the typed v2 payload embedded in a
// human-readable Markdown export. Legacy Markdown is inherently ambiguous:
// model/tool content can contain role-looking headings, so guessing authority
// from headings would enable a tool receipt to become a hidden user message.
func (m *Model) parseImportedConversation(data string) ([]ChatEntry, error) {
	return parseImportedConversationData(data)
}

func parseImportedConversationData(data string) ([]ChatEntry, error) {
	const marker = "<!-- sonar-export-v2:"
	start := strings.Index(data, marker)
	if start < 0 {
		return nil, fmt.Errorf("legacy Markdown imports are disabled because role headings inside model/tool output are ambiguous; import a v2 file created by this release")
	}
	encoded := data[start+len(marker):]
	end := strings.Index(encoded, " -->")
	if end < 0 {
		return nil, fmt.Errorf("v2 export payload is not terminated")
	}
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded[:end]))
	if err != nil {
		return nil, fmt.Errorf("decode v2 export payload: %w", err)
	}
	var portable portableConversationExport
	if err := json.Unmarshal(payload, &portable); err != nil {
		return nil, fmt.Errorf("decode v2 conversation: %w", err)
	}
	if portable.Version != 2 {
		return nil, fmt.Errorf("unsupported conversation export version %d", portable.Version)
	}
	if len(portable.Entries) == 0 || len(portable.Entries) > maxPortableConversationEntries {
		return nil, fmt.Errorf("v2 conversation contains %d entries", len(portable.Entries))
	}
	entries := make([]ChatEntry, 0, len(portable.Entries))
	for _, entry := range portable.Entries {
		switch entry.Kind {
		case "user", "assistant", "system":
		default:
			return nil, fmt.Errorf("v2 conversation contains unsupported entry kind %q", entry.Kind)
		}
		if strings.TrimSpace(entry.Content) != "" {
			entries = append(entries, ChatEntry{Kind: entry.Kind, Content: entry.Content})
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("v2 conversation contains no visible entries")
	}
	return entries, nil
}

func importedConversationMessages(entries []ChatEntry) ([]llm.Message, int, error) {
	messages := make([]llm.Message, 0, len(entries))
	uiOnlySections := 0
	for _, entry := range entries {
		switch entry.Kind {
		case "user", "assistant":
			if strings.TrimSpace(entry.Content) != "" {
				messages = append(messages, llm.Message{Role: entry.Kind, Content: entry.Content})
			}
		case "system":
			uiOnlySections++
		default:
			return nil, 0, fmt.Errorf("unsupported transcript section %q", entry.Kind)
		}
	}
	if len(messages) == 0 {
		return nil, 0, fmt.Errorf("no user or assistant messages were found")
	}
	return messages, uiOnlySections, nil
}
