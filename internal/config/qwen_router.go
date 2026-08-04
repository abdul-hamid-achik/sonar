package config

import (
	"context"
	"strings"
	"sync"
)

// QwenModelRouter is optimized for Qwen 3.5 variant selection (0.8B, 2B, 4B, 9B)
// It uses improved heuristics for small model capabilities and mode-aware routing.
type QwenModelRouter struct {
	config      *ModelConfig
	overrideLog []ModelOverride
	modeContext ModeContext // Current operational mode (ASK/PLAN/BUILD)
	available   map[string]struct{}
	mu          sync.RWMutex
}

var _ ModelRouter = (*QwenModelRouter)(nil)

// ModeContext represents the current operational mode
type ModeContext int

const (
	ModeAskContext ModeContext = iota
	ModePlanContext
	ModeBuildContext
	ModeNeutralContext
)

// Qwen-specific complexity levels mapped to model sizes
type QwenComplexity string

const (
	QwenTrivial  QwenComplexity = "trivial"  // 0.8B - simple facts, single operations
	QwenSimple   QwenComplexity = "simple"   // 2B - basic reasoning, simple tools
	QwenModerate QwenComplexity = "moderate" // 4B - multi-step, analysis
	QwenAdvanced QwenComplexity = "advanced" // 9B - complex reasoning, architecture
)

// Qwen-specific indicator lists with optimized patterns
var (
	// Trivial tasks perfect for 0.8B - ultra-fast responses
	qwenTrivialIndicators = []string{
		"what is", "who is", "when is", "where is",
		"define", "meaning of", "synonym", "antonym",
		"list files", "show me", "display",
		"yes", "no", "ok", "thanks",
		"hello", "hi", "hey",
	}

	// Simple tasks for 2B - basic reasoning with tool use
	qwenSimpleIndicators = []string{
		"how do i", "explain", "what does", "why does",
		"find", "search", "get", "read",
		"print", "echo", "cat", "ls", "grep",
		"simple", "quick", "fast", "brief",
		"check", "verify", "test",
		"create file", "write file", "save",
	}

	// Moderate tasks for 4B - multi-step reasoning
	qwenModerateIndicators = []string{
		"create", "generate", "add", "modify", "update",
		"fix", "debug", "refactor", "optimize",
		"function", "class", "method", "interface",
		"test", "unit test", "integration test",
		"script", "command", "pipeline",
		"compare", "analyze", "review",
		"multiple", "several", "across",
	}

	// Advanced tasks requiring 9B - complex system-level thinking
	qwenAdvancedIndicators = []string{
		"architecture", "design pattern", "system design",
		"infrastructure", "deployment", "scaling",
		"security audit", "performance optimization",
		"multi-step", "complex", "comprehensive",
		"build a", "implement", "develop", "engineer",
		"full stack", "end-to-end", "production",
		"migration", "refactor entire", "rewrite",
	}

	// Programming language-specific patterns
	qwenCodePatterns = map[string]QwenComplexity{
		// Simple patterns - 2B handles well
		"variable":  QwenSimple,
		"constant":  QwenSimple,
		"function":  QwenSimple,
		"loop":      QwenSimple,
		"condition": QwenSimple,
		"array":     QwenSimple,
		"slice":     QwenSimple,
		"map":       QwenSimple,

		// Moderate patterns - 4B recommended
		"struct":      QwenModerate,
		"interface":   QwenModerate,
		"generics":    QwenModerate,
		"concurrency": QwenModerate,
		"goroutine":   QwenModerate,
		"channel":     QwenModerate,
		"mutex":       QwenModerate,

		// Advanced patterns - 9B recommended
		"architecture": QwenAdvanced,
		"pattern":      QwenAdvanced,
		"microservice": QwenAdvanced,
		"distributed":  QwenAdvanced,
		"kubernetes":   QwenAdvanced,
	}
)

// NewQwenModelRouter creates an optimized router for Qwen models
func NewQwenModelRouter(cfg *ModelConfig) *QwenModelRouter {
	return &QwenModelRouter{
		config:      cfg,
		overrideLog: make([]ModelOverride, 0),
		modeContext: ModeAskContext,
	}
}

// SetModeContext updates the operational mode for routing decisions
func (r *QwenModelRouter) SetModeContext(mode ModeContext) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modeContext = mode
}

// ClassifyTaskComplexity returns the complexity level for a query
func (r *QwenModelRouter) ClassifyTaskComplexity(query string) QwenComplexity {
	r.mu.RLock()
	mode := r.modeContext
	r.mu.RUnlock()
	return classifyQwenTask(query, mode)
}

// SelectModel returns the optimal model for the query
func (r *QwenModelRouter) SelectModel(query string) string {
	complexity := classifyQwenTask(query, ModeNeutralContext)
	genericComplexity := ComplexityMedium
	switch complexity {
	case QwenTrivial:
		genericComplexity = ComplexitySimple
	case QwenSimple:
		genericComplexity = ComplexityMedium
	case QwenModerate:
		genericComplexity = ComplexityComplex
	case QwenAdvanced:
		genericComplexity = ComplexityAdvanced
	}
	return selectAvailableAutoModel(r.config.SelectModelForTask(string(genericComplexity)), r.config, r.availableSnapshot())
}

// SelectModelForMode returns the optimal model for the current mode and query
func (r *QwenModelRouter) SelectModelForMode(query string, mode ModeContext) string {
	return PromoteModelForMode(r, r.SelectModel(query), mode)
}

func (r *QwenModelRouter) GetModelForCapability(capability ModelCapability) string {
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

func (r *QwenModelRouter) SetAvailableModels(models []string) {
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

func (r *QwenModelRouter) ResolveAvailableModel(preferred string) string {
	return selectAvailableModel(preferred, r.config, r.availableSnapshot())
}

func (r *QwenModelRouter) availableSnapshot() map[string]struct{} {
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

func (r *QwenModelRouter) ListModels() []Model {
	return r.config.Models
}

// classifyQwenTask classifies a query using Qwen-optimized heuristics
func classifyQwenTask(query string, mode ModeContext) QwenComplexity {
	lowerQuery := strings.ToLower(query)
	words := strings.Fields(lowerQuery)
	wordCount := len(words)

	// Start with base score
	score := 0

	// Check trivial indicators first (strong negative weight)
	for _, indicator := range qwenTrivialIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score -= 4
		}
	}

	// Check simple indicators
	for _, indicator := range qwenSimpleIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score -= 1
		}
	}

	// Check moderate indicators
	for _, indicator := range qwenModerateIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score += 2
		}
	}

	// Check advanced indicators
	for _, indicator := range qwenAdvancedIndicators {
		if strings.Contains(lowerQuery, indicator) {
			score += 4
		}
	}

	// Check code patterns
	for pattern, complexity := range qwenCodePatterns {
		if strings.Contains(lowerQuery, pattern) {
			switch complexity {
			case QwenSimple:
				score -= 1
			case QwenModerate:
				score += 2
			case QwenAdvanced:
				score += 4
			}
		}
	}

	// Word count adjustments
	if wordCount > 50 {
		score += 3 // Very long queries likely complex
	} else if wordCount > 30 {
		score += 1
	} else if wordCount < 5 && score <= 0 {
		score -= 2 // Very short queries likely simple
	}

	// Question type adjustments
	if strings.Contains(lowerQuery, "why") || strings.Contains(lowerQuery, "reason") {
		score += 2 // Why questions need reasoning
	}
	if strings.Contains(lowerQuery, "how") && wordCount > 10 {
		score += 1 // Complex how questions
	}
	if strings.Contains(lowerQuery, "?") && wordCount < 10 {
		score -= 1 // Simple questions
	}

	// Mode-based adjustments
	switch mode {
	case ModeAskContext:
		score -= 1 // Prefer smaller models for ASK
	case ModeBuildContext:
		score += 1 // Prefer larger models for BUILD
	}

	// Map score to Qwen complexity
	switch {
	case score <= -3:
		return QwenTrivial
	case score <= 1:
		return QwenSimple
	case score <= 5:
		return QwenModerate
	default:
		return QwenAdvanced
	}
}

// RecordOverride logs user model selection for learning
func (r *QwenModelRouter) RecordOverride(query, userModel string) {
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

// GetLearnedPatterns returns word->complexity mappings from user overrides
func (r *QwenModelRouter) GetLearnedPatterns() map[string]QwenComplexity {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.overrideLog) < 3 {
		return nil
	}

	wordCounts := make(map[string]map[QwenComplexity]int)

	for _, o := range r.overrideLog {
		if o.Query == "" || o.UserModel == "" {
			continue
		}

		// Determine complexity from user-selected model
		var complexity QwenComplexity
		switch {
		case strings.Contains(o.UserModel, "0.8b"):
			complexity = QwenTrivial
		case strings.Contains(o.UserModel, "2b"):
			complexity = QwenSimple
		case strings.Contains(o.UserModel, "4b"):
			complexity = QwenModerate
		case strings.Contains(o.UserModel, "9b"):
			complexity = QwenAdvanced
		default:
			continue
		}

		words := strings.Fields(strings.ToLower(o.Query))
		for _, w := range words {
			if len(w) < 3 {
				continue
			}
			if _, ok := wordCounts[w]; !ok {
				wordCounts[w] = make(map[QwenComplexity]int)
			}
			wordCounts[w][complexity]++
		}
	}

	// Find dominant complexity for each word
	wordComplexity := make(map[string]QwenComplexity)
	for word, counts := range wordCounts {
		var maxCount int
		var dominant QwenComplexity
		for c, cnt := range counts {
			if cnt > maxCount {
				maxCount = cnt
				dominant = c
			}
		}
		if maxCount >= 2 {
			wordComplexity[word] = dominant
		}
	}

	return wordComplexity
}

// SelectAvailableModelForTask with Qwen-optimized fallback
func (r *QwenModelRouter) SelectAvailableModelForTask(ctx context.Context, pinger ModelPinger, query string) string {
	preferred := r.SelectModel(query)

	// Qwen-optimized fallback chain. Capped at the 4B tier: larger local
	// profiles (Qwen/Ornith 9B, Gemma ~7GB) are manual-exclusive on 16GB.
	fallbackOrder := []string{
		preferred,
		"qwen3.5:2b",
		"qwen3.5:0.8b",
		"qwen3.5:4b",
	}

	for _, model := range fallbackOrder {
		if err := pinger.PingModel(ctx, model); err == nil {
			return model
		}
	}

	return r.config.DefaultModel
}

// GetRecommendedModel returns a model recommendation with reasoning
func (r *QwenModelRouter) GetRecommendedModel(query string) (model string, reason string, complexity QwenComplexity) {
	r.mu.RLock()
	mode := r.modeContext
	r.mu.RUnlock()

	complexity = classifyQwenTask(query, mode)

	switch complexity {
	case QwenTrivial:
		model = "qwen3.5:0.8b"
		reason = "trivial task - ultra-fast response"
	case QwenSimple:
		model = "qwen3.5:2b"
		reason = "simple task - balanced speed/capability"
	case QwenModerate:
		model = "qwen3.5:4b"
		reason = "moderate complexity - multi-step reasoning"
	case QwenAdvanced:
		// Capped at 4B: Qwen/Ornith 9B and Gemma are manual-exclusive on 16GB.
		model = "qwen3.5:4b"
		reason = "advanced task - largest memory-safe local model (4B)"
	}

	// Adjust for mode
	switch mode {
	case ModeAskContext:
		reason += " (ASK mode - prefer speed)"
	case ModePlanContext:
		reason += " (PLAN mode - prefer reasoning)"
	case ModeBuildContext:
		reason += " (BUILD mode - prefer capability)"
	}

	return model, reason, complexity
}
