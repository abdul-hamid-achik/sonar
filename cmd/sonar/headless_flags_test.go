package main

import (
	"strings"
	"testing"
)

// These checks lived inline at the top of a 1154-line run() as bare `return 2`
// blocks, which made them unreachable from a test: the exact wording a script
// reads on stderr had no coverage at all, and the exit code is 2 whichever one
// fired.
func TestHeadlessFlagCombinationsAreRejectedWithTheirReason(t *testing.T) {
	for _, test := range []struct {
		name    string
		options rootOptions
		wants   []string
	}{
		{
			name:    "blank prompt",
			options: rootOptions{promptProvided: true, prompt: "   "},
			wants:   []string{"prompt:", "-p/--prompt", "non-empty"},
		},
		{
			name:    "tools without a prompt",
			options: rootOptions{toolsProvided: true},
			wants:   []string{"tools:", "--tools", "-p/--prompt"},
		},
		{
			name:    "json receipt without a prompt",
			options: rootOptions{jsonReceipt: true},
			wants:   []string{"json:", "--json", "-p/--prompt"},
		},
		{
			name:    "run id without a prompt",
			options: rootOptions{runID: "run_1"},
			wants:   []string{"identity:", "--run-id", "-p/--prompt"},
		},
		{
			name:    "turn id without a prompt",
			options: rootOptions{turnID: "turn_1"},
			wants:   []string{"identity:", "--turn-id"},
		},
		{
			name:    "actor without a prompt",
			options: rootOptions{actor: "ci"},
			wants:   []string{"identity:", "--actor"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateHeadlessFlagCombinations(test.options)
			if err == nil {
				t.Fatalf("%#v was accepted", test.options)
			}
			// The caller is usually a shell script, so the message has to name
			// both the flag that was rejected and the flag it needs.
			for _, want := range test.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// Every one of these flags is legitimate once a prompt is present — the checks
// exist to catch a flag that cannot mean anything, not to restrict headless
// runs.
func TestHeadlessFlagsAreAcceptedWithAPrompt(t *testing.T) {
	options := rootOptions{
		promptProvided: true,
		prompt:         "summarise the release notes",
		toolsProvided:  true,
		jsonReceipt:    true,
		runID:          "run_1",
		turnID:         "turn_1",
		actor:          "ci",
	}
	if err := validateHeadlessFlagCombinations(options); err != nil {
		t.Fatalf("a fully specified headless request was rejected: %v", err)
	}
}

// An interactive launch passes none of them and must not be second-guessed.
func TestInteractiveLaunchPassesValidation(t *testing.T) {
	if err := validateHeadlessFlagCombinations(rootOptions{}); err != nil {
		t.Fatalf("an ordinary interactive launch was rejected: %v", err)
	}
}
