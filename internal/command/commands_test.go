package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestRegistry() *Registry {
	r := NewRegistry()
	RegisterBuiltins(r)
	return r
}

func TestBuiltin_Help(t *testing.T) {
	r := newTestRegistry()
	result := r.Execute(&Context{}, "help", nil)
	if result.Action != ActionShowHelp {
		t.Errorf("help action = %d, want %d (ActionShowHelp)", result.Action, ActionShowHelp)
	}
}

func TestBuiltin_Clear(t *testing.T) {
	r := newTestRegistry()
	result := r.Execute(&Context{}, "clear", nil)
	if result.Action != ActionClear {
		t.Errorf("clear action = %d, want %d (ActionClear)", result.Action, ActionClear)
	}
	if result.Text == "" {
		t.Error("clear should have text")
	}
}

func TestBuiltin_New(t *testing.T) {
	r := newTestRegistry()
	result := r.Execute(&Context{}, "new", nil)
	if result.Action != ActionClear {
		t.Errorf("new action = %d, want %d (ActionClear)", result.Action, ActionClear)
	}
	if result.Text == "" {
		t.Error("new should have text")
	}
}

func TestBuiltin_Plan(t *testing.T) {
	r := newTestRegistry()
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "empty task"},
		{name: "prefilled task", args: []string{"refactor", "the", "router"}, want: "refactor the router"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := r.Execute(&Context{}, "plan", test.args)
			if result.Error != "" || result.Action != ActionOpenPlan || result.Data != test.want {
				t.Fatalf("/plan %v = %#v", test.args, result)
			}
		})
	}
}

func TestBuiltin_Recover(t *testing.T) {
	r := newTestRegistry()
	if result := r.Execute(&Context{}, "recover", nil); result.Error != "" || result.Action != ActionRecoverExecution {
		t.Fatalf("/recover = %#v", result)
	}
	if result := r.Execute(&Context{}, "recover", []string{"force"}); result.Error == "" || result.Action != ActionNone {
		t.Fatalf("/recover force did not fail closed: %#v", result)
	}
}

func TestBuiltin_ImageReturnsTypedAsyncActions(t *testing.T) {
	r := newTestRegistry()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		command    string
		args       []string
		wantAction Action
		wantData   string
		wantError  string
	}{
		{name: "attach relative path", command: "image", args: []string{"screenshots", "design review.png"}, wantAction: ActionAttachImage, wantData: "screenshots design review.png"},
		{name: "attach alias", command: "attach", args: []string{"capture.png"}, wantAction: ActionAttachImage, wantData: "capture.png"},
		{name: "expand home", command: "image", args: []string{"~/Desktop/capture.png"}, wantAction: ActionAttachImage, wantData: filepath.Join(home, "Desktop/capture.png")},
		{name: "list", command: "image", args: []string{"list"}, wantAction: ActionListImages},
		{name: "list alias", command: "image", args: []string{"ls"}, wantAction: ActionListImages},
		{name: "clear", command: "image", args: []string{"clear"}, wantAction: ActionClearImages},
		{name: "clear alias", command: "image", args: []string{"remove-all"}, wantAction: ActionClearImages},
		{name: "forget history", command: "image", args: []string{"forget-history"}, wantAction: ActionForgetImageHistory},
		{name: "forget history alias", command: "image", args: []string{"drop-history"}, wantAction: ActionForgetImageHistory},
		{name: "missing path", command: "image", wantError: "usage:"},
		{name: "list rejects suffix", command: "image", args: []string{"list", "unexpected"}, wantError: "usage: /image list"},
		{name: "clear rejects suffix", command: "image", args: []string{"clear", "unexpected"}, wantError: "usage: /image clear"},
		{name: "forget history rejects suffix", command: "image", args: []string{"forget-history", "unexpected"}, wantError: "usage: /image forget-history"},
		{name: "reject terminal controls", command: "image", args: []string{"capture\x1b[31m.png"}, wantError: "control characters"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := r.Execute(nil, test.command, test.args)
			if test.wantError != "" {
				if !strings.Contains(result.Error, test.wantError) || result.Action != ActionNone || result.Data != "" || result.Text != "" {
					t.Fatalf("result = %#v, want fail-closed error containing %q", result, test.wantError)
				}
				return
			}
			if result.Error != "" || result.Action != test.wantAction || result.Data != test.wantData || result.Text != "" {
				t.Fatalf("result = %#v, want action=%d data=%q", result, test.wantAction, test.wantData)
			}
		})
	}
}

func TestBuiltinImageIsDiscoverableWithoutExecutingIO(t *testing.T) {
	r := newTestRegistry()
	matches := r.Match("ima")
	if len(matches) != 1 {
		t.Fatalf("Match(image) = %#v", matches)
	}
	image := matches[0]
	if image.Name != "image" || image.Usage != "/image <path>|list|clear|forget-history" || !strings.Contains(image.Description, "Attach") {
		t.Fatalf("image metadata = %#v", image)
	}
	if result := r.Execute(nil, "attach", []string{"not-checked-here.png"}); result.Action != ActionAttachImage || result.Data != "not-checked-here.png" || result.Error != "" {
		t.Fatalf("alias result = %#v", result)
	}
}

func TestBuiltin_Goal(t *testing.T) {
	r := newTestRegistry()

	tests := []struct {
		name       string
		ctx        *Context
		args       []string
		wantAction Action
		wantData   string
		wantError  bool
	}{
		{name: "opens form when absent", ctx: &Context{}, wantAction: ActionOpenGoal},
		{name: "shows configured goal", ctx: &Context{GoalConfigured: true}, wantAction: ActionShowGoal},
		{name: "prefills free-form objective", ctx: &Context{}, args: []string{"ship", "the", "release"}, wantAction: ActionOpenGoal, wantData: "ship the release"},
		{name: "new accepts objective", ctx: &Context{}, args: []string{"new", "fix", "resume"}, wantAction: ActionOpenGoal, wantData: "fix resume"},
		{name: "show", ctx: &Context{GoalConfigured: true, GoalStatus: "active"}, args: []string{"show"}, wantAction: ActionShowGoal},
		{name: "pause", ctx: &Context{GoalConfigured: true, GoalStatus: "active"}, args: []string{"pause"}, wantAction: ActionPauseGoal},
		{name: "resume", ctx: &Context{GoalConfigured: true, GoalStatus: "paused"}, args: []string{"resume"}, wantAction: ActionResumeGoal},
		{name: "edit is budget-only", ctx: &Context{GoalConfigured: true, GoalObjective: "durable objective", GoalStatus: "paused"}, args: []string{"edit"}, wantAction: ActionEditGoalBudget},
		{name: "drop", ctx: &Context{GoalConfigured: true, GoalStatus: "paused"}, args: []string{"drop"}, wantAction: ActionDropGoal},
		{name: "drop rejects trailing input", ctx: &Context{GoalConfigured: true, GoalStatus: "paused"}, args: []string{"drop", "extra"}, wantError: true},
		{name: "pause rejects trailing input", ctx: &Context{GoalConfigured: true, GoalStatus: "active"}, args: []string{"pause", "now"}, wantError: true},
		{name: "status rejects trailing input", ctx: &Context{GoalConfigured: true, GoalStatus: "active"}, args: []string{"status", "verbose"}, wantError: true},
		{name: "pause explains missing goal", ctx: &Context{}, args: []string{"pause"}, wantError: true},
		{name: "new rejects active goal", ctx: &Context{GoalConfigured: true, GoalStatus: "active"}, args: []string{"new", "other"}, wantError: true},
		{name: "unknown flag fails closed", ctx: &Context{}, args: []string{"--forever"}, wantError: true},
	}

	for _, test := range []struct {
		args     []string
		wantTime time.Duration
		wantText string
		wantErr  bool
	}{
		{args: []string{"30m", "polish", "the", "TUI"}, wantTime: 30 * time.Minute, wantText: "polish the TUI"},
		{args: []string{"1h30m", "finish", "release"}, wantTime: 90 * time.Minute, wantText: "finish release"},
		{args: []string{"30m"}, wantErr: true},
		{args: []string{"0m", "never"}, wantErr: true},
		{args: []string{"30min", "polish", "the", "TUI"}, wantErr: true},
	} {
		result := r.Execute(&Context{}, "goal", test.args)
		if test.wantErr {
			if result.Error == "" {
				t.Fatalf("/goal %v accepted", test.args)
			}
			continue
		}
		if result.Error != "" || result.Goal == nil || result.Goal.TimeBudget != test.wantTime || result.Goal.Prompt != test.wantText || !result.Goal.TimeExplicit {
			t.Fatalf("/goal %v = %#v", test.args, result)
		}
	}

	t.Run("invalid duration is explicit", func(t *testing.T) {
		result := r.Execute(&Context{}, "goal", []string{"30min", "polish", "the", "TUI"})
		if result.Goal != nil || !strings.Contains(result.Error, "invalid goal duration") || !strings.Contains(result.Error, "30m") {
			t.Fatalf("invalid duration result = %#v", result)
		}
	})

	t.Run("numeric objective remains free-form", func(t *testing.T) {
		result := r.Execute(&Context{}, "goal", []string{"2026", "roadmap"})
		if result.Error != "" || result.Goal == nil || result.Goal.TimeExplicit || result.Goal.Prompt != "2026 roadmap" {
			t.Fatalf("numeric objective = %#v", result)
		}
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.Execute(tt.ctx, "goal", tt.args)
			if tt.wantError {
				if result.Error == "" {
					t.Fatal("expected an error")
				}
				return
			}
			if result.Error != "" {
				t.Fatalf("unexpected error: %s", result.Error)
			}
			if result.Action != tt.wantAction || result.Data != tt.wantData {
				t.Fatalf("goal result = action %d data %q, want action %d data %q", result.Action, result.Data, tt.wantAction, tt.wantData)
			}
		})
	}

	for _, alias := range []struct {
		name       string
		args       []string
		ctx        *Context
		wantAction Action
	}{
		{name: "g", args: []string{"set", "alias objective"}, ctx: &Context{}, wantAction: ActionOpenGoal},
		{name: "goal", args: []string{"status"}, ctx: &Context{GoalConfigured: true, GoalStatus: "paused"}, wantAction: ActionShowGoal},
		{name: "goal", args: []string{"retry"}, ctx: &Context{GoalConfigured: true, GoalStatus: "paused"}, wantAction: ActionResumeGoal},
		{name: "goal", args: []string{"budget"}, ctx: &Context{GoalConfigured: true, GoalStatus: "paused"}, wantAction: ActionEditGoalBudget},
	} {
		result := r.Execute(alias.ctx, alias.name, alias.args)
		if result.Error != "" || result.Action != alias.wantAction {
			t.Fatalf("/%s %v = action %d error %q, want action %d", alias.name, alias.args, result.Action, result.Error, alias.wantAction)
		}
	}
}

func TestBuiltin_Model(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{
		Model:     "qwen3.5:0.8b",
		ModelList: []string{"qwen3.5:0.8b", "qwen3.5:2b", "qwen3.5:4b", "qwen3.5:9b"},
	}

	tests := []struct {
		name       string
		args       []string
		wantAction Action
		wantData   string
		wantErr    bool
		checkText  string
	}{
		{
			name:       "no args opens model picker",
			args:       nil,
			wantAction: ActionShowModelPicker,
		},
		{
			name:       "auto resumes routing",
			args:       []string{"auto"},
			wantAction: ActionEnableAutoModel,
		},
		{
			name:      "list shows models",
			args:      []string{"list"},
			checkText: "Available models",
		},
		{name: "fast shortcut removed", args: []string{"fast"}, wantErr: true},
		{name: "smart shortcut removed", args: []string{"smart"}, wantErr: true},
		{
			name:       "valid name switches",
			args:       []string{"qwen3.5:2b"},
			wantAction: ActionSwitchModel,
			wantData:   "qwen3.5:2b",
		},
		{
			name:    "invalid name errors",
			args:    []string{"nonexistent"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.Execute(ctx, "model", tt.args)
			if tt.wantErr {
				if result.Error == "" {
					t.Error("expected error")
				}
				return
			}
			if result.Error != "" {
				t.Errorf("unexpected error: %s", result.Error)
				return
			}
			if tt.wantAction != ActionNone && result.Action != tt.wantAction {
				t.Errorf("action = %d, want %d", result.Action, tt.wantAction)
			}
			if tt.wantData != "" && result.Data != tt.wantData {
				t.Errorf("data = %q, want %q", result.Data, tt.wantData)
			}
			if tt.checkText != "" && !strings.Contains(result.Text, tt.checkText) {
				t.Errorf("text %q does not contain %q", result.Text, tt.checkText)
			}
		})
	}
}

func TestBuiltin_Models(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{
		Model:     "qwen3.5:0.8b",
		ModelList: []string{"qwen3.5:0.8b", "qwen3.5:2b"},
	}
	result := r.Execute(ctx, "models", nil)
	if result.Action != ActionShowModelPicker {
		t.Errorf("expected ActionShowModelPicker, got %d", result.Action)
	}
}

func TestBuiltin_Provider(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{
		Provider:     "ollama",
		ProviderList: []string{"ollama", "xai", "openai"},
		Model:        "qwen3.5:2b",
	}
	open := r.Execute(ctx, "provider", nil)
	if open.Action != ActionShowProviderPicker {
		t.Fatalf("empty args = %#v, want picker", open)
	}
	list := r.Execute(ctx, "provider", []string{"list"})
	if list.Error != "" || !strings.Contains(list.Text, "xai") {
		t.Fatalf("list = %#v", list)
	}
	sw := r.Execute(ctx, "provider", []string{"xai"})
	if sw.Action != ActionSwitchProvider || sw.Data != "xai" {
		t.Fatalf("switch = %#v", sw)
	}
	bad := r.Execute(ctx, "provider", []string{"nope"})
	if bad.Error == "" {
		t.Fatal("expected unknown provider error")
	}
}

func TestBuiltinChangesUsesStablePathOrder(t *testing.T) {
	r := newTestRegistry()
	result := r.Execute(&Context{FileChanges: map[string]int{
		"zeta.go":   1,
		"alpha.go":  2,
		"middle.go": 1,
	}}, "changes", nil)
	if result.Error != "" {
		t.Fatalf("/changes error = %q", result.Error)
	}
	alpha := strings.Index(result.Text, "alpha.go (2x)")
	middle := strings.Index(result.Text, "middle.go")
	zeta := strings.Index(result.Text, "zeta.go")
	if alpha < 0 || middle < 0 || zeta < 0 || alpha >= middle || middle >= zeta {
		t.Fatalf("/changes order is not deterministic:\n%s", result.Text)
	}
}

func TestBuiltinResumeOpensSavedSessions(t *testing.T) {
	r := newTestRegistry()
	result := r.Execute(&Context{}, "resume", nil)
	if result.Error != "" || result.Action != ActionShowSessions {
		t.Fatalf("/resume = %#v, want saved-session picker", result)
	}
}

func TestBuiltinArtifactsListsOnlyTypedDigestFields(t *testing.T) {
	r := newTestRegistry()
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	ctx := &Context{Artifacts: []ArtifactInfo{
		{
			URI:            "fcheap://stash/stash-a",
			FileCount:      1,
			TotalBytes:     42,
			CreatedAt:      "2026-07-13T12:30:00Z",
			ContentSHA256:  shaA,
			SecretsWarning: true,
			IndexingFailed: true,
		},
		{
			URI:           "fcheap://stash/stash-b",
			FileCount:     3,
			TotalBytes:    2048,
			CreatedAt:     "2026-07-13T13:45:00Z",
			ContentSHA256: shaB,
		},
	}}

	result := r.Execute(ctx, "artifacts", nil)
	if result.Error != "" {
		t.Fatalf("/artifacts error = %q", result.Error)
	}
	want := "Saved artifacts (2):\n" +
		"  fcheap://stash/stash-a\n" +
		"    1 file · 42 bytes · created 2026-07-13T12:30:00Z\n" +
		"    Content SHA-256 (full): " + shaA + "\n" +
		"    Warning: potential secrets need review.\n" +
		"    Indexing: incomplete.\n" +
		"  fcheap://stash/stash-b\n" +
		"    3 files · 2048 bytes · created 2026-07-13T13:45:00Z\n" +
		"    Content SHA-256 (full): " + shaB
	if result.Text != want {
		t.Fatalf("/artifacts text:\n%s\nwant:\n%s", result.Text, want)
	}
	if alias := r.Execute(ctx, "artifact", nil); alias.Text != result.Text || alias.Error != "" {
		t.Fatalf("/artifact alias = %#v", alias)
	}
}

func TestBuiltinArtifactsEmptyAndArguments(t *testing.T) {
	r := newTestRegistry()
	if result := r.Execute(&Context{}, "artifacts", nil); result.Text != "No saved artifacts in this session." || result.Error != "" {
		t.Fatalf("empty /artifacts = %#v", result)
	}
	if result := r.Execute(&Context{}, "artifacts", []string{"verbose"}); result.Error != "usage: /artifacts" || result.Text != "" {
		t.Fatalf("argument-bearing /artifacts = %#v", result)
	}
}

func TestBuiltinArtifactsReportsBoundedOutputHonestly(t *testing.T) {
	artifacts := make([]ArtifactInfo, MaxContextArtifacts+1)
	for i := range artifacts {
		artifacts[i] = ArtifactInfo{
			URI: "fcheap://stash/bounded", FileCount: 1, TotalBytes: 1,
			CreatedAt: "2026-07-13T12:30:00Z", ContentSHA256: strings.Repeat("a", 64),
		}
	}
	result := newTestRegistry().Execute(&Context{Artifacts: artifacts}, "artifacts", nil)
	if result.Error != "" || !strings.HasPrefix(result.Text, "Saved artifacts (64 shown; more omitted):") {
		t.Fatalf("bounded /artifacts = %#v", result)
	}
}

func TestLegacyMigrationCommandsAreNotRegistered(t *testing.T) {
	r := newTestRegistry()
	for _, name := range []string{"migrate-memory", "migrate-ice", "migrate-checkpoints"} {
		result := r.Execute(&Context{}, name, nil)
		if result.Action != ActionNone || !strings.Contains(result.Error, "unknown command") {
			t.Fatalf("legacy command %q remains executable: %#v", name, result)
		}
	}
}

func TestBuiltin_Agent(t *testing.T) {
	r := newTestRegistry()

	t.Run("no args lists agents", func(t *testing.T) {
		ctx := &Context{AgentList: []string{"coder", "reviewer"}, AgentProfile: "coder"}
		result := r.Execute(ctx, "agent", nil)
		if !strings.Contains(result.Text, "Available agent profiles") {
			t.Errorf("expected agent list, got %q", result.Text)
		}
	})

	t.Run("list subcommand", func(t *testing.T) {
		ctx := &Context{AgentList: []string{"coder"}}
		result := r.Execute(ctx, "agent", []string{"list"})
		if !strings.Contains(result.Text, "Available agent profiles") {
			t.Errorf("expected agent list, got %q", result.Text)
		}
	})

	t.Run("valid switch", func(t *testing.T) {
		ctx := &Context{AgentList: []string{"coder", "reviewer"}}
		result := r.Execute(ctx, "agent", []string{"reviewer"})
		if result.Action != ActionSwitchAgent {
			t.Errorf("action = %d, want %d", result.Action, ActionSwitchAgent)
		}
		if result.Data != "reviewer" {
			t.Errorf("data = %q, want %q", result.Data, "reviewer")
		}
	})

	t.Run("invalid errors", func(t *testing.T) {
		ctx := &Context{AgentList: []string{"coder"}}
		result := r.Execute(ctx, "agent", []string{"unknown"})
		if result.Error == "" {
			t.Error("expected error for unknown agent")
		}
	})

	t.Run("no agents", func(t *testing.T) {
		ctx := &Context{AgentList: []string{}}
		result := r.Execute(ctx, "agent", nil)
		if !strings.Contains(result.Text, "No agent profiles") {
			t.Errorf("expected no agents message, got %q", result.Text)
		}
	})
}

func TestBuiltin_Load(t *testing.T) {
	r := newTestRegistry()

	t.Run("no args errors", func(t *testing.T) {
		result := r.Execute(&Context{}, "load", nil)
		if result.Error == "" {
			t.Error("expected error for no args")
		}
	})

	t.Run("valid file loads", func(t *testing.T) {
		tmp := t.TempDir()
		path := filepath.Join(tmp, "test.md")
		if err := os.WriteFile(path, []byte("# Hello"), 0644); err != nil {
			t.Fatal(err)
		}
		result := r.Execute(&Context{}, "load", []string{path})
		if result.Error != "" {
			t.Errorf("unexpected error: %s", result.Error)
		}
		if result.Action != ActionLoadContext {
			t.Errorf("action = %d, want %d", result.Action, ActionLoadContext)
		}
		if result.Data != path {
			t.Errorf("data path = %q, want %q", result.Data, path)
		}
	})

	t.Run("filesystem validation is deferred to async TUI effect", func(t *testing.T) {
		tmp := t.TempDir()
		path := filepath.Join(tmp, "big.md")
		data := make([]byte, 33*1024) // > 32KB
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
		result := r.Execute(&Context{}, "load", []string{path})
		if result.Error != "" || result.Action != ActionLoadContext || result.Data != path {
			t.Fatalf("load handler performed I/O instead of returning an effect: %#v", result)
		}
	})

	t.Run("nonexistent path is also deferred", func(t *testing.T) {
		result := r.Execute(&Context{}, "load", []string{"/nonexistent/file.md"})
		if result.Error != "" || result.Action != ActionLoadContext {
			t.Fatalf("load handler performed synchronous path I/O: %#v", result)
		}
	})
}

func TestBuiltin_Unload(t *testing.T) {
	r := newTestRegistry()

	t.Run("no loaded file", func(t *testing.T) {
		result := r.Execute(&Context{LoadedFile: ""}, "unload", nil)
		if !strings.Contains(result.Text, "No context") {
			t.Errorf("expected 'No context' message, got %q", result.Text)
		}
	})

	t.Run("loaded file unloads", func(t *testing.T) {
		result := r.Execute(&Context{LoadedFile: "something.md"}, "unload", nil)
		if result.Action != ActionUnloadContext {
			t.Errorf("action = %d, want %d", result.Action, ActionUnloadContext)
		}
	})
}

func TestBuiltin_Skill(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{
		Skills: []SkillInfo{
			{Name: "coder", Description: "Code generation", Active: true},
			{Name: "reviewer", Description: "Code review", Active: false},
		},
	}

	t.Run("empty catalog points to shared skills directory", func(t *testing.T) {
		result := r.Execute(&Context{}, "skill", []string{"list"})
		if !strings.Contains(result.Text, "configured agents directory") || !strings.Contains(result.Text, "skills/<name>/SKILL.md") || strings.Contains(result.Text, ".config/sonar/skills") {
			t.Fatalf("empty skill guidance = %q", result.Text)
		}
	})

	t.Run("no args lists skills", func(t *testing.T) {
		result := r.Execute(ctx, "skill", nil)
		if !strings.Contains(result.Text, "Skills") {
			t.Errorf("expected skills list, got %q", result.Text)
		}
	})

	t.Run("list subcommand", func(t *testing.T) {
		result := r.Execute(ctx, "skill", []string{"list"})
		if !strings.Contains(result.Text, "Skills") {
			t.Errorf("expected skills list, got %q", result.Text)
		}
	})

	t.Run("activate", func(t *testing.T) {
		result := r.Execute(ctx, "skill", []string{"activate", "reviewer"})
		if result.Action != ActionActivateSkill {
			t.Errorf("action = %d, want %d", result.Action, ActionActivateSkill)
		}
		if result.Data != "reviewer" {
			t.Errorf("data = %q, want %q", result.Data, "reviewer")
		}
	})

	t.Run("deactivate", func(t *testing.T) {
		result := r.Execute(ctx, "skill", []string{"deactivate", "coder"})
		if result.Action != ActionDeactivateSkill {
			t.Errorf("action = %d, want %d", result.Action, ActionDeactivateSkill)
		}
		if result.Data != "coder" {
			t.Errorf("data = %q, want %q", result.Data, "coder")
		}
	})

	t.Run("unknown action errors", func(t *testing.T) {
		result := r.Execute(ctx, "skill", []string{"unknown", "foo"})
		if result.Error == "" {
			t.Error("expected error for unknown skill action")
		}
	})

	t.Run("missing name errors", func(t *testing.T) {
		result := r.Execute(ctx, "skill", []string{"activate"})
		if result.Error == "" {
			t.Error("expected error for missing skill name")
		}
	})
}

func TestBuiltin_Servers(t *testing.T) {
	r := newTestRegistry()

	t.Run("no servers", func(t *testing.T) {
		result := r.Execute(&Context{ServerNames: nil}, "servers", nil)
		if !strings.Contains(result.Text, "No MCP servers") {
			t.Errorf("expected no servers message, got %q", result.Text)
		}
	})

	t.Run("with servers", func(t *testing.T) {
		ctx := &Context{
			ServerNames: []string{"server-a", "server-b"},
			ToolCount:   10,
		}
		result := r.Execute(ctx, "servers", nil)
		if !strings.Contains(result.Text, "server-a") {
			t.Errorf("expected server-a in output, got %q", result.Text)
		}
		if !strings.Contains(result.Text, "server-b") {
			t.Errorf("expected server-b in output, got %q", result.Text)
		}
		if !strings.Contains(result.Text, "10") {
			t.Errorf("expected tool count in output, got %q", result.Text)
		}
	})
}

func TestBuiltin_StatsSeparatesWorkloadFromContextOccupancy(t *testing.T) {
	r := newTestRegistry()
	result := r.Execute(&Context{
		CurrentModel:       "kimi-k2.7-code:cloud",
		SessionTurnCount:   6,
		SessionEvalTotal:   3769,
		SessionPromptTotal: 120351,
		LatestPromptTokens: 120351,
		NumCtx:             1_048_576,
	}, "stats", nil)

	for _, want := range []string{
		"Prompt processed:  120351",
		"Current request:  120351",
		"Context window:  1048576",
		"Context used:    11%",
	} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("stats missing %q:\n%s", want, result.Text)
		}
	}
	if strings.Contains(result.Text, "734%") || strings.Contains(result.Text, "(last turn)") {
		t.Fatalf("stats still conflates session workload with occupancy:\n%s", result.Text)
	}
}

func TestBuiltin_ICE(t *testing.T) {
	r := newTestRegistry()

	t.Run("disabled", func(t *testing.T) {
		result := r.Execute(&Context{ICEEnabled: false}, "ice", nil)
		if !strings.Contains(result.Text, "not enabled") {
			t.Errorf("expected disabled message, got %q", result.Text)
		}
	})

	t.Run("enabled shows status", func(t *testing.T) {
		ctx := &Context{
			ICEEnabled:       true,
			ICEConversations: 5,
			ICESessionID:     "abc-123",
		}
		result := r.Execute(ctx, "ice", nil)
		if !strings.Contains(result.Text, "enabled") {
			t.Errorf("expected enabled status, got %q", result.Text)
		}
		if !strings.Contains(result.Text, "5") {
			t.Errorf("expected conversation count, got %q", result.Text)
		}
		if !strings.Contains(result.Text, "abc-123") {
			t.Errorf("expected session ID, got %q", result.Text)
		}
	})
}

func TestBuiltin_Exit(t *testing.T) {
	r := newTestRegistry()
	result := r.Execute(&Context{}, "exit", nil)
	if result.Action != ActionQuit {
		t.Errorf("exit action = %d, want %d (ActionQuit)", result.Action, ActionQuit)
	}
}

func TestBuiltinsRejectUnexpectedArguments(t *testing.T) {
	r := newTestRegistry()
	ctx := &Context{}
	for _, name := range []string{
		"help", "clear", "unload", "servers", "ice", "sessions",
		"artifacts", "changes", "stats", "checkpoints", "exit",
	} {
		t.Run(name, func(t *testing.T) {
			result := r.Execute(ctx, name, []string{"unexpected"})
			if result.Error == "" {
				t.Fatalf("/%s accepted an unexpected argument: %#v", name, result)
			}
			if result.Action != ActionNone {
				t.Fatalf("/%s executed action %d after invalid arguments", name, result.Action)
			}
		})
	}

	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "model", args: []string{"auto", "unexpected"}},
		{name: "agent", args: []string{"list", "unexpected"}},
		{name: "skill", args: []string{"list", "unexpected"}},
		{name: "skill", args: []string{"activate", "review", "unexpected"}},
		{name: "restore", args: []string{"1", "unexpected"}},
	} {
		t.Run(test.name+"_shape", func(t *testing.T) {
			result := r.Execute(ctx, test.name, test.args)
			if result.Error == "" || result.Action != ActionNone {
				t.Fatalf("/%s %v = %#v, want usage error with no action", test.name, test.args, result)
			}
		})
	}
}

func TestBuiltinRestoreRequiresCanonicalPositiveID(t *testing.T) {
	r := newTestRegistry()
	for _, id := range []string{"0", "-1", "+1", "01", "1.0", "not-an-id"} {
		t.Run(id, func(t *testing.T) {
			result := r.Execute(nil, "restore", []string{id})
			if result.Error == "" || result.Action != ActionNone {
				t.Fatalf("/restore %q = %#v, want validation error", id, result)
			}
		})
	}
	result := r.Execute(nil, "restore", []string{"42"})
	if result.Error != "" || result.Action != ActionRestoreCheckpoint || result.Data != "42" {
		t.Fatalf("/restore 42 = %#v", result)
	}
}

func TestBuiltinPermissionsParsesAcceptEditsAndStatus(t *testing.T) {
	r := newTestRegistry()

	status := r.Execute(&Context{ApprovalPosture: "accept_workspace_edits", SessionApprovals: []string{"write · session_tool"}}, "permissions", nil)
	if status.Error != "" || status.Action != ActionNone || !strings.Contains(status.Text, "accept workspace edits") || !strings.Contains(status.Text, "write · session_tool") {
		t.Fatalf("/permissions status = %#v", status)
	}
	on := r.Execute(&Context{}, "permissions", []string{"accept-edits", "on"})
	if on.Error != "" || on.Action != ActionPermissionsAcceptEdits || on.Data != "on" {
		t.Fatalf("/permissions accept-edits on = %#v", on)
	}
	off := r.Execute(&Context{}, "permissions", []string{"accept-edits", "off"})
	if off.Error != "" || off.Action != ActionPermissionsAcceptEdits || off.Data != "off" {
		t.Fatalf("/permissions accept-edits off = %#v", off)
	}
	clear := r.Execute(&Context{}, "permissions", []string{"clear"})
	if clear.Error != "" || clear.Action != ActionPermissionsClear {
		t.Fatalf("/permissions clear = %#v", clear)
	}
	revoke := r.Execute(&Context{}, "permissions", []string{"revoke", "write"})
	if revoke.Error != "" || revoke.Action != ActionPermissionsRevoke || revoke.Data != "write" {
		t.Fatalf("/permissions revoke write = %#v", revoke)
	}
	allowBash := r.Execute(&Context{}, "permissions", []string{"allow-bash", "git", "status", "*"})
	if allowBash.Error != "" || allowBash.Action != ActionPermissionsAllowBash || allowBash.Data != "git status *" {
		t.Fatalf("/permissions allow-bash = %#v", allowBash)
	}
	allowPath := r.Execute(&Context{}, "permissions", []string{"allow-path", "src/app.go"})
	if allowPath.Error != "" || allowPath.Action != ActionPermissionsAllowPath || allowPath.Data != "src/app.go" {
		t.Fatalf("/permissions allow-path = %#v", allowPath)
	}
	panel := r.Execute(&Context{}, "permissions", []string{"panel"})
	if panel.Error != "" || panel.Action != ActionPermissionsPanel {
		t.Fatalf("/permissions panel = %#v", panel)
	}
	export := r.Execute(&Context{}, "permissions", []string{"export", "/tmp/rules.json"})
	if export.Error != "" || export.Action != ActionPermissionsExport || export.Data != "/tmp/rules.json" {
		t.Fatalf("/permissions export = %#v", export)
	}
	imp := r.Execute(&Context{}, "permissions", []string{"import", "--replace", "/tmp/rules.json"})
	if imp.Error != "" || imp.Action != ActionPermissionsImport || imp.Data != "replace|/tmp/rules.json" {
		t.Fatalf("/permissions import = %#v", imp)
	}
	clearRules := r.Execute(&Context{}, "permissions", []string{"clear-rules"})
	if clearRules.Error != "" || clearRules.Action != ActionPermissionsClearRules {
		t.Fatalf("/permissions clear-rules = %#v", clearRules)
	}
	statusRules := r.Execute(&Context{
		WorkspaceBashPrefixes: []string{"git status *"},
		WorkspaceWritePaths:   []string{"src/app.go"},
		WorkspaceMCPTools:     []string{"mcphub__mcphub_list_servers"},
	}, "permissions", []string{"rules"})
	if statusRules.Error != "" || !strings.Contains(statusRules.Text, "git status *") || !strings.Contains(statusRules.Text, "src/app.go") || !strings.Contains(statusRules.Text, "mcphub__mcphub_list_servers") {
		t.Fatalf("/permissions rules = %#v", statusRules)
	}
	for _, args := range [][]string{{"accept-edits"}, {"accept-edits", "maybe"}, {"clear", "extra"}, {"unknown"}, {"allow-path"}, {"allow-bash"}} {
		if result := r.Execute(&Context{}, "permissions", args); result.Error == "" || result.Action != ActionNone {
			t.Fatalf("/permissions %v = %#v, want usage error", args, result)
		}
	}
}

func TestBuiltinScopeParsesProcessLocalReadRootActions(t *testing.T) {
	r := newTestRegistry()

	if result := r.Execute(&Context{}, "scope", nil); result.Error != "" || result.Action != ActionNone || !strings.Contains(result.Text, "No temporary external read-only grants") || !strings.Contains(result.Text, "exact-file access") {
		t.Fatalf("/scope empty = %#v", result)
	}
	listed := r.Execute(&Context{ReadRoots: []string{"/z", "/a"}}, "scope", []string{"list"})
	if listed.Error != "" || !strings.Contains(listed.Text, "/a") || strings.Index(listed.Text, "/a") > strings.Index(listed.Text, "/z") || !strings.Contains(listed.Text, "not persisted") {
		t.Fatalf("/scope list = %#v", listed)
	}
	typed := r.Execute(&Context{ReadGrants: []ReadGrantInfo{{Path: "/tmp/report.pdf", Kind: "exact_file"}}}, "scope", []string{"list"})
	if typed.Error != "" || !strings.Contains(typed.Text, "exact file") || !strings.Contains(typed.Text, "never include siblings") {
		t.Fatalf("/scope typed list = %#v", typed)
	}

	for _, test := range []struct {
		name       string
		args       []string
		wantAction Action
		wantData   string
	}{
		{name: "add", args: []string{"add-read", "/projects/shared", "docs"}, wantAction: ActionAddReadRoot, wantData: "/projects/shared docs"},
		{name: "add alias", args: []string{"mount", "/projects/mcphub"}, wantAction: ActionAddReadRoot, wantData: "/projects/mcphub"},
		{name: "remove", args: []string{"remove-read", "/projects/mcphub"}, wantAction: ActionRemoveReadRoot, wantData: "/projects/mcphub"},
		{name: "clear", args: []string{"clear-read"}, wantAction: ActionClearReadRoots},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := r.Execute(&Context{}, "scope", test.args)
			if result.Error != "" || result.Action != test.wantAction || result.Data != test.wantData {
				t.Fatalf("result = %#v", result)
			}
		})
	}
	for _, args := range [][]string{{"add-read"}, {"remove-read"}, {"clear-read", "extra"}, {"unknown"}} {
		if result := r.Execute(&Context{}, "scope", args); result.Error == "" || result.Action != ActionNone {
			t.Fatalf("/scope %v = %#v, want usage error", args, result)
		}
	}
}

func TestBuiltinRegistrySurfaceIsUniqueAndExecutable(t *testing.T) {
	r := newTestRegistry()
	wantNames := []string{
		"help", "clear", "plan", "goal", "model", "theme", "provider", "recover", "agent", "load",
		"image", "scope", "permissions", "unload", "skill", "servers", "mcp", "agents", "tools", "ice", "memory", "sessions", "artifacts",
		"changes", "commit", "voice", "context", "stats", "export", "import", "checkpoint",
		"checkpoints", "restore", "exit",
	}
	all := r.All()
	if len(all) != len(wantNames) {
		t.Fatalf("built-in command count = %d, want %d", len(all), len(wantNames))
	}
	seen := make(map[string]string)
	for i, cmd := range all {
		if cmd.Name != wantNames[i] {
			t.Fatalf("command[%d] = %q, want %q", i, cmd.Name, wantNames[i])
		}
		if cmd.Handler == nil || strings.TrimSpace(cmd.Description) == "" {
			t.Fatalf("/%s has incomplete command metadata", cmd.Name)
		}
		for _, spelling := range append([]string{cmd.Name}, cmd.Aliases...) {
			if owner, exists := seen[spelling]; exists {
				t.Fatalf("command spelling %q is shared by /%s and /%s", spelling, owner, cmd.Name)
			}
			seen[spelling] = cmd.Name
			result := r.Execute(nil, spelling, nil)
			if result.Error != "" && !strings.Contains(strings.ToLower(result.Error), "usage:") {
				t.Fatalf("/%s default invocation failed unexpectedly: %s", spelling, result.Error)
			}
		}
	}
}

func TestContextCommandParsesSubcommands(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltins(r)
	for _, test := range []struct {
		args       []string
		wantAction Action
		wantData   string
		wantError  bool
	}{
		{args: nil, wantAction: ActionShowContextDoctor},
		{args: []string{"status"}, wantAction: ActionContextWindowStatus},
		// The num_ctx tuning verbs are gone, not hidden: every provider sonar
		// can run against is hosted and owns its own window, so auto/set only
		// ever errored and save wrote a value nothing read.
		{args: []string{"auto"}, wantError: true},
		{args: []string{"set", "96k"}, wantError: true},
		{args: []string{"save"}, wantError: true},
		{args: []string{"nope"}, wantError: true},
	} {
		result := r.Execute(&Context{}, "context", test.args)
		if test.wantError {
			if result.Error == "" {
				t.Fatalf("args=%v expected error, got %#v", test.args, result)
			}
			continue
		}
		if result.Error != "" || result.Action != test.wantAction || result.Data != test.wantData {
			t.Fatalf("args=%v result=%#v want action=%d data=%q", test.args, result, test.wantAction, test.wantData)
		}
	}
}
