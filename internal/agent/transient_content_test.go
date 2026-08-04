package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	transientPageSecret = "SECRET_TRANSIENT_MCPHUB_PAGE"
	durablePageReceipt  = "MCPHub · result page metadata only"
)

func TestSanitizeMessagesForPersistenceReplacesTransientContent(t *testing.T) {
	messages := []llm.Message{{
		Role: "tool", Content: transientPageSecret, DurableContent: durablePageReceipt,
		ToolName: "mcphub__mcphub_get_result", ToolCallID: "call-1",
	}}

	safe := SanitizeMessagesForPersistence(messages)
	if safe[0].Content != durablePageReceipt || safe[0].DurableContent != "" {
		t.Fatalf("sanitized message = %#v", safe[0])
	}
	if messages[0].Content != transientPageSecret || messages[0].DurableContent != durablePageReceipt {
		t.Fatalf("sanitization mutated live history: %#v", messages[0])
	}
}

func TestCheckpointSnapshotNeverPersistsTransientContent(t *testing.T) {
	ag := New(nil, nil, 8192)
	ag.AppendMessage(llm.Message{
		Role: "tool", Content: transientPageSecret, DurableContent: durablePageReceipt,
		ToolName: "mcphub__mcphub_get_result", ToolCallID: "call-1",
	})

	raw, count, err := ag.snapshotMessagesJSON()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || strings.Contains(raw, transientPageSecret) || !strings.Contains(raw, durablePageReceipt) || strings.Contains(raw, "DurableContent") {
		t.Fatalf("checkpoint snapshot leaked transient content: %s", raw)
	}
	if got := ag.Messages()[0]; got.Content != transientPageSecret || got.DurableContent != durablePageReceipt {
		t.Fatalf("checkpoint changed live history: %#v", got)
	}
}

func TestSettleTransientMessagesEndsProviderOnlyLifetime(t *testing.T) {
	ag := New(nil, nil, 8192)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "keep"},
		{Role: "tool", Content: transientPageSecret, DurableContent: durablePageReceipt},
		{Role: "assistant", Content: "keep response"},
	})

	if settled := ag.settleTransientMessages(); settled != 1 {
		t.Fatalf("settled messages = %d, want 1", settled)
	}
	got := ag.Messages()
	if got[1].Content != durablePageReceipt || got[1].DurableContent != "" || got[0].Content != "keep" || got[2].Content != "keep response" {
		t.Fatalf("settled history = %#v", got)
	}
	if settled := ag.settleTransientMessages(); settled != 0 {
		t.Fatalf("second settlement changed %d messages", settled)
	}
}

type transientCompactionClient struct {
	summaryPrompt string
}

func (c *transientCompactionClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	if strings.Contains(options.System, "conversation summarizer") {
		if len(options.Messages) > 0 {
			c.summaryPrompt = options.Messages[0].Content
		}
		return emit(llm.StreamChunk{Text: "bounded safe summary", Done: true})
	}
	return nil
}

func (*transientCompactionClient) Ping() error   { return nil }
func (*transientCompactionClient) Model() string { return "transient-compaction-test" }
func (*transientCompactionClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestCompactionSummarizesDurableReceiptAndKeepsRecentTransientPage(t *testing.T) {
	client := &transientCompactionClient{}
	ag := New(client, nil, 100_000)
	ag.ReplaceMessages([]llm.Message{
		{Role: "user", Content: "old request"},
		{Role: "tool", Content: "OLD_" + transientPageSecret, DurableContent: "old " + durablePageReceipt, ToolName: "mcphub__mcphub_get_result"},
		{Role: "assistant", Content: "old response"},
		{Role: "user", Content: "middle request"},
		{Role: "assistant", Content: "middle response"},
		{Role: "user", Content: "recent request"},
		{Role: "tool", Content: "RECENT_" + transientPageSecret, DurableContent: "recent " + durablePageReceipt, ToolName: "mcphub__mcphub_get_result"},
		{Role: "assistant", Content: "recent response"},
	})

	if !ag.compact(context.Background(), &mockOutput{}) {
		t.Fatal("expected compaction")
	}
	if strings.Contains(client.summaryPrompt, transientPageSecret) || !strings.Contains(client.summaryPrompt, "old "+durablePageReceipt) {
		t.Fatalf("summary prompt crossed transient boundary: %q", client.summaryPrompt)
	}

	compacted := ag.Messages()
	foundRecentTransient := false
	for _, message := range compacted {
		if strings.Contains(message.Content, "OLD_"+transientPageSecret) {
			t.Fatalf("old transient content survived compaction: %#v", compacted)
		}
		if strings.Contains(message.Content, "RECENT_"+transientPageSecret) {
			foundRecentTransient = true
			if message.DurableContent != "recent "+durablePageReceipt {
				t.Fatalf("recent transient lost durable replacement: %#v", message)
			}
		}
	}
	if !foundRecentTransient {
		t.Fatalf("active recent transient page was discarded: %#v", compacted)
	}

	persisted := SanitizeMessagesForPersistence(compacted)
	for _, message := range persisted {
		if strings.Contains(message.Content, transientPageSecret) || message.DurableContent != "" {
			t.Fatalf("compacted persistence leaked transient content: %#v", persisted)
		}
	}
}
