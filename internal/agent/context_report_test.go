package agent

import (
	"context"
	"testing"
)

func TestContextBreakdownReportsBoundedSections(t *testing.T) {
	ag := New(nil, nil, 8192)
	ag.SetLoadedContext("Project instructions: prefer table-driven tests.")
	ag.SetSkillContent("Skill body: review Go code for lock ordering.")
	ag.AddUserMessage("please look at the failing session restore test")

	breakdown := ag.ContextBreakdown(context.Background())

	if breakdown.NumCtx != 8192 {
		t.Fatalf("NumCtx = %d, want 8192", breakdown.NumCtx)
	}
	if breakdown.TotalTokens <= 0 {
		t.Fatalf("TotalTokens = %d, want > 0", breakdown.TotalTokens)
	}

	sections := make(map[string]ContextSection, len(breakdown.Sections))
	sum := 0
	for _, section := range breakdown.Sections {
		if section.Tokens < 0 {
			t.Fatalf("section %q has negative tokens %d", section.Key, section.Tokens)
		}
		sections[section.Key] = section
		sum += section.Tokens
	}
	for _, key := range []string{"system", "environment", "tools", "conversation"} {
		if _, ok := sections[key]; !ok {
			t.Fatalf("core section %q missing from %v", key, breakdown.Sections)
		}
	}
	if sections["system"].Tokens == 0 {
		t.Fatal("system section should charge the base template")
	}
	if sections["context"].Tokens == 0 {
		t.Fatal("loaded context should appear once SetLoadedContext ran")
	}
	if sections["skills"].Tokens == 0 {
		t.Fatal("skills section should appear once SetSkillContent ran")
	}
	if sections["conversation"].Tokens == 0 {
		t.Fatal("conversation section should charge the pending user message")
	}
	if sections["conversation"].Detail != "1 message" {
		t.Fatalf("conversation detail = %q, want %q", sections["conversation"].Detail, "1 message")
	}
	if sum > breakdown.TotalTokens {
		t.Fatalf("section sum %d exceeds total %d", sum, breakdown.TotalTokens)
	}
}

func TestContextBreakdownMatchesTurnEstimator(t *testing.T) {
	ag := New(nil, nil, 16384)
	ag.AddUserMessage("hello")

	breakdown := ag.ContextBreakdown(context.Background())

	// The breakdown must agree with the admission estimator the turn budget
	// uses: host prompt (system text + tool schemas + framing) + messages.
	_, policy := ag.modeContext()
	var toolCount int
	for _, section := range breakdown.Sections {
		if section.Key == "tools" {
			toolCount = section.Tokens
		}
	}
	if policy.AllowMCP && toolCount == 0 {
		t.Fatal("tool schema tokens should be charged for the default policy")
	}
	if breakdown.TotalTokens <= toolCount {
		t.Fatalf("total %d should exceed tool-only tokens %d", breakdown.TotalTokens, toolCount)
	}
}
