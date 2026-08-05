package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Two defects that shipped here, in opposite directions, both about a chord and
// its description disagreeing.
//
// The model picker's footer advertised "d details · a add · r refresh" while
// only Enter was handled — keys promised in the one place a user looks to learn
// what a surface can do. And alt+d, which opens the full diff, worked but
// appeared in no help group at all, so the thing people most want after an edit
// was reachable only by already knowing it existed.
//
// These guard both directions. They are cheap; rediscovering either by accident
// was not.

// Every chord the key registry defines must be reachable from /help. A binding
// that never renders is a feature only its author can use.
func TestEveryRegisteredChordIsDocumented(t *testing.T) {
	m := newTestModel(t)
	help := ansi.Strip(m.buildHelpContent(m.helpContentWidth()))
	for _, section := range m.keys.HelpSections() {
		for _, binding := range section.Bindings {
			shown := strings.TrimSpace(binding.Help().Key)
			if shown == "" {
				continue
			}
			if !strings.Contains(help, shown) {
				t.Errorf("%q (%s) is bound but never rendered in /help",
					shown, binding.Help().Desc)
			}
		}
	}
}

// No two registry bindings may claim the same chord. A collision resolves by
// switch order, which is invisible at the call site and silent at review.
func TestNoTwoBindingsClaimTheSameChord(t *testing.T) {
	m := newTestModel(t)
	owner := map[string]string{}
	for _, section := range m.keys.HelpSections() {
		for _, binding := range section.Bindings {
			for _, chord := range binding.Keys() {
				desc := binding.Help().Desc
				if previous, taken := owner[chord]; taken && previous != desc {
					t.Errorf("%q is claimed by both %q and %q", chord, previous, desc)
				}
				owner[chord] = desc
			}
		}
	}
}

// Escape, Ctrl+C and Enter are the keys a user reaches for when everything else
// has gone wrong. They must keep meaning cancel, quit and send.
func TestReservedChordsKeepTheirMeaning(t *testing.T) {
	m := newTestModel(t)
	for chord, want := range map[string]string{
		"esc":    "cancel",
		"ctrl+c": "quit",
		"enter":  "send",
	} {
		found := ""
		for _, section := range m.keys.HelpSections() {
			for _, binding := range section.Bindings {
				for _, k := range binding.Keys() {
					if k == chord {
						found = strings.ToLower(binding.Help().Desc)
					}
				}
			}
		}
		if found == "" {
			t.Errorf("%q is unbound; it must always do something predictable", chord)
			continue
		}
		if !strings.Contains(found, want) {
			t.Errorf("%q now means %q, want it to still mean %q", chord, found, want)
		}
	}
}

// A chord handled by a literal msg.String() escapes the registry, and with it
// /help and the collision check — which is exactly how alt+d came to open the
// diff viewer while appearing nowhere.
//
// A literal is only a defect when the registry does not already define that
// chord. Modal viewports implementing ctrl+d/ctrl+u as half-page scroll are
// spelling out the meaning the registry already publishes; that is consistency.
// A literal for a chord nobody declared is an invisible feature.
func TestNoLiteralChordEscapesTheRegistry(t *testing.T) {
	m := newTestModel(t)
	declared := map[string]bool{}
	for _, section := range m.keys.HelpSections() {
		for _, binding := range section.Bindings {
			for _, chord := range binding.Keys() {
				declared[chord] = true
			}
		}
	}

	literal := regexp.MustCompile(`"((?:ctrl|alt)\+[a-z0-9]+)"`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "keys.go" {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for lineNumber, line := range strings.Split(string(body), "\n") {
			for _, match := range literal.FindAllStringSubmatch(line, -1) {
				if !declared[match[1]] {
					t.Errorf("%s:%d handles %q, which no key.Binding declares — it cannot reach /help or the collision check",
						name, lineNumber+1, match[1])
				}
			}
		}
	}
}
