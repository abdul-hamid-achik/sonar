package command

import "testing"

// /voice grew from one verb to six, and every one of them is a different kind
// of thing: a microphone toggle, a session switch, a screen, three reports, and
// a per-channel setting. Nothing checked that they land where they claim.
//
// The shape that makes this worth pinning is the fall-through: an unrecognised
// first word is read as a CHANNEL NAME, so a new verb added above without a
// case here does not error — it silently becomes "unknown channel". A typo in
// the parser would be reported to the user as their own typo.
func TestVoiceCommandRoutesEveryForm(t *testing.T) {
	registry := NewRegistry()
	RegisterBuiltins(registry)

	for _, testCase := range []struct {
		args   []string
		action Action
		data   string
	}{
		{args: nil, action: ActionVoiceInput},
		{args: []string{"on"}, action: ActionVoiceEnable, data: "on"},
		{args: []string{"off"}, action: ActionVoiceEnable, data: "off"},
		{args: []string{"view"}, action: ActionVoiceStage},
		{args: []string{"stage"}, action: ActionVoiceStage},
		{args: []string{"status"}, action: ActionVoiceStatus},
		{args: []string{"doctor"}, action: ActionVoiceStatus},
		{args: []string{"voices"}, action: ActionVoiceVoices},
		{args: []string{"test"}, action: ActionVoiceTest},
		{args: []string{"provider", "openai"}, action: ActionVoiceSetting, data: "provider openai"},
		{args: []string{"speak_when", "unfocused"}, action: ActionVoiceSetting, data: "speak_when unfocused"},
		{args: []string{"rate", "195"}, action: ActionVoiceSetting, data: "rate 195"},
		{args: []string{"voice", "es", "Paulina"}, action: ActionVoiceSetting, data: "voice es Paulina"},
		{args: []string{"pronounce", "deploy", "dipló"}, action: ActionVoiceSetting, data: "pronounce deploy dipló"},
		// A setting with no value still routes: the UI answers with its usage
		// rather than the parser guessing what was meant.
		{args: []string{"pronounce"}, action: ActionVoiceSetting, data: "pronounce"},
		{args: []string{"answer", "off"}, action: ActionVoiceChannel, data: "answer off"},
		{args: []string{"alerts", "on"}, action: ActionVoiceChannel, data: "alerts on"},
		// Case is the user's, not the parser's.
		{args: []string{"ON"}, action: ActionVoiceEnable, data: "on"},
	} {
		result := runVoice(t, registry, testCase.args)
		if result.Error != "" {
			t.Errorf("/voice %v errored: %s", testCase.args, result.Error)
			continue
		}
		if result.Action != testCase.action {
			t.Errorf("/voice %v -> action %d, want %d", testCase.args, result.Action, testCase.action)
		}
		if testCase.data != "" && result.Data != testCase.data {
			t.Errorf("/voice %v -> data %q, want %q", testCase.args, result.Data, testCase.data)
		}
	}

	// A channel name with no state is a usage error rather than a silent
	// no-action, because "/voice answer" reads like it should do something.
	if result := runVoice(t, registry, []string{"answer"}); result.Error == "" {
		t.Errorf("/voice answer produced no error: %+v", result)
	}
	if result := runVoice(t, registry, []string{"answer", "maybe"}); result.Error == "" {
		t.Errorf("/voice answer maybe produced no error: %+v", result)
	}
}

func runVoice(t *testing.T, registry *Registry, args []string) Result {
	t.Helper()
	return registry.Execute(&Context{}, "voice", args)
}
