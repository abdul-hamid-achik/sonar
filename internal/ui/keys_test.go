package ui

import (
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
)

// Every binding on KeyMap must appear in exactly one help section. Adding a
// key and forgetting to document it is otherwise invisible: the overlay simply
// never mentions it, and reflection is the only way to notice.
func TestEveryKeyBindingIsDocumentedInExactlyOneSection(t *testing.T) {
	keys := DefaultKeyMap()

	documented := map[string]int{}
	for _, section := range keys.HelpSections() {
		if strings.TrimSpace(section.Title) == "" {
			t.Fatal("help section has no title")
		}
		for _, binding := range section.Bindings {
			documented[strings.Join(binding.Keys(), ",")]++
		}
	}

	value := reflect.ValueOf(keys)
	bindingType := reflect.TypeOf(key.Binding{})
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		if !field.IsExported() || value.Field(i).Type() != bindingType {
			continue
		}
		binding, ok := value.Field(i).Interface().(key.Binding)
		if !ok {
			continue
		}
		id := strings.Join(binding.Keys(), ",")
		switch documented[id] {
		case 1:
			// Documented once, as required.
		case 0:
			t.Errorf("binding %s (%s) is not in any help section", field.Name, id)
		default:
			t.Errorf("binding %s (%s) appears in %d help sections", field.Name, id, documented[id])
		}
	}
}

// FullHelp must stay derived from HelpSections so the Bubbles help surface and
// the overlay cannot describe different key sets.
func TestFullHelpMirrorsHelpSections(t *testing.T) {
	keys := DefaultKeyMap()
	sections := keys.HelpSections()
	groups := keys.FullHelp()
	if len(groups) != len(sections) {
		t.Fatalf("FullHelp has %d groups, HelpSections has %d", len(groups), len(sections))
	}
	for i := range sections {
		if len(groups[i]) != len(sections[i].Bindings) {
			t.Fatalf("group %d (%s): %d bindings vs %d", i, sections[i].Title, len(groups[i]), len(sections[i].Bindings))
		}
	}
}
