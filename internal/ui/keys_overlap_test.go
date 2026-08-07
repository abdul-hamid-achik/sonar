package ui

import (
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// The dictation key has to be unclaimed, and "unclaimed" has three parts that
// are checked in three different places.
//
// This is the test alt+v needed and did not have. That binding was in the
// keymap, matched nothing else, and still never fired — because the claim that
// mattered was made by the terminal, one layer below anything Go can see. The
// checks below are the ones that CAN be made here, and the fourth is why the
// primary key is a control chord at all.
func TestTheDictationKeyIsUnclaimed(t *testing.T) {
	m := newTestModel(t)
	press := ctrlKey('g')

	// 1. It reaches dictation.
	if !key.Matches(press, m.keys.VoiceInput) {
		t.Fatalf("the dictation binding does not match %q", press.String())
	}

	// 2. Nothing else in the keymap answers to it. Several keys here are
	// deliberately shared between bindings that cannot both be live — enter is
	// Send and CompleteSelect, up is CompleteUp and HistoryUp — so this is
	// asked of one key rather than asserted over the whole map: dictation has no
	// state to share with.
	keymap := reflect.ValueOf(m.keys)
	for index := 0; index < keymap.NumField(); index++ {
		field := keymap.Type().Field(index)
		binding, ok := keymap.Field(index).Interface().(key.Binding)
		if !ok || field.Name == "VoiceInput" {
			continue
		}
		if key.Matches(press, binding) {
			t.Errorf("%q also triggers %s", press.String(), field.Name)
		}
	}

	// 3. The composer does not claim it, and the composer is the claim a keymap
	// review misses: those bindings live in another module and are not in this
	// file to read. Measured against Bubbles v2, the textarea already answers to
	// ctrl+a, ctrl+b, ctrl+d, ctrl+e, ctrl+f, ctrl+h, ctrl+k, ctrl+m, ctrl+n,
	// ctrl+p, ctrl+t, ctrl+u, ctrl+v and ctrl+w — most of what a review of this
	// file alone would call free.
	composer := textarea.New()
	composerKeys := reflect.ValueOf(composer.KeyMap)
	claimed := 0
	for index := 0; index < composerKeys.NumField(); index++ {
		binding, ok := composerKeys.Field(index).Interface().(key.Binding)
		if !ok {
			continue
		}
		claimed += len(binding.Keys())
		if key.Matches(press, binding) {
			t.Errorf("the composer claims %q for %s", press.String(), composerKeys.Type().Field(index).Name)
		}
	}
	if claimed == 0 {
		t.Fatal("read no bindings off the composer; this check proves nothing")
	}

	// 4. And it is not an alt chord, which is the claim that cannot be seen from
	// here at all: on a stock macOS terminal Option is not Meta, so Option+V
	// composes "√" and types it into the draft. The binding existed, matched,
	// and never ran.
	keys := m.keys.VoiceInput.Keys()
	if len(keys) == 0 || strings.HasPrefix(keys[0], "alt+") {
		t.Fatalf("the primary dictation key is an alt chord a stock macOS terminal composes away: %q", keys)
	}
}

// An unhandled key that carries text is inserted as text. That is what made
// alt+v's failure silent rather than loud, so the replacement is checked
// against the same behaviour.
func TestTheDictationKeyIsNotTypedIntoTheDraft(t *testing.T) {
	composer := textarea.New()
	composer.Focus()
	var cmd tea.Cmd
	composer, cmd = composer.Update(ctrlKey('g'))
	_ = cmd
	if got := composer.Value(); strings.TrimSpace(got) != "" {
		t.Fatalf("the composer inserted the dictation key as text: %q", got)
	}
}
