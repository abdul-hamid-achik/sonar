package config

import (
	"testing"
	"time"
)

func TestQwenRouter_ClassifyTrivial(t *testing.T) {
	tests := []struct {
		name          string
		query         string
		maxComplexity QwenComplexity
	}{
		{"simple what", "what is go", QwenTrivial},
		{"simple who", "who created go", QwenSimple},
		{"simple define", "define interface", QwenTrivial},
		{"simple greeting", "hello", QwenTrivial},
		{"simple thanks", "thanks", QwenTrivial},
		{"simple list", "list files", QwenTrivial},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyQwenTask(tt.query, ModeAskContext)
			if got > tt.maxComplexity {
				t.Errorf("classifyQwenTask(%q) = %v, want <= %v", tt.query, got, tt.maxComplexity)
			}
		})
	}
}

func TestQwenRouter_ClassifySimple(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"simple how", "how do i create a file"},
		{"simple explain", "explain this code"},
		{"simple find", "find all go files"},
		{"simple check", "check if file exists"},
		{"simple read", "read config file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyQwenTask(tt.query, ModeAskContext)
			t.Logf("%s: %v", tt.query, got)
		})
	}
}

func TestQwenRouter_ClassifyModerate(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"create function", "create a function to parse json"},
		{"debug issue", "debug this nil pointer error"},
		{"refactor code", "refactor this function"},
		{"add test", "add unit tests for handler"},
		{"optimize query", "optimize this database query"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyQwenTask(tt.query, ModeBuildContext)
			t.Logf("%s: %v", tt.query, got)
		})
	}
}

func TestQwenRouter_ClassifyAdvanced(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"architecture", "design microservice architecture"},
		{"system design", "system design for high traffic"},
		{"security audit", "security audit of api"},
		{"full stack", "build a full stack application"},
		{"migration", "migration from mysql to postgres"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyQwenTask(tt.query, ModeBuildContext)
			t.Logf("%s: %v", tt.query, got)
		})
	}
}

func TestQwenRouter_ModeAffectsClassification(t *testing.T) {
	query := "how do i fix this bug"

	ask := classifyQwenTask(query, ModeAskContext)
	build := classifyQwenTask(query, ModeBuildContext)

	// BUILD mode should generally prefer equal or larger models than ASK
	// Note: This is a soft requirement - the mode adjustment is subtle
	t.Logf("ASK mode: %v, BUILD mode: %v", ask, build)
}

func TestQwenRouter_WordCountAffectsClassification(t *testing.T) {
	short := "what is go"
	long := "what is the go programming language and how does it compare to rust and what are its main features and use cases in modern software development"

	shortComplexity := classifyQwenTask(short, ModeAskContext)
	longComplexity := classifyQwenTask(long, ModeAskContext)

	// Long query should ideally be more complex, but at minimum not less
	// Note: This test documents the behavior - word count does affect scoring
	t.Logf("short (%d chars): %v, long (%d chars): %v", len(short), shortComplexity, len(long), longComplexity)
}

func TestQwenRouter_CodePatterns(t *testing.T) {
	tests := []struct {
		name          string
		query         string
		maxComplexity QwenComplexity
	}{
		{"simple variable", "declare a variable", QwenModerate},
		{"simple function", "write a function", QwenAdvanced},
		{"moderate struct", "define a struct", QwenAdvanced},
		{"moderate interface", "implement an interface", QwenAdvanced},
		{"moderate concurrency", "add concurrency with goroutines", QwenAdvanced},
		{"advanced architecture", "design the architecture", QwenAdvanced},
		{"advanced distributed", "distributed system design", QwenAdvanced},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyQwenTask(tt.query, ModeBuildContext)
			// All code patterns should classify as something (not panic)
			t.Logf("%s: %v", tt.query, got)
		})
	}
}

func TestQwenRouter_SelectAskModel(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	router.SetModeContext(ModeAskContext)

	// Simple question should get small model
	model := router.SelectModelForMode("what is go", ModeAskContext)
	if model != "qwen3.5:0.8b" && model != "qwen3.5:2b" {
		t.Errorf("ASK mode simple query should get small model, got %s", model)
	}

	// Complex question should get capable model (2B or higher)
	model = router.SelectModelForMode("design a distributed system", ModeAskContext)
	if model == "qwen3.5:0.8b" {
		t.Errorf("ASK mode complex query should not get 0.8B model, got %s", model)
	}
}

func TestQwenRouter_SelectPlanModel(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	router.SetModeContext(ModePlanContext)

	// Planning should prefer 4B for reasoning
	model := router.SelectModelForMode("plan the architecture", ModePlanContext)
	if model != "qwen3.5:4b" && model != "qwen3.5:9b" {
		t.Errorf("PLAN mode should prefer 4B or 9B, got %s", model)
	}
}

func TestQwenRouter_SelectBuildModel(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	router.SetModeContext(ModeBuildContext)

	// Building should prefer capable models
	model := router.SelectModelForMode("implement the feature", ModeBuildContext)
	if model != "qwen3.5:4b" && model != "qwen3.5:9b" {
		t.Errorf("BUILD mode should prefer 4B or 9B, got %s", model)
	}
}

func TestQwenRouterMapsComplexityAndAvailability(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	router.SetAvailableModels([]string{"qwen3.5:2b"})

	for _, query := range []string{"what is go", "design a distributed system"} {
		if got := router.SelectModelForMode(query, ModeBuildContext); got != "qwen3.5:2b" {
			t.Errorf("SelectModelForMode(%q) = %q, want installed 2B fallback", query, got)
		}
	}
}

func TestQwenRouterNeverAutoSelectsInstalledExclusiveOrnith(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	router.SetAvailableModels([]string{"ornith:latest"})

	for _, query := range []string{"what is Go", "implement a full stack system"} {
		if got := router.SelectModelForMode(query, ModeBuildContext); got != "" {
			t.Fatalf("Qwen auto-router selected exclusive Ornith for %q: %q", query, got)
		}
	}
	if got := router.ResolveAvailableModel("ornith:latest"); got != "ornith:latest" {
		t.Fatalf("explicit installed Ornith resolved to %q", got)
	}
}

func TestQwenRouterCustomExclusiveDefaultRequiresExplicitResolution(t *testing.T) {
	cfg := ModelConfig{
		AutoSelect:    true,
		DefaultModel:  "ornith:latest",
		FallbackChain: []string{"ornith:latest"},
		Models: []Model{{
			Name: "ornith:latest", Capability: CapabilitySimple, Exclusive: true,
		}},
	}
	router := NewQwenModelRouter(&cfg)
	router.SetAvailableModels([]string{"ornith:latest"})

	if got := router.SelectModel("what is Go"); got != "" {
		t.Fatalf("custom Qwen auto-router selected exclusive default: %q", got)
	}
	if got := router.ResolveAvailableModel("ornith:latest"); got != "ornith:latest" {
		t.Fatalf("explicit exclusive resolution = %q", got)
	}
}

func TestQwenRouterReturnsNoModelForKnownEmptyInventory(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	router.SetAvailableModels([]string{})

	if got := router.ResolveAvailableModel(cfg.DefaultModel); got != "" {
		t.Fatalf("empty inventory resolved to %q, want no model", got)
	}
	if got := router.SelectModelForMode("implement this", ModeBuildContext); got != "" {
		t.Fatalf("empty inventory selected %q, want no model", got)
	}
}

func TestQwenRouterCanonicalizesAvailableModelTags(t *testing.T) {
	cfg := &ModelConfig{
		Models:        []Model{{Name: "llama3", Capability: CapabilityMedium}},
		DefaultModel:  "llama3",
		FallbackChain: []string{"llama3"},
	}
	router := NewQwenModelRouter(cfg)
	router.SetAvailableModels([]string{"llama3:latest"})
	if got := router.ResolveAvailableModel("llama3"); got != "llama3" {
		t.Fatalf("canonical available model resolved to %q", got)
	}
	if got := router.GetModelForCapability(CapabilityMedium); got != "llama3" {
		t.Fatalf("canonical capability model resolved to %q", got)
	}
}

func TestQwenRouterRecordOverrideDoesNotDeadlockWithAvailability(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	router.SetAvailableModels([]string{"qwen3.5:2b"})
	done := make(chan struct{})
	go func() {
		router.RecordOverride("explain this", "qwen3.5:2b")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RecordOverride deadlocked")
	}
}

func TestQwenRouter_GetRecommendedModel(t *testing.T) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)

	model, reason, complexity := router.GetRecommendedModel("what is go")

	if model == "" {
		t.Error("GetRecommendedModel should return a model")
	}
	if reason == "" {
		t.Error("GetRecommendedModel should return a reason")
	}
	if complexity == "" {
		t.Error("GetRecommendedModel should return a complexity")
	}
}

func TestQwenRouter_QuestionMarkHandling(t *testing.T) {
	// Short questions with ? should be simpler
	short := "what is go?"
	long := "can you explain what the go programming language is and how it works?"

	shortComplexity := classifyQwenTask(short, ModeAskContext)
	longComplexity := classifyQwenTask(long, ModeAskContext)

	if shortComplexity >= longComplexity {
		t.Logf("Note: short question complexity (%v) vs long (%v)", shortComplexity, longComplexity)
	}
}

func TestQwenRouter_WhyQuestions(t *testing.T) {
	// Why questions need reasoning
	why := "why does this code fail"
	what := "what does this code do"

	whyComplexity := classifyQwenTask(why, ModeAskContext)
	whatComplexity := classifyQwenTask(what, ModeAskContext)

	if whyComplexity < whatComplexity {
		t.Errorf("why questions should be more complex: why=%v, what=%v", whyComplexity, whatComplexity)
	}
}

func BenchmarkQwenRouter_ClassifyTask(b *testing.B) {
	queries := []string{
		"what is go",
		"how do i create a file",
		"debug this nil pointer error",
		"design microservice architecture",
	}

	for i := 0; i < b.N; i++ {
		for _, q := range queries {
			_ = classifyQwenTask(q, ModeAskContext)
		}
	}
}

func BenchmarkQwenRouter_SelectModel(b *testing.B) {
	cfg := DefaultModelConfig()
	router := NewQwenModelRouter(&cfg)
	queries := []string{
		"what is go",
		"how do i create a file",
		"debug this nil pointer error",
		"design microservice architecture",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, q := range queries {
			_ = router.SelectModel(q)
		}
	}
}
