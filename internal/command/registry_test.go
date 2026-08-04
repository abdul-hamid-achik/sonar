package command

import "testing"

func TestRegistry_Register(t *testing.T) {
	r := NewRegistry()
	cmd := &Command{
		Name:        "test",
		Description: "A test command",
		Handler: func(_ *Context, _ []string) Result {
			return Result{Text: "ok"}
		},
	}
	r.Register(cmd)

	all := r.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 command, got %d", len(all))
	}
	if all[0].Name != "test" {
		t.Errorf("command name = %q, want %q", all[0].Name, "test")
	}

	// Execute to verify it was registered correctly
	result := r.Execute(&Context{}, "test", nil)
	if result.Text != "ok" {
		t.Errorf("result text = %q, want %q", result.Text, "ok")
	}
}

func TestRegistry_Execute(t *testing.T) {
	r := NewRegistry()
	called := false
	r.Register(&Command{
		Name: "run",
		Handler: func(_ *Context, _ []string) Result {
			called = true
			return Result{Text: "executed"}
		},
	})

	t.Run("found command executes handler", func(t *testing.T) {
		result := r.Execute(&Context{}, "run", nil)
		if !called {
			t.Error("handler was not called")
		}
		if result.Text != "executed" {
			t.Errorf("result text = %q, want %q", result.Text, "executed")
		}
	})

	t.Run("not found returns error", func(t *testing.T) {
		result := r.Execute(&Context{}, "nonexistent", nil)
		if result.Error == "" {
			t.Error("expected error for unknown command")
		}
	})
}

func TestRegistryExecuteProvidesEmptyContextWhenNil(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name: "nil-safe",
		Handler: func(ctx *Context, _ []string) Result {
			if ctx == nil {
				t.Fatal("handler received a nil context")
			}
			return Result{Text: "ok"}
		},
	})
	if result := r.Execute(nil, "nil-safe", nil); result.Text != "ok" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRegistry_ExecuteByAlias(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "mycommand",
		Aliases: []string{"mc", "m"},
		Handler: func(_ *Context, _ []string) Result {
			return Result{Text: "alias works"}
		},
	})

	tests := []struct {
		name    string
		cmdName string
		wantOk  bool
	}{
		{name: "by name", cmdName: "mycommand", wantOk: true},
		{name: "by alias mc", cmdName: "mc", wantOk: true},
		{name: "by alias m", cmdName: "m", wantOk: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.Execute(&Context{}, tt.cmdName, nil)
			if tt.wantOk && result.Error != "" {
				t.Errorf("unexpected error: %s", result.Error)
			}
			if tt.wantOk && result.Text != "alias works" {
				t.Errorf("result text = %q, want %q", result.Text, "alias works")
			}
		})
	}
}

func TestRegistry_All(t *testing.T) {
	r := NewRegistry()
	names := []string{"alpha", "beta", "gamma"}
	for _, name := range names {
		n := name // capture
		r.Register(&Command{
			Name:    n,
			Handler: func(_ *Context, _ []string) Result { return Result{} },
		})
	}

	all := r.All()
	if len(all) != len(names) {
		t.Fatalf("expected %d commands, got %d", len(names), len(all))
	}
	for i, cmd := range all {
		if cmd.Name != names[i] {
			t.Errorf("All()[%d].Name = %q, want %q", i, cmd.Name, names[i])
		}
	}
}

func TestRegistry_Match(t *testing.T) {
	r := NewRegistry()
	r.Register(&Command{
		Name:    "model",
		Aliases: []string{"m"},
		Handler: func(_ *Context, _ []string) Result { return Result{} },
	})
	r.Register(&Command{
		Name:    "models",
		Aliases: []string{"ml"},
		Handler: func(_ *Context, _ []string) Result { return Result{} },
	})
	r.Register(&Command{
		Name:    "help",
		Handler: func(_ *Context, _ []string) Result { return Result{} },
	})

	tests := []struct {
		name   string
		prefix string
		want   int
	}{
		{name: "prefix mo matches model and models", prefix: "mo", want: 2},
		{name: "prefix model matches model and models", prefix: "model", want: 2},
		{name: "prefix models matches only models", prefix: "models", want: 1},
		{name: "prefix h matches help", prefix: "h", want: 1},
		{name: "no match", prefix: "z", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := r.Match(tt.prefix)
			if len(matches) != tt.want {
				t.Errorf("Match(%q) returned %d results, want %d", tt.prefix, len(matches), tt.want)
			}
		})
	}

	// Verify no duplicates from aliases
	t.Run("aliases dont create dupes", func(t *testing.T) {
		matches := r.Match("model")
		seen := make(map[string]bool)
		for _, m := range matches {
			if seen[m.Name] {
				t.Errorf("duplicate match for %q", m.Name)
			}
			seen[m.Name] = true
		}
	})
}
