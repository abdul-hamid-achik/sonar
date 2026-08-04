package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRootOptionsTreatsVersionOnlyAsAParsedFlag(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantVersion   bool
		wantPrompt    string
		wantPromptSet bool
		wantModeSet   bool
		wantArguments []string
	}{
		{name: "long flag", args: []string{"--version"}, wantVersion: true},
		{name: "single dash compatibility", args: []string{"-version"}, wantVersion: true},
		{name: "long prompt value", args: []string{"--prompt", "--version"}, wantPrompt: "--version", wantPromptSet: true},
		{name: "short prompt value", args: []string{"-p", "--version"}, wantPrompt: "--version", wantPromptSet: true},
		{name: "equals prompt value", args: []string{"--prompt=--version"}, wantPrompt: "--version", wantPromptSet: true},
		{name: "prompt-looking model value", args: []string{"--model", "--prompt"}},
		{name: "mode-looking prompt value", args: []string{"--prompt", "--mode"}, wantPrompt: "--mode", wantPromptSet: true},
		{name: "explicit mode", args: []string{"--mode", "plan", "--prompt", "work"}, wantPrompt: "work", wantPromptSet: true, wantModeSet: true},
		{name: "after terminator", args: []string{"--", "--version"}, wantArguments: []string{"--version"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := parseRootOptions("sonar", test.args, &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if options.version != test.wantVersion || options.prompt != test.wantPrompt {
				t.Fatalf("options = version:%v prompt:%q, want %v/%q", options.version, options.prompt, test.wantVersion, test.wantPrompt)
			}
			if options.promptProvided != test.wantPromptSet || options.modeProvided != test.wantModeSet {
				t.Fatalf("presence = prompt:%v mode:%v, want %v/%v", options.promptProvided, options.modeProvided, test.wantPromptSet, test.wantModeSet)
			}
			if strings.Join(options.arguments, "\x00") != strings.Join(test.wantArguments, "\x00") {
				t.Fatalf("arguments = %#v, want %#v", options.arguments, test.wantArguments)
			}
		})
	}
}

func TestParseRootOptionsPreservesOrderingAliasesAndFlagLikeValues(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantPrompt    string
		wantPromptSet bool
		wantMode      string
		wantModeSet   bool
		wantModel     string
		wantAuto      bool
		wantPlan      bool
		wantSkip      bool
		wantYolo      bool
		wantVersion   bool
		wantResume    string
		wantArguments []string
	}{
		{
			name:          "flags before and after long prompt",
			args:          []string{"--auto", "--prompt", "work", "--skip-approvals"},
			wantPrompt:    "work",
			wantPromptSet: true,
			wantMode:      "normal",
			wantAuto:      true,
			wantSkip:      true,
		},
		{
			name:          "deprecated approval alias remains independent from plan",
			args:          []string{"--prompt", "work", "--plan", "--yolo"},
			wantPrompt:    "work",
			wantPromptSet: true,
			wantMode:      "normal",
			wantPlan:      true,
			wantYolo:      true,
		},
		{
			name:          "long prompt consumes auto-looking value",
			args:          []string{"--prompt", "--auto", "--plan"},
			wantPrompt:    "--auto",
			wantPromptSet: true,
			wantMode:      "normal",
			wantPlan:      true,
		},
		{
			name:          "short prompt consumes plan-looking value",
			args:          []string{"-p", "--plan", "--auto"},
			wantPrompt:    "--plan",
			wantPromptSet: true,
			wantMode:      "normal",
			wantAuto:      true,
		},
		{
			name:          "mode consumes auto-looking value",
			args:          []string{"--mode", "--auto", "--prompt", "work"},
			wantPrompt:    "work",
			wantPromptSet: true,
			wantMode:      "--auto",
			wantModeSet:   true,
		},
		{
			name:      "model consumes prompt-looking value",
			args:      []string{"--model", "--prompt", "--auto"},
			wantMode:  "normal",
			wantModel: "--prompt",
			wantAuto:  true,
		},
		{
			name:          "last prompt alias wins",
			args:          []string{"-p", "first", "--prompt", "second"},
			wantPrompt:    "second",
			wantPromptSet: true,
			wantMode:      "normal",
		},
		{
			name:        "resume consumes version-looking value",
			args:        []string{"--resume", "--version"},
			wantMode:    "normal",
			wantResume:  "--version",
			wantVersion: false,
		},
		{
			name:          "positional argument stops flag parsing",
			args:          []string{"work", "--auto", "--version"},
			wantMode:      "normal",
			wantArguments: []string{"work", "--auto", "--version"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := parseRootOptions("sonar", test.args, &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if options.prompt != test.wantPrompt || options.promptProvided != test.wantPromptSet {
				t.Fatalf("prompt = %q provided=%v, want %q/%v", options.prompt, options.promptProvided, test.wantPrompt, test.wantPromptSet)
			}
			if options.mode != test.wantMode || options.modeProvided != test.wantModeSet {
				t.Fatalf("mode = %q provided=%v, want %q/%v", options.mode, options.modeProvided, test.wantMode, test.wantModeSet)
			}
			if options.model != test.wantModel || options.auto != test.wantAuto || options.plan != test.wantPlan {
				t.Fatalf("model/authority = %q auto=%v plan=%v, want %q/%v/%v", options.model, options.auto, options.plan, test.wantModel, test.wantAuto, test.wantPlan)
			}
			if options.skipApprovals != test.wantSkip || options.legacyYolo != test.wantYolo {
				t.Fatalf("approval flags = skip:%v yolo:%v, want %v/%v", options.skipApprovals, options.legacyYolo, test.wantSkip, test.wantYolo)
			}
			if options.version != test.wantVersion || options.resume.value != test.wantResume || options.resume.set != (test.wantResume != "") {
				t.Fatalf("version/resume = %v/%q (set=%v), want %v/%q", options.version, options.resume.value, options.resume.set, test.wantVersion, test.wantResume)
			}
			if strings.Join(options.arguments, "\x00") != strings.Join(test.wantArguments, "\x00") {
				t.Fatalf("arguments = %#v, want %#v", options.arguments, test.wantArguments)
			}
		})
	}
}

func TestParseRootOptionsHelpIsSideEffectFree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, err := parseRootOptions("sonar", []string{"-h"}, &stderr, &stdout)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v, want flag.ErrHelp", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("help wrote to stderr: %q", stderr.String())
	}
	help := stdout.String()
	for _, text := range []string{
		"sonar <command> [options]",
		"init       ",
		"logs       ",
		"goal       ",
		"execution  ",
		"-p, --prompt <text>",
		"--auto",
		"--plan",
		"--tools",
		"--skip-approvals",
		"--version",
	} {
		if !strings.Contains(help, text) {
			t.Fatalf("help omitted %q:\n%s", text, help)
		}
	}
	for _, legacySpelling := range []string{"\n  -auto ", "\n  -plan ", "\n  -skip-approvals "} {
		if strings.Contains(help, legacySpelling) {
			t.Fatalf("help exposed single-dash spelling %q:\n%s", legacySpelling, help)
		}
	}
	assertHelpLinesAtMost(t, help, 100)
}

func TestParseRootOptionsTracksHeadlessToolsPresence(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		value string
	}{
		{name: "comma separated", args: []string{"--tools", "read,diff", "--prompt", "work"}, value: "read,diff"},
		{name: "explicit empty", args: []string{"--tools=", "--prompt", "work"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, err := parseRootOptions("sonar", test.args, &bytes.Buffer{}, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if !options.toolsProvided || options.tools != test.value {
				t.Fatalf("tools = %q provided=%v, want %q/true", options.tools, options.toolsProvided, test.value)
			}
		})
	}
}

func TestRunValidatesHeadlessToolsBeforeConfiguration(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "sonar.yaml"), []byte("ollama: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)

	runWithArgs := func(args ...string) int {
		t.Helper()
		previous := os.Args
		os.Args = append([]string{"sonar"}, args...)
		defer func() { os.Args = previous }()
		return run()
	}

	for _, args := range [][]string{
		{"--tools", "read"},
		{"--tools=", "--prompt", "work"},
		{"--tools", "read,read", "--prompt", "work"},
		{"--tools", "not_a_tool", "--prompt", "work"},
		{"--plan", "--tools", "write", "--prompt", "work"},
	} {
		if code := runWithArgs(args...); code != 2 {
			t.Fatalf("invalid tools invocation %q exit = %d, want usage exit 2", args, code)
		}
	}
	if code := runWithArgs("--plan", "--tools", "read,diff", "--prompt", "work"); code != 1 {
		t.Fatalf("valid narrowed invocation exit = %d, want invalid-config exit 1", code)
	}
}

func TestParseRootOptionsErrorsStayOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, err := parseRootOptions("sonar", []string{"--unknown"}, &stderr, &stdout)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		t.Fatalf("unknown flag error = %v, want parse error", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unknown flag wrote help to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("unknown flag stderr = %q", stderr.String())
	}
}

func TestRunHandlesVersionAndEmptyPromptsBeforeConfiguration(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "sonar.yaml"), []byte("ollama: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workDir)

	runWithArgs := func(args ...string) int {
		t.Helper()
		previous := os.Args
		os.Args = append([]string{"sonar"}, args...)
		defer func() { os.Args = previous }()
		return run()
	}

	if code := runWithArgs("--version"); code != 0 {
		t.Fatalf("real --version exit = %d, want 0 before invalid config", code)
	}
	for _, args := range [][]string{{"help"}, {"--help"}} {
		if code := runWithArgs(args...); code != 0 {
			t.Fatalf("help invocation %q exit = %d, want 0 before invalid config", args, code)
		}
	}
	for _, args := range [][]string{{"--prompt="}, {"-p="}, {"--prompt", "   "}} {
		if code := runWithArgs(args...); code != 2 {
			t.Fatalf("empty prompt %q exit = %d, want 2 before invalid config", args, code)
		}
	}
	if code := runWithArgs("--", "--version"); code != 2 {
		t.Fatalf("terminated --version exit = %d, want unexpected-argument exit 2", code)
	}
	if code := runWithArgs("--prompt", "--version"); code != 1 {
		t.Fatalf("prompt value --version exit = %d, want invalid-config exit 1 instead of version success", code)
	}
	for _, args := range [][]string{
		{"-p", "--plan"},
		{"--auto", "--prompt", "work"},
		{"--prompt", "work", "--auto"},
		{"--plan", "--prompt", "work"},
		{"--prompt", "work", "--plan"},
		{"--skip-approvals"},
		{"--yolo"},
	} {
		if code := runWithArgs(args...); code != 1 {
			t.Fatalf("valid preflight invocation %q exit = %d, want invalid-config exit 1", args, code)
		}
	}
	for _, args := range [][]string{
		{"--auto"},
		{"--plan"},
		{"--auto", "--plan", "--prompt", "work"},
		{"--auto", "--mode", "plan", "--prompt", "work"},
		{"--plan", "--mode", "auto", "--prompt", "work"},
		{"--mode", "--auto", "--prompt", "work"},
		{"--model", "--prompt", "--auto"},
		{"--resume", "--version"},
		{"--resume", "latest", "--prompt", "work"},
		{"work", "--version"},
	} {
		if code := runWithArgs(args...); code != 2 {
			t.Fatalf("invalid preflight invocation %q exit = %d, want usage exit 2", args, code)
		}
	}
}
