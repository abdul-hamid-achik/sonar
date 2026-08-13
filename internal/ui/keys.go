package ui

import "charm.land/bubbles/v2/key"

// KeyMap defines all keyboard shortcuts for the application.
type KeyMap struct {
	Send              key.Binding
	NewLine           key.Binding
	Cancel            key.Binding
	Quit              key.Binding
	ClearView         key.Binding
	NewConvo          key.Binding
	Help              key.Binding
	ToggleTools       key.Binding
	PageUp            key.Binding
	PageDown          key.Binding
	HalfPageUp        key.Binding
	HalfPageDn        key.Binding
	JumpLatest        key.Binding
	Complete          key.Binding
	CompleteUp        key.Binding
	CompleteDown      key.Binding
	CompleteToggle    key.Binding
	CompleteSelect    key.Binding
	CopyLast          key.Binding
	ToggleMouse       key.Binding
	Paste             key.Binding
	InspectOutput     key.Binding
	InspectDiff       key.Binding
	CycleMode         key.Binding
	ModelPicker       key.Binding
	SettingsPicker    key.Binding
	TranscriptSearch  key.Binding
	HistoryUp         key.Binding
	HistoryDown       key.Binding
	ToggleFocusedTool key.Binding
	VoiceInput        key.Binding
	ToggleThinking    key.Binding
	CompactToggle     key.Binding
	ExternalEditor    key.Binding
}

// DefaultKeyMap returns the default keybindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Send: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "send / queue one follow-up"),
		),
		NewLine: key.NewBinding(
			// Ctrl+J works on terminals that cannot distinguish Shift+Enter from
			// Enter; Alt+Enter is a second ergonomic fallback on enhanced terminals.
			key.WithKeys("shift+enter", "ctrl+j", "alt+enter"),
			key.WithHelp("shift+enter", "new line · ctrl+j/alt+enter also"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "cancel / close overlay"),
		),
		// alt+<letter> is this map's namespace for toggles (alt+m mouse capture,
		// alt+t, alt+d). A toggle rather than hold-to-talk because terminals
		// report key RELEASE only under the Kitty protocol — a hold binding
		// would work in Ghostty and do nothing in Terminal.app.
		//
		// The help names /voice as well, and that is not redundancy. On macOS
		// this chord does not exist until the terminal is told that Option means
		// Alt (Terminal.app: "Use Option as Meta key"; Ghostty:
		// macos-option-as-alt = true) — before that Option composes a character
		// and the app is never sent anything. A leader key or a multiplexer can
		// also claim it first. Someone whose terminal eats the chord needs the
		// other way in printed next to it, not in a footnote.
		VoiceInput: key.NewBinding(
			// ctrl+g, and the letter names nothing — which is the point, because
			// every letter that DID name it is claimed by something that eats it.
			//
			// alt+v was the original and it never arrived. On a stock macOS
			// terminal Option is not Meta: Option+V composes "√" and inserts it in
			// the composer, so the binding was invisible, un-actionable, and
			// indistinguishable from dictation being broken. Every alt+<letter>
			// chord in this map has the same problem; the others are all
			// secondary gestures with a menu or a command behind them, and this
			// one was the only way in.
			//
			// So it has to be a control chord, and the free ones are what is left
			// after the claims: ctrl+a and ctrl+b are screen's and tmux's
			// prefixes, ctrl+s and ctrl+q are flow control that can wedge a
			// terminal, ctrl+z suspends, ctrl+w and ctrl+k are readline's
			// word-kill and line-kill, ctrl+x is an editor prefix, and ctrl+r is
			// deliberately kept free elsewhere in this file because it is
			// reverse-history-search everywhere. ctrl+g is the remainder, and
			// nothing wants it: in readline it aborts, which no surface here uses.
			//
			// alt+v stays as a second key for terminals told that Option is Meta.
			// It costs nothing and it is what anyone who learned it will press.
			key.WithKeys("ctrl+g", "alt+v"),
			key.WithHelp("ctrl+g", "open the mic / close and transcribe · /voice also"),
		),
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		ClearView: key.NewBinding(
			key.WithKeys("ctrl+l"),
			key.WithHelp("ctrl+l", "clear screen"),
		),
		NewConvo: key.NewBinding(
			key.WithKeys("ctrl+n"),
			key.WithHelp("ctrl+n", "new conversation"),
		),
		Help: key.NewBinding(
			// F1 is the unambiguous primary gesture. Ctrl+H remains an
			// enhanced-terminal fallback, but legacy terminals may decode the
			// same control byte as Backspace.
			key.WithKeys("f1", "ctrl+h"),
			key.WithHelp("f1", "show help (empty input)"),
		),
		ToggleTools: key.NewBinding(
			// t is the tool; alt is "all of them". ctrl+t toggles the one in
			// focus. These were ctrl+b and ctrl+r — the same operation at two
			// scopes, with two letters that named neither the operation nor
			// each other.
			//
			// The Glyphrun runner rejects alt+<letter> as an unsupported key, so
			// the specs send the raw ESC-prefixed bytes instead. That is what a
			// terminal actually transmits for this chord, which makes the
			// coverage stronger than a press alias would have been.
			key.WithKeys("alt+t"),
			key.WithHelp("alt+t", "toggle all tools (empty input)"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "scroll up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdown", "scroll down"),
		),
		HalfPageUp: key.NewBinding(
			key.WithKeys("ctrl+u"),
			key.WithHelp("ctrl+u", "edit / half page up"),
		),
		HalfPageDn: key.NewBinding(
			key.WithKeys("ctrl+d"),
			key.WithHelp("ctrl+d", "edit / half page down"),
		),
		JumpLatest: key.NewBinding(
			key.WithKeys("end"),
			key.WithHelp("end", "latest output (empty input)"),
		),
		Complete: key.NewBinding(
			key.WithKeys("tab", "ctrl+i"),
			key.WithHelp("tab", "autocomplete"),
		),
		CompleteUp: key.NewBinding(
			key.WithKeys("up"),
			key.WithHelp("up", "previous completion"),
		),
		CompleteDown: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("down", "next completion"),
		),
		CompleteToggle: key.NewBinding(
			key.WithKeys("tab", "ctrl+i"),
			key.WithHelp("tab", "toggle selection"),
		),
		CompleteSelect: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select item"),
		),
		CopyLast: key.NewBinding(
			key.WithKeys("ctrl+y"),
			key.WithHelp("ctrl+y", "copy last response (empty input)"),
		),
		Paste: key.NewBinding(
			key.WithKeys("ctrl+v"),
			key.WithHelp("ctrl+v", "paste text; on macOS convert a clipboard image to PNG and attach it"),
		),
		// Contextual: these act only while a tool receipt is inspected. They
		// were handled by literal msg.String() cases and appeared in no help
		// group at all, so the diff viewer — the thing people most want after
		// an edit — was reachable only by knowing it existed.
		InspectOutput: key.NewBinding(
			key.WithKeys("alt+o"),
			key.WithHelp("alt+o", "open full output (inspected receipt)"),
		),
		InspectDiff: key.NewBinding(
			key.WithKeys("alt+d"),
			key.WithHelp("alt+d", "open full diff (inspected receipt)"),
		),
		ToggleMouse: key.NewBinding(
			key.WithKeys("alt+m"),
			// /mouse is the non-Option path: stock macOS terminals compose µ
			// instead of sending alt+m until Option is configured as Meta.
			key.WithHelp("alt+m", "mouse capture off/on — turn off to select and copy · /mouse also"),
		),
		CycleMode: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "cycle mode (NORMAL/PLAN/AUTO)"),
		),
		ModelPicker: key.NewBinding(
			// Ctrl+M is carriage return in ordinary terminals and therefore
			// indistinguishable from Enter without an enhanced keyboard protocol.
			key.WithKeys("ctrl+o"),
			key.WithHelp("ctrl+o", "open model picker"),
		),
		SettingsPicker: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "open settings"),
		),
		TranscriptSearch: key.NewBinding(
			key.WithKeys("ctrl+f"),
			key.WithHelp("ctrl+f", "search transcript"),
		),
		HistoryUp: key.NewBinding(
			key.WithKeys("up"),
			key.WithHelp("↑", "previous input (empty input)"),
		),
		HistoryDown: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("↓", "next input (history active)"),
		),
		ToggleFocusedTool: key.NewBinding(
			// Freeing ctrl+r matters on its own: it is reverse-history-search in
			// every shell, so binding it here fought muscle memory every day.
			key.WithKeys("ctrl+t"),
			key.WithHelp("ctrl+t", "toggle focused tool (empty input)"),
		),
		ToggleThinking: key.NewBinding(
			// r for reasoning, alt because it is the batch operation. There is no
			// single-receipt counterpart yet; ctrl+r stays free rather than being
			// spent on one.
			key.WithKeys("alt+r"),
			key.WithHelp("alt+r", "toggle all reasoning (empty draft)"),
		),
		CompactToggle: key.NewBinding(
			// c for compact. ctrl+k is kill-line in readline, and k named nothing.
			key.WithKeys("alt+c"),
			key.WithHelp("alt+c", "toggle compact mode"),
		),
		ExternalEditor: key.NewBinding(
			key.WithKeys("ctrl+e"),
			key.WithHelp("ctrl+e", "open in $VISUAL/$EDITOR"),
		),
	}
}

// ShortHelp returns the key groups for the short help view.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Send, k.NewLine, k.Cancel, k.Quit, k.Help}
}

// KeyHelpSection is a titled group of bindings.
//
// The title lives here rather than in the help renderer because this is where
// the grouping decision belongs: a binding added to a section is documented
// under that heading automatically, and a renderer cannot invent a category
// the keymap disagrees with.
type KeyHelpSection struct {
	Title    string
	Bindings []key.Binding
}

// HelpSections groups every binding by the task a reader is trying to do.
//
// The help overlay used to print all twenty-six as one undifferentiated list,
// which meant finding "how do I search the transcript" was a linear scan past
// every editing and paging key. Sections are ordered by how often a reader
// needs them, not alphabetically or by keystroke.
func (k KeyMap) HelpSections() []KeyHelpSection {
	return []KeyHelpSection{
		{"Compose", []key.Binding{
			k.Send, k.NewLine, k.Paste, k.Complete, k.HistoryUp, k.HistoryDown,
			k.ExternalEditor,
		}},
		// Select before Read/Inspect: the default mouse mode blocks terminal
		// drag-select, so the toggle has to be findable without scrolling past
		// paging and tool-expand chords.
		{"Select", []key.Binding{k.ToggleMouse, k.CopyLast}},
		{"Read", []key.Binding{
			k.PageUp, k.PageDown, k.HalfPageUp, k.HalfPageDn, k.JumpLatest, k.TranscriptSearch,
		}},
		{"Inspect", []key.Binding{
			k.ToggleTools, k.ToggleFocusedTool, k.ToggleThinking, k.InspectOutput, k.InspectDiff,
			k.CompactToggle,
		}},
		{"Session", []key.Binding{
			k.CycleMode, k.ModelPicker, k.SettingsPicker, k.NewConvo, k.ClearView, k.Help,
		}},
		// Voice gets its own heading even though it holds one chord today: the
		// microphone key lived under Compose — accurate, dictation is composer
		// input — and nobody looking for voice found it there. The rest of the
		// surface is slash verbs (/voice view, /voice status), which the help
		// overlay documents beside the commands rather than here.
		{"Voice", []key.Binding{k.VoiceInput}},
		{"Leave", []key.Binding{k.Cancel, k.Quit}},
	}
}

// FullHelp satisfies the Bubbles help.KeyMap interface. It is derived from
// HelpSections so the two cannot describe different key surfaces.
func (k KeyMap) FullHelp() [][]key.Binding {
	sections := k.HelpSections()
	groups := make([][]key.Binding, 0, len(sections))
	for _, section := range sections {
		groups = append(groups, section.Bindings)
	}
	return groups
}
