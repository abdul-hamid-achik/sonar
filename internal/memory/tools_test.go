package memory

import "testing"

func TestBuiltinToolDefs(t *testing.T) {
	defs := BuiltinToolDefs()
	if len(defs) != 5 {
		t.Fatalf("BuiltinToolDefs() returned %d defs, want 5", len(defs))
	}

	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}

	expected := []string{"memory_save", "memory_recall", "memory_delete", "memory_update", "memory_list"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("missing %s tool definition", name)
		}
	}
}

func TestIsBuiltinTool(t *testing.T) {
	tests := []struct {
		name string
		tool string
		want bool
	}{
		{name: "memory_save", tool: "memory_save", want: true},
		{name: "memory_recall", tool: "memory_recall", want: true},
		{name: "memory_delete", tool: "memory_delete", want: true},
		{name: "memory_update", tool: "memory_update", want: true},
		{name: "memory_list", tool: "memory_list", want: true},
		{name: "unknown tool", tool: "unknown", want: false},
		{name: "empty string", tool: "", want: false},
		{name: "partial match", tool: "memory_", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsBuiltinTool(tt.tool)
			if got != tt.want {
				t.Errorf("IsBuiltinTool(%q) = %v, want %v", tt.tool, got, tt.want)
			}
		})
	}
}
