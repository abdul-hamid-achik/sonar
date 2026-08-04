package main

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

func TestNarrowHeadlessToolPolicyOnlyReducesSelectedMode(t *testing.T) {
	policy, err := narrowHeadlessToolPolicy(agent.PlanToolPolicy(), " read, diff ")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read", "diff"} {
		if !policy.AllowsBuiltin(name) {
			t.Fatalf("narrowed policy omitted %q", name)
		}
	}
	if policy.AllowsBuiltin("exists") {
		t.Fatal("narrowed policy retained an unrequested built-in")
	}
	if policy.AllowMCP {
		t.Fatal("narrowed policy enabled MCP")
	}
	if got := policy.MemoryNames(); len(got) != 0 {
		t.Fatalf("narrowed policy retained memory tools: %v", got)
	}
}

func TestNarrowHeadlessToolPolicyRejectsInvalidLists(t *testing.T) {
	tests := []struct {
		name    string
		base    agent.ToolPolicy
		value   string
		wantErr string
	}{
		{name: "empty", base: agent.BuildToolPolicy(), value: "  ", wantErr: "one or more"},
		{name: "empty item", base: agent.BuildToolPolicy(), value: "read,,diff", wantErr: "empty tool name"},
		{name: "duplicate", base: agent.BuildToolPolicy(), value: "read, read", wantErr: "repeats tool"},
		{name: "unknown", base: agent.BuildToolPolicy(), value: "read,not_a_tool", wantErr: "unknown built-in"},
		{name: "memory is not a built-in", base: agent.BuildToolPolicy(), value: "memory_recall", wantErr: "unknown built-in"},
		{name: "mode disallows", base: agent.PlanToolPolicy(), value: "write", wantErr: "not allowed by the selected mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := narrowHeadlessToolPolicy(test.base, test.value)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want text %q", err, test.wantErr)
			}
		})
	}
}

func TestNarrowHeadlessSystemPromptIsDeterministicAndStopsDuplicateCalls(t *testing.T) {
	policy := agent.NewToolPolicy([]string{"read", "diff"}, nil, false)
	prompt := narrowHeadlessSystemPrompt("Plan safely.", policy)
	for _, want := range []string{
		"Plan safely.",
		"at most once",
		"visible final text",
		"Available built-ins for this turn: diff, read.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("narrowed system prompt omitted %q: %q", want, prompt)
		}
	}
}
