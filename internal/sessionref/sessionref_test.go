package sessionref

import (
	"strings"
	"testing"
)

func TestNewAndValid(t *testing.T) {
	seen := make(map[string]struct{}, 32)
	for i := 0; i < 32; i++ {
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !Valid(id) {
			t.Fatalf("New() = %q is not Valid", id)
		}
		if Format(id) != id {
			t.Fatalf("Format(%q) = %q", id, Format(id))
		}
		if _, dup := seen[id]; dup {
			// Extremely unlikely with 28 bits across 32 draws; still fail closed.
			t.Fatalf("duplicate public id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestParse(t *testing.T) {
	id, err := Parse("A1b2C3d")
	if err != nil || id != "a1b2c3d" {
		t.Fatalf("Parse mixed case = %q, %v; want a1b2c3d", id, err)
	}
	if _, err := Parse("  a1b2c3d  "); err != nil {
		t.Fatalf("Parse trimmed: %v", err)
	}
	for _, value := range []string{
		"", "S7", "s7", "7", "42", "S42", "0", "a1b2c3", "a1b2c3de",
		"g1b2c3d", " session ", "session-7", "a1b2c3D!",
	} {
		if got, err := Parse(value); err == nil || got != "" {
			t.Fatalf("Parse(%q) = %q, %v; want invalid", value, got, err)
		}
	}
}

func TestFormatRejectsInvalid(t *testing.T) {
	if Format("S7") != "" || Format("42") != "" || Format(strings.Repeat("a", Length+1)) != "" {
		t.Fatal("Format should reject non-handles")
	}
}
