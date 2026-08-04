package main

import (
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

func TestParseHeadlessMode(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		headless bool
		want     ui.Mode
		wantErr  bool
		wantText string
	}{
		{name: "default", value: "", headless: true, want: ui.ModeNormal},
		{name: "normal", value: " NORMAL ", headless: true, want: ui.ModeNormal},
		{name: "plan", value: "plan", headless: true, want: ui.ModePlan},
		{name: "auto", value: "auto", headless: true, want: ui.ModeAuto},
		{name: "unknown", value: "build", headless: true, wantErr: true},
		{name: "interactive flag unsupported", value: "plan", headless: false, wantErr: true, wantText: "-p/--prompt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseHeadlessMode(test.value, test.headless)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseHeadlessMode() error = %v, wantErr=%v", err, test.wantErr)
			}
			if test.wantText != "" && (err == nil || !strings.Contains(err.Error(), test.wantText)) {
				t.Fatalf("parseHeadlessMode() error = %v, want containing %q", err, test.wantText)
			}
			if !test.wantErr && got != test.want {
				t.Fatalf("parseHeadlessMode() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveAuthorityShortcut(t *testing.T) {
	tests := []struct {
		name         string
		value        string
		modeExplicit bool
		auto         bool
		plan         bool
		want         string
		wantErr      string
	}{
		{name: "unchanged", value: "normal", want: "normal"},
		{name: "auto shortcut", value: "normal", auto: true, want: "auto"},
		{name: "plan shortcut", value: "normal", plan: true, want: "plan"},
		{name: "matching explicit auto", value: " AUTO ", modeExplicit: true, auto: true, want: "auto"},
		{name: "matching explicit plan", value: "plan", modeExplicit: true, plan: true, want: "plan"},
		{name: "shortcuts conflict", value: "normal", auto: true, plan: true, wantErr: "mutually exclusive"},
		{name: "auto conflicts with mode", value: "plan", modeExplicit: true, auto: true, wantErr: "conflicts"},
		{name: "plan conflicts with mode", value: "auto", modeExplicit: true, plan: true, wantErr: "conflicts"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveAuthorityShortcut(test.value, test.modeExplicit, test.auto, test.plan)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("resolveAuthorityShortcut() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("resolveAuthorityShortcut() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveAuthorityShortcutQuotesUntrustedModeValue(t *testing.T) {
	_, err := resolveAuthorityShortcut("plan\n\x1b[31m\u202einjected", true, true, false)
	if err == nil {
		t.Fatal("control-bearing conflicting mode succeeded")
	}
	if strings.ContainsAny(err.Error(), "\n\x1b\u202e") {
		t.Fatalf("conflict diagnostic contains raw terminal controls: %q", err.Error())
	}
	if !strings.Contains(err.Error(), `"plan\n\x1b[31m\u202einjected"`) {
		t.Fatalf("conflict diagnostic did not preserve a safely quoted value: %q", err.Error())
	}
}

func TestHeadlessAuthorityMode(t *testing.T) {
	tests := []struct {
		mode ui.Mode
		want agent.AuthorityMode
	}{
		{mode: ui.ModeNormal, want: agent.AuthorityNormal},
		{mode: ui.ModePlan, want: agent.AuthorityPlan},
		{mode: ui.ModeAuto, want: agent.AuthorityAutoScoped},
		{mode: ui.Mode(99), want: agent.AuthorityNormal},
	}
	for _, test := range tests {
		if got := headlessAuthorityMode(test.mode); got != test.want {
			t.Fatalf("headless authority for %v = %v, want %v", test.mode, got, test.want)
		}
	}
}
