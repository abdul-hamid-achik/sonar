package tools

import (
	"strings"
	"testing"
)

func TestGrepToolDef(t *testing.T) {
	tool := GrepToolDef()

	if tool.Name != "grep" {
		t.Errorf("Name = %q, want %q", tool.Name, "grep")
	}
	if tool.Description == "" {
		t.Error("Description should not be empty")
	}
	if tool.Parameters == nil {
		t.Error("Parameters should not be nil")
	}
}

func TestReadToolDef(t *testing.T) {
	tool := ReadToolDef()

	if tool.Name != "read" {
		t.Errorf("Name = %q, want %q", tool.Name, "read")
	}
	props := tool.Parameters["properties"].(map[string]any)
	if _, ok := props["path"]; !ok {
		t.Error("should have path property")
	}
}

func TestWriteToolDef(t *testing.T) {
	tool := WriteToolDef()

	if tool.Name != "write" {
		t.Errorf("Name = %q, want %q", tool.Name, "write")
	}
	props := tool.Parameters["properties"].(map[string]any)
	if _, ok := props["path"]; !ok {
		t.Error("should have path property")
	}
	if _, ok := props["content"]; !ok {
		t.Error("should have content property")
	}
}

func TestGlobToolDef(t *testing.T) {
	tool := GlobToolDef()

	if tool.Name != "glob" {
		t.Errorf("Name = %q, want %q", tool.Name, "glob")
	}
}

func TestBashToolDef(t *testing.T) {
	tool := BashToolDef()

	if tool.Name != "bash" {
		t.Errorf("Name = %q, want %q", tool.Name, "bash")
	}
	props := tool.Parameters["properties"].(map[string]any)
	if _, ok := props["command"]; !ok {
		t.Error("should have command property")
	}
	timeout, ok := props["timeout"].(map[string]any)
	if !ok {
		t.Fatal("should have timeout property")
	}
	if got, ok := timeout["minimum"].(int); !ok || got != BashTimeoutMinSecs {
		t.Fatalf("timeout.minimum = %#v, want %d", timeout["minimum"], BashTimeoutMinSecs)
	}
	if got, ok := timeout["maximum"].(int); !ok || got != BashTimeoutMaxSecs {
		t.Fatalf("timeout.maximum = %#v, want %d", timeout["maximum"], BashTimeoutMaxSecs)
	}
	desc, _ := timeout["description"].(string)
	for _, want := range []string{"1–120", "tools.timeout", "OUTCOME UNKNOWN", "background"} {
		if !strings.Contains(desc, want) {
			t.Errorf("timeout description omitted %q: %s", want, desc)
		}
	}
	if !strings.Contains(tool.Description, "background") || !strings.Contains(tool.Description, "sleep") {
		t.Errorf("bash description should discourage foreground sleep fills: %s", tool.Description)
	}
}

func TestBashTimeoutBoundsAreTheOnlyTimeoutContract(t *testing.T) {
	// Sweep: bash is the only built-in that advertises a timeout argument.
	// Anything else claiming one without sharing BashTimeoutMaxSecs would
	// recreate the silent-clamp footgun.
	for _, def := range AllToolDefs() {
		props, _ := def.Parameters["properties"].(map[string]any)
		if props == nil {
			continue
		}
		timeout, ok := props["timeout"]
		if !ok {
			continue
		}
		if def.Name != "bash" {
			t.Fatalf("%s advertises timeout; only bash is wired to host clamping", def.Name)
		}
		spec, ok := timeout.(map[string]any)
		if !ok {
			t.Fatalf("bash timeout property has unexpected shape %#v", timeout)
		}
		if got, _ := spec["maximum"].(int); got != BashTimeoutMaxSecs {
			t.Fatalf("bash timeout.maximum = %v, want shared BashTimeoutMaxSecs=%d", spec["maximum"], BashTimeoutMaxSecs)
		}
	}
}

func TestLsToolDef(t *testing.T) {
	tool := LsToolDef()

	if tool.Name != "ls" {
		t.Errorf("Name = %q, want %q", tool.Name, "ls")
	}
}

func TestFindToolDef(t *testing.T) {
	tool := FindToolDef()

	if tool.Name != "find" {
		t.Errorf("Name = %q, want %q", tool.Name, "find")
	}
	props := tool.Parameters["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Error("should have name property")
	}
}

func TestDiffToolDef(t *testing.T) {
	tool := DiffToolDef()

	if tool.Name != "diff" {
		t.Errorf("Name = %q, want %q", tool.Name, "diff")
	}
}

func TestEditToolDef(t *testing.T) {
	tool := EditToolDef()

	if tool.Name != "edit" {
		t.Errorf("Name = %q, want %q", tool.Name, "edit")
	}
}

func TestMkdirToolDef(t *testing.T) {
	tool := MkdirToolDef()

	if tool.Name != "mkdir" {
		t.Errorf("Name = %q, want %q", tool.Name, "mkdir")
	}
}

func TestRemoveToolDef(t *testing.T) {
	tool := RemoveToolDef()

	if tool.Name != "remove" {
		t.Errorf("Name = %q, want %q", tool.Name, "remove")
	}
	props := tool.Parameters["properties"].(map[string]any)
	if _, ok := props["recursive"]; !ok {
		t.Error("should have recursive property")
	}
	if _, ok := props["force"]; !ok {
		t.Error("should have force property")
	}
}

func TestCopyToolDef(t *testing.T) {
	tool := CopyToolDef()

	if tool.Name != "copy" {
		t.Errorf("Name = %q, want %q", tool.Name, "copy")
	}
	props := tool.Parameters["properties"].(map[string]any)
	if _, ok := props["source"]; !ok {
		t.Error("should have source property")
	}
	if _, ok := props["destination"]; !ok {
		t.Error("should have destination property")
	}
}

func TestMoveToolDef(t *testing.T) {
	tool := MoveToolDef()

	if tool.Name != "move" {
		t.Errorf("Name = %q, want %q", tool.Name, "move")
	}
}

func TestExistsToolDef(t *testing.T) {
	tool := ExistsToolDef()

	if tool.Name != "exists" {
		t.Errorf("Name = %q, want %q", tool.Name, "exists")
	}
}

func TestLoadSkillToolDef(t *testing.T) {
	tool := LoadSkillToolDef()
	if tool.Name != "load_skill" {
		t.Fatalf("Name = %q", tool.Name)
	}
	if !IsBuiltinTool(tool.Name) {
		t.Fatal("load_skill is not classified as built-in")
	}
	properties, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v", tool.Parameters["properties"])
	}
	if _, ok := properties["name"]; !ok {
		t.Fatal("name property missing")
	}
	required, ok := tool.Parameters["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "name" {
		t.Fatalf("required = %#v", tool.Parameters["required"])
	}
	if additional, ok := tool.Parameters["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("additionalProperties = %#v", tool.Parameters["additionalProperties"])
	}
}

func TestConsultExpertsToolDef(t *testing.T) {
	tool := ConsultExpertsToolDef()
	if tool.Name != "consult_experts" {
		t.Fatalf("Name = %q", tool.Name)
	}
	if !IsBuiltinTool(tool.Name) {
		t.Fatal("consult_experts is not classified as built-in")
	}
	if !strings.Contains(tool.Description, "omit experts") || !strings.Contains(tool.Description, "never invent personas") ||
		!strings.Contains(tool.Description, "architect, critic, explorer, generalist, and verifier") {
		t.Fatalf("tool description lacks exact-profile safety guidance: %q", tool.Description)
	}
	properties, ok := tool.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v", tool.Parameters["properties"])
	}
	strategy, ok := properties["strategy"].(map[string]any)
	if !ok {
		t.Fatalf("strategy = %#v", properties["strategy"])
	}
	values, ok := strategy["enum"].([]string)
	if !ok || len(values) != 3 || values[0] != "team" || values[1] != "swarm" || values[2] != "moe" {
		t.Fatalf("strategy enum = %#v", strategy["enum"])
	}
	experts, ok := properties["experts"].(map[string]any)
	if !ok || experts["maxItems"] != 16 {
		t.Fatalf("experts schema = %#v", properties["experts"])
	}
	if description, _ := experts["description"].(string); !strings.Contains(description, "Omit this field") ||
		!strings.Contains(description, "never invent") || !strings.Contains(description, "architect, critic, explorer, generalist, and verifier") {
		t.Fatalf("experts schema lacks correction guidance: %#v", experts)
	}
	model, ok := properties["model"].(map[string]any)
	if !ok || model["type"] != "string" || model["minLength"] != 1 || model["maxLength"] != 256 {
		t.Fatalf("model schema = %#v", properties["model"])
	}
	overrides, ok := properties["model_overrides"].(map[string]any)
	if !ok || overrides["type"] != "array" || overrides["maxItems"] != 16 {
		t.Fatalf("model_overrides schema = %#v", properties["model_overrides"])
	}
	assignment, ok := overrides["items"].(map[string]any)
	if !ok {
		t.Fatalf("model override item = %#v", overrides["items"])
	}
	assignmentProperties, ok := assignment["properties"].(map[string]any)
	if !ok || len(assignmentProperties) != 2 {
		t.Fatalf("model override properties = %#v", assignment["properties"])
	}
	for name, limit := range map[string]int{"expert": 128, "model": 256} {
		property, ok := assignmentProperties[name].(map[string]any)
		if !ok || property["minLength"] != 1 || property["maxLength"] != limit {
			t.Fatalf("model override %s property = %#v", name, assignmentProperties[name])
		}
	}
	if additional, ok := assignment["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("model override additionalProperties = %#v", assignment["additionalProperties"])
	}
	assignmentRequired, ok := assignment["required"].([]string)
	if !ok || len(assignmentRequired) != 2 || assignmentRequired[0] != "expert" || assignmentRequired[1] != "model" {
		t.Fatalf("model override required = %#v", assignment["required"])
	}
	required, ok := tool.Parameters["required"].([]string)
	if !ok || len(required) != 2 || required[0] != "strategy" || required[1] != "objective" {
		t.Fatalf("required = %#v", tool.Parameters["required"])
	}
	if additional, ok := tool.Parameters["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("additionalProperties = %#v", tool.Parameters["additionalProperties"])
	}
}
