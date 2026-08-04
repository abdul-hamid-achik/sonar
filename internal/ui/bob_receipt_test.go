package ui

import (
	"strings"
	"testing"
)

func TestIsBobToolName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"bob", true},
		{"bob_plan", true},
		{"bob__bob_plan", true},
		{"mcphub__bob__bob_apply", true},
		{"BOB__BOB_CHECK", true},
		{"bob-validate-manifest", true},
		{"bash", false},
		{"read_file", false},
		{"bobbin", false},
		{"mcphub__bobcat__scan", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isBobToolName(tc.name); got != tc.want {
			t.Errorf("isBobToolName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

const bobRefusedApplyEnvelope = `{
  "schema_version": 1,
  "ok": false,
  "command": "apply",
  "data": {
    "conflicts": [
      {"code": "unmanaged_differs", "path": "README.md", "reason": "unmanaged file differs from the desired content"}
    ],
    "error": {"code": "conflicts", "message": "apply: plan contains conflicts; run bob plan for details"}
  },
  "warnings": ["1 conflict(s) block apply"],
  "next_actions": ["run: bob plan --json and inspect actions with kind=conflict", "resolve each conflict, then rerun bob apply"]
}`

func TestBobReceiptDigest(t *testing.T) {
	digest := bobReceiptDigest("mcphub__bob__bob_apply", bobRefusedApplyEnvelope)
	for _, want := range []string{"conflicts", "unmanaged_differs: README.md", "next: "} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing %q:\n%s", want, digest)
		}
	}

	if digest := bobReceiptDigest("bash", bobRefusedApplyEnvelope); digest != "" {
		t.Errorf("non-Bob tools must not be digested, got:\n%s", digest)
	}
	if digest := bobReceiptDigest("bob__bob_plan", "plain text output"); digest != "" {
		t.Errorf("non-envelope output must be left alone, got:\n%s", digest)
	}
}

func TestBobToolPresentationLabels(t *testing.T) {
	p := presentTool("mcphub__bob__bob_plan", ToolCardGeneric, ToolCardSuccess)
	if p.label != "Planned repository" {
		t.Errorf("unexpected label %q", p.label)
	}
	p = presentTool("bob_apply", ToolCardGeneric, ToolCardError)
	if p.label != "Repository apply failed" {
		t.Errorf("unexpected label %q", p.label)
	}
	p = presentTool("bob__bob_context", ToolCardGeneric, ToolCardAttention)
	if p.label != "Repository context needs attention" {
		t.Errorf("unexpected label %q", p.label)
	}
}
