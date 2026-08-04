package ui

import (
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"

	"github.com/abdul-hamid-achik/sonar/internal/command"
)

func TestUIActionRegistryKeepsStableOrderAcrossReplacement(t *testing.T) {
	t.Parallel()

	first := UIActionSpec{
		ID:       command.ActionID("viewer.copy"),
		Label:    "Copy visible page",
		Shortcut: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
	}.Resolve(EntityRef{}, true, "")
	second := UIActionSpec{
		ID:       command.ActionID("viewer.search"),
		Label:    "Search output",
		Shortcut: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
	}.Resolve(EntityRef{}, true, "")

	registry := NewUIActionRegistry(first, second)
	replacement := first
	replacement.Label = "Copy current page"
	replacement.Enabled = false
	replacement.Reason = "Nothing visible."
	if !registry.Register(replacement) {
		t.Fatal("valid replacement was rejected")
	}

	actions := registry.Actions()
	if len(actions) != 2 {
		t.Fatalf("action count = %d, want 2", len(actions))
	}
	if actions[0].ID != first.ID || actions[1].ID != second.ID {
		t.Fatalf("replacement reordered actions: %#v", []command.ActionID{actions[0].ID, actions[1].ID})
	}
	if actions[0].Label != "Copy current page" || actions[0].Enabled ||
		actions[0].Reason != "Nothing visible." {
		t.Fatalf("replacement state = %#v", actions[0])
	}
}

func TestUIActionRegistryIsolatesKeyBindingCopies(t *testing.T) {
	t.Parallel()

	binding := key.NewBinding(
		key.WithKeys("enter", "o"),
		key.WithHelp("enter", "open"),
	)
	registry := NewUIActionRegistry(UIAction{
		ID:       command.ActionID("viewer.open"),
		Label:    "Open",
		Shortcut: binding,
		Enabled:  true,
		Target: EntityRef{
			Kind:    EntityKindTranscriptBlock,
			BlockID: BlockID("blk_1"),
		},
	})

	binding.Keys()[0] = "mutated-before-read"
	first, ok := registry.Action(command.ActionID("viewer.open"))
	if !ok {
		t.Fatal("registered action missing")
	}
	if first.Shortcut.Keys()[0] != "enter" {
		t.Fatalf("registry retained caller binding storage: %q", first.Shortcut.Keys()[0])
	}

	first.Shortcut.Keys()[0] = "mutated-after-read"
	first.Shortcut.SetHelp("x", "changed")
	second, _ := registry.Action(command.ActionID("viewer.open"))
	if second.Shortcut.Keys()[0] != "enter" || second.Shortcut.Help().Key != "enter" {
		t.Fatalf("returned action mutated registry: keys=%v help=%#v", second.Shortcut.Keys(), second.Shortcut.Help())
	}
}

func TestUIActionRegistryNormalizesPresentationAndFailsClosed(t *testing.T) {
	t.Parallel()

	registry := &UIActionRegistry{}
	if registry.Register(UIAction{Label: "missing id", Enabled: true}) {
		t.Fatal("action without ID was accepted")
	}
	if registry.Register(UIAction{
		ID:      command.ActionID("bad id"),
		Label:   "invalid identity",
		Enabled: true,
	}) {
		t.Fatal("action with invalid ID was accepted")
	}
	if registry.Register(UIAction{
		ID:       command.ActionID("viewer.bad-key"),
		Label:    "Invalid shortcut",
		Shortcut: key.NewBinding(key.WithKeys("x\nescape")),
		Enabled:  true,
	}) {
		t.Fatal("action with row-breaking shortcut was accepted")
	}

	disabled := UIAction{
		ID:    command.ActionID("viewer.disabled"),
		Label: "\x1b[31mOpen\noutput",
		Shortcut: key.NewBinding(
			key.WithKeys("o"),
			key.WithHelp("\x1b[31mo\npen", strings.Repeat("description ", 40)),
		),
		Enabled: false,
		Reason:  "\x1b[33m",
	}
	if !registry.Register(disabled) {
		t.Fatal("sanitizable disabled action was rejected")
	}
	got, _ := registry.Action(disabled.ID)
	if got.Label != "Open output" {
		t.Fatalf("sanitized label = %q", got.Label)
	}
	if got.Reason != uiActionUnavailableReason {
		t.Fatalf("empty sanitized reason = %q", got.Reason)
	}
	if strings.Contains(got.Label+got.Reason, "\x1b") {
		t.Fatal("terminal control crossed action registry")
	}
	if got.Shortcut.Help().Key != "o pen" ||
		len([]rune(got.Shortcut.Help().Desc)) > maxUIActionHelpDesc {
		t.Fatalf("help was not bounded/sanitized: %#v", got.Shortcut.Help())
	}

	enabled := disabled
	enabled.Enabled = true
	enabled.Reason = "stale disabled reason"
	if !registry.Register(enabled) {
		t.Fatal("enabled replacement was rejected")
	}
	got, _ = registry.Action(enabled.ID)
	if got.Reason != "" {
		t.Fatalf("enabled action retained disabled reason %q", got.Reason)
	}
}

func TestUIActionRegistryResolveRequestMatrix(t *testing.T) {
	t.Parallel()

	target := EntityRef{
		Kind:         EntityKindToolInvocation,
		BlockID:      BlockID("blk_tool"),
		InvocationID: "call_1",
	}
	enabled := UIAction{
		ID:      command.ActionID("tool.open-output"),
		Label:   "Open output",
		Enabled: true,
		Target:  target,
	}
	disabled := UIAction{
		ID:      command.ActionID("tool.copy-output"),
		Label:   "Copy output",
		Enabled: false,
		Reason:  "Output is still loading.",
		Target:  target,
	}
	registry := NewUIActionRegistry(enabled, disabled)

	tests := []struct {
		name       string
		request    UIActionRequest
		wantOK     bool
		wantReason string
	}{
		{
			name:    "keyboard",
			request: enabled.Request(UIActionSourceKeyboard),
			wantOK:  true,
		},
		{
			name:    "mouse",
			request: enabled.Request(UIActionSourceMouse),
			wantOK:  true,
		},
		{
			name: "disabled",
			request: UIActionRequest{
				ActionID: disabled.ID,
				Target:   target,
				Source:   UIActionSourceKeyboard,
			},
			wantReason: "Output is still loading.",
		},
		{
			name: "unknown",
			request: UIActionRequest{
				ActionID: command.ActionID("tool.unknown"),
				Target:   target,
				Source:   UIActionSourceKeyboard,
			},
			wantReason: uiActionMissingReason,
		},
		{
			name: "stale target",
			request: UIActionRequest{
				ActionID: enabled.ID,
				Target: EntityRef{
					Kind:         EntityKindToolInvocation,
					InvocationID: "call_2",
				},
				Source: UIActionSourceKeyboard,
			},
			wantReason: uiActionTargetReason,
		},
		{
			name: "invalid source",
			request: UIActionRequest{
				ActionID: enabled.ID,
				Target:   target,
			},
			wantReason: uiActionSourceReason,
		},
		{
			name: "malformed identity",
			request: UIActionRequest{
				ActionID: command.ActionID("bad id"),
				Target:   target,
				Source:   UIActionSourceMouse,
			},
			wantReason: uiActionMissingReason,
		},
		{
			name: "malformed target",
			request: UIActionRequest{
				ActionID: enabled.ID,
				Target: EntityRef{
					Kind:         EntityKindToolInvocation,
					InvocationID: "\x1b[31mcall",
				},
				Source: UIActionSourceMouse,
			},
			wantReason: uiActionMissingReason,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			action, reason, ok := registry.ResolveRequest(test.request)
			if ok != test.wantOK || reason != test.wantReason {
				t.Fatalf("ResolveRequest() = (%#v, %q, %v), want ok=%v reason=%q",
					action, reason, ok, test.wantOK, test.wantReason)
			}
			if ok && (action.ID != enabled.ID || action.Target != target) {
				t.Fatalf("accepted action = %#v", action)
			}
		})
	}
}

func TestEntityRefContainsOnlyScalarFieldsAndValidatesCategory(t *testing.T) {
	t.Parallel()

	entityType := reflect.TypeOf(EntityRef{})
	for index := range entityType.NumField() {
		field := entityType.Field(index)
		switch field.Type.Kind() {
		case reflect.Int, reflect.Uint8, reflect.String:
			// Scalar aliases are the entire admitted boundary.
		default:
			t.Errorf("EntityRef.%s has non-scalar type %s", field.Name, field.Type)
		}
	}

	valid := []EntityRef{
		{},
		{Kind: EntityKindTranscriptBlock, BlockID: BlockID("blk_1")},
		{Kind: EntityKindToolInvocation, InvocationID: "call_1"},
		{Kind: EntityKindAgentInvocation, InvocationID: "agent_1"},
		{Kind: EntityKindOverlay, OverlayID: OverlayID("output_1")},
		{Kind: EntityKindDiffLocation, BlockID: BlockID("blk_diff"), FileIndex: 1, HunkIndex: 2, LineIndex: 3},
	}
	for _, ref := range valid {
		if !ref.Valid() {
			t.Errorf("valid ref rejected: %#v", ref)
		}
	}

	invalid := []EntityRef{
		{Kind: EntityKind(255)},
		{Kind: EntityKindNone, BlockID: BlockID("blk_1")},
		{Kind: EntityKindTranscriptBlock},
		{Kind: EntityKindToolInvocation, InvocationID: "call id"},
		{Kind: EntityKindAgentInvocation},
		{Kind: EntityKindOverlay, OverlayID: OverlayID("../output")},
		{Kind: EntityKindDiffLocation, BlockID: BlockID("blk_diff"), FileIndex: -1},
	}
	for _, ref := range invalid {
		if ref.Valid() {
			t.Errorf("invalid ref accepted: %#v", ref)
		}
	}
}

func TestHitRegionSetGeometryExhaustive(t *testing.T) {
	t.Parallel()

	actionID := command.ActionID("viewer.open")
	target := EntityRef{}
	for originX := -2; originX <= 2; originX++ {
		for originY := -2; originY <= 2; originY++ {
			for width := 0; width <= 10; width++ {
				for height := 0; height <= 10; height++ {
					var set HitRegionSet
					rect := NewCellRect(originX, originY, originX+width, originY+height)
					added := set.Add(rect, 0, actionID, target)
					wantAdded := width > 0 && height > 0
					if added != wantAdded {
						t.Fatalf("Add(%#v) = %v, want %v", rect, added, wantAdded)
					}
					for y := originY - 1; y <= originY+height; y++ {
						for x := originX - 1; x <= originX+width; x++ {
							request, hit := set.Hit(x, y)
							wantHit := wantAdded &&
								x >= originX && x < originX+width &&
								y >= originY && y < originY+height
							if hit != wantHit {
								t.Fatalf("rect=%#v cell=(%d,%d) hit=%v want=%v", rect, x, y, hit, wantHit)
							}
							if hit && (request.ActionID != actionID || request.Target != target ||
								request.Source != UIActionSourceMouse) {
								t.Fatalf("pointer request = %#v", request)
							}
						}
					}
				}
			}
		}
	}
}

func TestHitRegionSetResolvesZThenPaintOrder(t *testing.T) {
	t.Parallel()

	var set HitRegionSet
	rect := NewCellRect(4, 6, 12, 10)
	target := EntityRef{}
	if !set.Add(rect, 1, command.ActionID("low"), target) ||
		!set.Add(rect, 3, command.ActionID("high-first"), target) ||
		!set.Add(rect, 3, command.ActionID("high-last"), target) {
		t.Fatal("valid overlapping region rejected")
	}
	request, ok := set.Hit(7, 8)
	if !ok || request.ActionID != command.ActionID("high-last") {
		t.Fatalf("top hit = %#v, %v", request, ok)
	}

	regions := set.Regions()
	if len(regions) != 3 || regions[0].Order() != 1 ||
		regions[1].Order() != 2 || regions[2].Order() != 3 {
		t.Fatalf("paint orders = %#v", regions)
	}
	regions[2].ActionID = command.ActionID("mutated")
	request, _ = set.Hit(7, 8)
	if request.ActionID != command.ActionID("high-last") {
		t.Fatal("Regions exposed mutable stack storage")
	}

	set.Reset()
	if len(set.Regions()) != 0 {
		t.Fatal("Reset retained regions")
	}
	if set.Add(rect, 0, command.ActionID("fresh"), target) && set.Regions()[0].Order() != 1 {
		t.Fatal("Reset did not restart deterministic paint order")
	}
}

func TestKeyboardAndPointerUseSameActionRequestShape(t *testing.T) {
	t.Parallel()

	action := UIAction{
		ID:      command.ActionID("viewer.copy"),
		Label:   "Copy",
		Enabled: true,
		Target: EntityRef{
			Kind:    EntityKindTranscriptBlock,
			BlockID: BlockID("blk_copy"),
		},
	}
	keyboard := action.Request(UIActionSourceKeyboard)

	var set HitRegionSet
	if !set.Add(NewCellRect(2, 3, 8, 4), 0, action.ID, action.Target) {
		t.Fatal("pointer region rejected")
	}
	pointer, ok := set.Hit(2, 3)
	if !ok {
		t.Fatal("pointer action not hit")
	}
	if keyboard.ActionID != pointer.ActionID || keyboard.Target != pointer.Target {
		t.Fatalf("request paths diverged: keyboard=%#v pointer=%#v", keyboard, pointer)
	}
	if keyboard.Source != UIActionSourceKeyboard || pointer.Source != UIActionSourceMouse {
		t.Fatalf("request sources = %v, %v", keyboard.Source, pointer.Source)
	}
}
