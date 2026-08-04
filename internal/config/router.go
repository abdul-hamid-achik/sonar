package config

import (
	"context"
	"strings"
	"sync"
)

type TaskComplexity string

const (
	ComplexitySimple   TaskComplexity = "simple"
	ComplexityMedium   TaskComplexity = "medium"
	ComplexityComplex  TaskComplexity = "complex"
	ComplexityAdvanced TaskComplexity = "advanced"
)

var simpleIndicators = []string{
	"what is", "how do i", "explain", "what does",
	"find", "search", "list", "show", "get",
	"print", "echo", "read", "cat", "ls",
	"simple", "quick", "fast",
}

var mediumIndicators = []string{
	"create", "write", "generate", "add", "modify",
	"change", "update", "fix", "refactor",
	"function", "class", "variable", "test",
	"script", "command", "file", "directory",
}

var complexIndicators = []string{
	"debug", "error", "bug", "issue", "problem",
	"refactor", "architecture", "design", "review",
	"multiple", "several", "across", "migrate",
	"optimize", "performance", "security",
	"explain why", "analyze", "compare",
}

var advancedIndicators = []string{
	"build a", "create a", "implement", "develop",
	"full stack", "system", "infrastructure",
	"multi-step", "complex", "comprehensive",
	"security audit", "architecture design",
}

// ModelPinger is an interface for checking if a model is available.
type ModelPinger interface {
	PingModel(ctx context.Context, model string) error
}

// ModelOverride records when a user explicitly selects a model.
//
// It carried RouterModel and Timestamp as well, neither of which any code read.
// RouterModel in particular cost a full SelectModel pass on every /model switch
// to compute a value that was only ever appended to a slice — the
// "router choice vs. user choice" comparison the field names imply was never
// implemented.
type ModelOverride struct {
	Query     string
	UserModel string
}

type Router struct {
	config      *ModelConfig
	overrideLog []ModelOverride
	available   map[string]struct{}
	mu          sync.RWMutex
}

var _ ModelRouter = (*Router)(nil)

func NewRouter(cfg *ModelConfig) *Router {
	return &Router{
		config:      cfg,
		overrideLog: make([]ModelOverride, 0),
	}
}

func (r *Router) ClassifyTaskComplexity(query string) TaskComplexity {
	return ClassifyTask(query)
}

func (r *Router) SelectModel(query string) string {
	complexity := r.ClassifyTaskComplexity(query)

	// Check learned patterns if we have enough data
	wordComplexity := r.getLearnedPatterns()
	if len(wordComplexity) > 0 {
		words := strings.Fields(strings.ToLower(query))

		// Count votes from learned patterns
		complexityVotes := make(map[TaskComplexity]int)
		for _, w := range words {
			if len(w) >= 3 { // Skip short words
				if c, ok := wordComplexity[w]; ok {
					complexityVotes[c]++
				}
			}
		}

		// If strong learned signal (>30% words match a pattern), use it
		if len(words) > 0 {
			matchRatio := float64(complexityVotes[ComplexitySimple]+complexityVotes[ComplexityAdvanced]) / float64(len(words))
			if matchRatio > 0.3 {
				if complexityVotes[ComplexityAdvanced] > complexityVotes[ComplexitySimple] {
					complexity = ComplexityAdvanced
				} else if complexityVotes[ComplexitySimple] > complexityVotes[ComplexityAdvanced] {
					complexity = ComplexitySimple
				}
			}
		}
	}

	return selectAvailableAutoModel(r.config.SelectModelForTask(string(complexity)), r.config, r.availableSnapshot())
}

func (r *Router) SelectModelForMode(query string, mode ModeContext) string {
	return PromoteModelForMode(r, r.SelectModel(query), mode)
}

// RecordOverride logs when a user explicitly selects a model.
// This helps the router learn from user preferences.
func (r *Router) RecordOverride(query, userModel string) {
	// No SelectModel call here: nothing reads its result, and calling it under
	// the same RWMutex this method then write-locks is what deadlocked the
	// TUI's /model command. TestRouter_RecordOverrideDoesNotDeadlock still pins
	// that, so a future reader cannot reintroduce the nesting.
	r.mu.Lock()
	defer r.mu.Unlock()

	r.overrideLog = append(r.overrideLog, ModelOverride{
		Query:     query,
		UserModel: userModel,
	})

	// Keep last 100 overrides
	if len(r.overrideLog) > 100 {
		r.overrideLog = r.overrideLog[len(r.overrideLog)-100:]
	}
}

// getLearnedPatterns analyzes override history to find word->complexity mappings.
func (r *Router) getLearnedPatterns() map[string]TaskComplexity {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.overrideLog) < 3 {
		return nil // Not enough data
	}

	wordCounts := make(map[string]map[TaskComplexity]int)

	for _, o := range r.overrideLog {
		if o.Query == "" || o.UserModel == "" {
			continue
		}

		// Determine complexity from user-selected model
		var complexity TaskComplexity
		switch {
		case strings.Contains(o.UserModel, "0.8") || strings.Contains(o.UserModel, "2b"):
			complexity = ComplexitySimple
		case strings.Contains(o.UserModel, "4b"):
			complexity = ComplexityMedium
		case strings.Contains(o.UserModel, "9b"):
			complexity = ComplexityAdvanced
		default:
			continue
		}

		words := strings.Fields(strings.ToLower(o.Query))
		for _, w := range words {
			if len(w) < 3 {
				continue // Skip short words
			}
			if _, ok := wordCounts[w]; !ok {
				wordCounts[w] = make(map[TaskComplexity]int)
			}
			wordCounts[w][complexity]++
		}
	}

	// For each word, find dominant complexity
	wordComplexity := make(map[string]TaskComplexity)
	for word, counts := range wordCounts {
		var maxCount int
		var dominant TaskComplexity
		for c, cnt := range counts {
			if cnt > maxCount {
				maxCount = cnt
				dominant = c
			}
		}
		// Only use if we have enough samples (at least 2 overrides)
		if maxCount >= 2 {
			wordComplexity[word] = dominant
		}
	}

	return wordComplexity
}

func (r *Router) GetFallbackChain(currentModel string) []string {
	chain := r.config.FallbackChain

	for i, model := range chain {
		if CanonicalModelName(model) == CanonicalModelName(currentModel) {
			return chain[i:]
		}
	}

	return chain
}

func (r *Router) GetModelForCapability(capability ModelCapability) string {
	available := r.availableSnapshot()
	for _, m := range r.config.Models {
		if m.Capability == capability && !m.Exclusive && CheckModelMemorySafe(m.Name) == nil {
			if available == nil {
				return m.Name
			}
			if _, ok := available[CanonicalModelName(m.Name)]; ok {
				return m.Name
			}
		}
	}
	return selectAvailableAutoModel(r.config.DefaultModel, r.config, available)
}

// SetAvailableModels installs the locally available Ollama model set used by
// future routing decisions. Passing nil restores unknown/legacy behavior.
func (r *Router) SetAvailableModels(models []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if models == nil {
		r.available = nil
		return
	}
	r.available = make(map[string]struct{}, len(models))
	for _, model := range models {
		r.available[CanonicalModelName(model)] = struct{}{}
	}
}

func (r *Router) ResolveAvailableModel(preferred string) string {
	return selectAvailableModel(preferred, r.config, r.availableSnapshot())
}

func (r *Router) availableSnapshot() map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.available == nil {
		return nil
	}
	result := make(map[string]struct{}, len(r.available))
	for name := range r.available {
		result[name] = struct{}{}
	}
	return result
}

// SelectAvailableModel returns the first available model from the fallback chain.
// It checks each model in order and returns the first one that responds to a ping.
// If no models are available, returns the default model.
func (r *Router) SelectAvailableModel(ctx context.Context, pinger ModelPinger) string {
	chain := r.config.FallbackChain

	for _, model := range chain {
		if err := pinger.PingModel(ctx, model); err == nil {
			return model
		}
	}

	// Fallback to default if none available
	return r.config.DefaultModel
}

// SelectAvailableModelForTask returns the first available model for the given task complexity.
// It prioritizes models appropriate for the task, then falls back to larger models if unavailable.
func (r *Router) SelectAvailableModelForTask(ctx context.Context, pinger ModelPinger, query string) string {
	// First, get the preferred model for this task
	preferred := r.SelectModel(query)

	// Check if preferred model is available
	if err := pinger.PingModel(ctx, preferred); err == nil {
		return preferred
	}

	// Try fallback chain
	chain := r.GetFallbackChain(preferred)
	for _, model := range chain {
		if err := pinger.PingModel(ctx, model); err == nil {
			return model
		}
	}

	// Last resort: default model
	return r.config.DefaultModel
}

func (r *Router) ForceModel(name string) (*Model, error) {
	return r.config.GetModel(name)
}

func (r *Router) ListModels() []Model {
	return r.config.Models
}

func (r *Router) GetDefaultModel() string {
	return r.config.DefaultModel
}

func ClassifyTask(query string) TaskComplexity {
	lowerQuery := strings.ToLower(query)
	wordCount := len(strings.Fields(query))

	score := 0

	for _, indicator := range simpleIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score -= 2
		}
	}

	for _, indicator := range mediumIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score += 1
		}
	}

	for _, indicator := range complexIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score += 2
		}
	}

	for _, indicator := range advancedIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score += 3
		}
	}

	if wordCount > 50 {
		score += 2
	}

	if strings.Contains(lowerQuery, "why") || strings.Contains(lowerQuery, "reason") {
		score += 1
	}

	if strings.Contains(lowerQuery, "how") && wordCount > 10 {
		score += 1
	}

	switch {
	case score <= -2:
		return ComplexitySimple
	case score <= 1:
		return ComplexityMedium
	case score <= 4:
		return ComplexityComplex
	default:
		return ComplexityAdvanced
	}
}
