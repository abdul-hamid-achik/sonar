package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Context window steps used for recommendations. Values are chosen so Ollama
// can allocate clean KV sizes and operators can reason about them in the TUI.
var numCtxSteps = []int{
	8_192,
	16_384,
	32_768,
	49_152,
	65_536,
	98_304,
	131_072,
	196_608,
	262_144,
}

// Agent-oriented sweet spot when the host has enough RAM. Matches Ollama's
// guidance (≥64k for agents/coding) while leaving headroom on 16GB unified
// memory for a 9B Q4 model (measured ~8.5GB at 96k on M5/16GB).
const preferredAgentNumCtx = 98_304

// Conservative KV estimate (bytes per token) for planning. Slightly above the
// product default so recommendations under-allocate rather than OOM.
const numCtxKVBytesPerToken int64 = 40 << 10 // 40 KiB

const (
	numCtxOSReserveBytes    int64 = 5 << 30   // macOS + browser + MCPHub + terminal
	numCtxEmbedReserveBytes int64 = 512 << 20 // nomic-embed-text co-resident
	numCtxMinRecommend            = 8_192
	defaultUnknownWeight    int64 = 6 << 30 // exclusive 9B-class local weights
)

// NumCtxRecommendation is a host-side plan for ollama.num_ctx.
type NumCtxRecommendation struct {
	Current      int
	Recommended  int
	MaxSafe      int
	NativeMax    int
	TotalRAM     int64
	ModelWeight  int64
	AllowLarge   bool
	ClampedByEnv bool // recommended was reduced because ALLOW_LARGE is off
	Reason       string
}

// RecommendNumCtx chooses a durable local context allocation from host RAM,
// model weight, and the model's native maximum. It never invents a value above
// the native ceiling when that ceiling is known.
func RecommendNumCtx(totalRAM, modelWeightBytes int64, nativeMax, current int) NumCtxRecommendation {
	rec := NumCtxRecommendation{
		Current:     current,
		NativeMax:   nativeMax,
		TotalRAM:    totalRAM,
		ModelWeight: modelWeightBytes,
		AllowLarge:  largeModelsAllowed(),
	}
	if modelWeightBytes <= 0 {
		modelWeightBytes = defaultUnknownWeight
		rec.ModelWeight = modelWeightBytes
	}
	if totalRAM <= 0 {
		rec.Recommended = 16_384
		rec.MaxSafe = 16_384
		rec.Reason = "host RAM unknown; keeping the modest 16k default"
		return rec
	}

	budget := totalRAM - numCtxOSReserveBytes - numCtxEmbedReserveBytes
	if budget < totalRAM/3 {
		budget = totalRAM * 2 / 3
	}
	kvBudget := budget - modelWeightBytes
	if kvBudget < 0 {
		rec.Recommended = numCtxMinRecommend
		rec.MaxSafe = numCtxMinRecommend
		rec.Reason = "model weights already leave little RAM; use the minimum agent window"
		return rec
	}

	maxByRAM := int(kvBudget / numCtxKVBytesPerToken)
	maxSafe := snapNumCtxDown(maxByRAM)
	if maxSafe < numCtxMinRecommend {
		maxSafe = numCtxMinRecommend
	}
	if nativeMax > 0 && maxSafe > nativeMax {
		maxSafe = snapNumCtxDown(nativeMax)
		if maxSafe < numCtxMinRecommend {
			maxSafe = min(nativeMax, numCtxMinRecommend)
		}
	}
	rec.MaxSafe = maxSafe

	recommended := maxSafe
	if maxSafe >= preferredAgentNumCtx {
		// Prefer the measured agent sweet spot over the absolute RAM ceiling so
		// 16GB machines do not jump straight to 128k under ordinary load.
		recommended = preferredAgentNumCtx
	}
	if !rec.AllowLarge && recommended > safeMaxNumCtx {
		rec.ClampedByEnv = true
		recommended = safeMaxNumCtx
	}
	if !rec.AllowLarge && rec.MaxSafe > safeMaxNumCtx {
		rec.MaxSafe = safeMaxNumCtx
	}
	rec.Recommended = recommended

	switch {
	case rec.ClampedByEnv:
		rec.Reason = fmt.Sprintf(
			"host can take more, but SONAR_ALLOW_LARGE_MODELS is unset so apply is capped at %d; set the env to use up to %d",
			safeMaxNumCtx, maxSafe,
		)
	case recommended == preferredAgentNumCtx && maxSafe > preferredAgentNumCtx:
		rec.Reason = fmt.Sprintf(
			"agent sweet spot %d fits this machine (max safe ~%d by RAM); raise with /context set if needed",
			recommended, maxSafe,
		)
	default:
		rec.Reason = fmt.Sprintf("largest step that fits estimated weights+KV under host RAM (max safe %d)", maxSafe)
	}
	return rec
}

func snapNumCtxDown(n int) int {
	if n <= 0 {
		return 0
	}
	best := 0
	for _, step := range numCtxSteps {
		if step <= n {
			best = step
		}
	}
	if best == 0 {
		return n
	}
	return best
}

// NormalizeNumCtx validates an explicit operator-chosen window.
func NormalizeNumCtx(value int) (int, error) {
	if value < 2_048 {
		return 0, fmt.Errorf("num_ctx must be at least 2048, got %d", value)
	}
	if value > 1_048_576 {
		return 0, fmt.Errorf("num_ctx %d exceeds the 1M hard ceiling", value)
	}
	return value, nil
}

// UpdateOllamaNumCtxFile rewrites the num_ctx field in a config file in place.
// It preserves surrounding comments and structure by only replacing the first
// top-level-ish `num_ctx:` assignment (map values are typically indented).
func UpdateOllamaNumCtxFile(path string, numCtx int) error {
	if _, err := NormalizeNumCtx(numCtx); err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("config path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("config file is not valid UTF-8")
	}
	original := string(data)
	re := regexp.MustCompile(`(?m)^([ \t]*num_ctx:[ \t]*)-?\d+[ \t]*$`)
	if !re.MatchString(original) {
		return fmt.Errorf("no num_ctx field found in %s; add ollama.num_ctx first", path)
	}
	updated := re.ReplaceAllString(original, fmt.Sprintf("${1}%d", numCtx))
	if updated == original {
		// Same value already present.
		return nil
	}
	tmp := path + ".numctx-tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

// ParseNumCtxArg accepts plain integers and common suffixes (k/K).
func ParseNumCtxArg(raw string) (int, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return 0, fmt.Errorf("empty num_ctx value")
	}
	multiplier := 1
	switch {
	case strings.HasSuffix(raw, "k"):
		multiplier = 1024
		raw = strings.TrimSuffix(raw, "k")
	case strings.HasSuffix(raw, "m"):
		multiplier = 1024 * 1024
		raw = strings.TrimSuffix(raw, "m")
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid num_ctx %q", raw)
	}
	if n > 0 && multiplier > 1 && n > 1_048_576/multiplier {
		return 0, fmt.Errorf("num_ctx %q overflows", raw)
	}
	return NormalizeNumCtx(n * multiplier)
}

// FormatBytesIEC formats a byte count for operator-facing status text.
func FormatBytesIEC(n int64) string {
	if n < 0 {
		n = 0
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
