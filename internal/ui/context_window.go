package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"github.com/abdul-hamid-achik/sonar/internal/resource"
)

// numCtxAppliedMsg carries the result of an off-loop num_ctx change.
type numCtxAppliedMsg struct {
	Token uint64
	Value int
	Text  string
	Err   error
}

// handleContextWindowCommand implements /context status|auto|set.
//
// Validation is cheap and stays on the event loop; applying is not. SetNumCtx
// takes ModelManager's exclusive inference lock, which ChatStream holds for a
// whole streamed response — including the AI session-title job that starts the
// moment a turn settles, exactly when the composer becomes free to accept
// /context. Running it inside Update froze painting and every key, Ctrl+C
// included, for as long as that background stream lasted. The returned command
// is nil when nothing needs applying.
func (m *Model) handleContextWindowCommand(spec string) (string, tea.Cmd, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "status" {
		return m.contextWindowStatusReport(), nil, nil
	}
	if m.modelManager == nil {
		return "", nil, fmt.Errorf("model manager unavailable")
	}
	if m.modelManager.RemoteProvider() {
		return "", nil, fmt.Errorf("remote providers own their context window; /context only tunes local Ollama num_ctx")
	}

	switch {
	case spec == "auto":
		rec := m.recommendNumCtx()
		if rec.Recommended <= 0 {
			return "", nil, fmt.Errorf("could not recommend a num_ctx: %s", rec.Reason)
		}
		if rec.Recommended == m.modelManager.ConfiguredNumCtx() {
			return m.formatContextApplied(rec, rec.Recommended, false, "already at recommended value"), nil, nil
		}
		return "", m.beginNumCtxApply(
			rec.Recommended,
			m.formatContextApplied(rec, rec.Recommended, true, "applied recommendation for this process"),
		), nil

	case strings.HasPrefix(spec, "set:"):
		raw := strings.TrimPrefix(spec, "set:")
		value, err := config.ParseNumCtxArg(raw)
		if err != nil {
			return "", nil, err
		}
		rec := m.recommendNumCtx()
		if rec.MaxSafe > 0 && value > rec.MaxSafe {
			return "", nil, fmt.Errorf(
				"num_ctx %d exceeds estimated max safe %d on this host (%s total RAM); pick a lower value or free memory",
				value, rec.MaxSafe, config.FormatBytesIEC(rec.TotalRAM),
			)
		}
		if !rec.AllowLarge && value > 32_768 {
			return "", nil, fmt.Errorf(
				"num_ctx %d needs SONAR_ALLOW_LARGE_MODELS=1 (host clamp is 32768 without it)",
				value,
			)
		}
		return "", m.beginNumCtxApply(
			value,
			m.formatContextApplied(rec, value, true, "applied explicit value for this process"),
		), nil
	default:
		return "", nil, fmt.Errorf("unknown /context action %q", spec)
	}
}

// beginNumCtxApply moves the blocking manager call off Bubble Tea's event loop.
// A token drops a late result that a newer /context has already superseded.
func (m *Model) beginNumCtxApply(value int, receipt string) tea.Cmd {
	m.numCtxApplyToken++
	token := m.numCtxApplyToken
	manager := m.modelManager
	return func() tea.Msg {
		var err error
		if manager == nil {
			err = fmt.Errorf("model manager unavailable")
		} else {
			err = manager.SetNumCtx(value)
		}
		return numCtxAppliedMsg{Token: token, Value: value, Text: receipt, Err: err}
	}
}

// handleNumCtxApplied records the outcome back on the event loop, where the
// effective-context resync and transcript refresh belong.
func (m *Model) handleNumCtxApplied(msg numCtxAppliedMsg) tea.Cmd {
	if msg.Token != m.numCtxApplyToken {
		return nil
	}
	if msg.Err != nil {
		m.entries = append(m.entries, ChatEntry{Kind: "error", Content: msg.Err.Error()})
	} else {
		m.syncEffectiveContext(false)
		m.entries = append(m.entries, ChatEntry{Kind: "system", Content: msg.Text})
	}
	m.refreshTranscript()
	m.resumeFollow()
	return nil
}

func (m *Model) saveConfiguredNumCtx() (string, error) {
	if m.modelManager == nil {
		return "", fmt.Errorf("model manager unavailable")
	}
	if m.configSourcePath == "" {
		return "", fmt.Errorf("no host config path is available to save; set ollama.num_ctx in your config.yaml manually")
	}
	value := m.modelManager.ConfiguredNumCtx()
	if value <= 0 {
		return "", fmt.Errorf("no local num_ctx is configured")
	}
	if err := config.UpdateOllamaNumCtxFile(m.configSourcePath, value); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"Saved ollama.num_ctx: %d\n  file: %s\n  Restart is not required for the current process; new launches load this value.",
		value, m.configSourcePath,
	), nil
}

func (m *Model) recommendNumCtx() config.NumCtxRecommendation {
	var totalRAM int64
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if snap, err := (resource.SystemProbe{}).Snapshot(ctx); err == nil {
		totalRAM = snap.TotalRAMBytes
	}

	current := 0
	modelName := m.model
	var weight int64
	native := 0
	if m.modelManager != nil {
		current = m.modelManager.ConfiguredNumCtx()
		if modelName == "" {
			modelName = m.modelManager.CurrentModel()
		}
		weight = m.modelManager.LocalModelWeightBytes(modelName)
		policy := m.modelManager.ContextPolicy(modelName)
		if policy.NativeKnown {
			native = policy.Native
		}
	}
	return config.RecommendNumCtx(totalRAM, weight, native, current)
}

func (m *Model) contextWindowStatusReport() string {
	rec := m.recommendNumCtx()
	var b strings.Builder
	b.WriteString("Context window (ollama.num_ctx)\n")
	fmt.Fprintf(&b, "  Model:            %s\n", emptyDash(m.model))
	fmt.Fprintf(&b, "  Current:          %d tokens\n", rec.Current)
	if rec.NativeMax > 0 {
		fmt.Fprintf(&b, "  Model native max: %d tokens\n", rec.NativeMax)
	} else {
		b.WriteString("  Model native max: unknown\n")
	}
	if rec.TotalRAM > 0 {
		fmt.Fprintf(&b, "  Host RAM:         %s\n", config.FormatBytesIEC(rec.TotalRAM))
	} else {
		b.WriteString("  Host RAM:         unknown\n")
	}
	if rec.ModelWeight > 0 {
		fmt.Fprintf(&b, "  Model weights:    %s (estimate)\n", config.FormatBytesIEC(rec.ModelWeight))
	}
	fmt.Fprintf(&b, "  Recommended:      %d tokens\n", rec.Recommended)
	fmt.Fprintf(&b, "  Max safe (est.):  %d tokens\n", rec.MaxSafe)
	fmt.Fprintf(&b, "  Large models env: %v\n", rec.AllowLarge)
	fmt.Fprintf(&b, "  Note: %s\n", rec.Reason)
	b.WriteString("\nCommands:\n")
	b.WriteString("  /context auto       apply the recommendation now\n")
	b.WriteString("  /context set 96k    set an explicit window (e.g. 65536, 96k)\n")
	b.WriteString("  /context save       write the active value into config.yaml\n")
	if rec.Current > 0 && rec.Recommended > 0 && rec.Current != rec.Recommended {
		fmt.Fprintf(&b, "\nSuggestion: /context auto  (move %d → %d)\n", rec.Current, rec.Recommended)
	}
	return b.String()
}

func (m *Model) formatContextApplied(rec config.NumCtxRecommendation, applied int, changed bool, detail string) string {
	var b strings.Builder
	if changed {
		fmt.Fprintf(&b, "Context window updated to %d tokens\n", applied)
	} else {
		fmt.Fprintf(&b, "Context window remains %d tokens\n", applied)
	}
	fmt.Fprintf(&b, "  %s\n", detail)
	fmt.Fprintf(&b, "  Effective now: %d\n", m.numCtx)
	if rec.MaxSafe > 0 {
		fmt.Fprintf(&b, "  Max safe est.: %d\n", rec.MaxSafe)
	}
	b.WriteString("  Persist with: /context save\n")
	if rec.Reason != "" {
		fmt.Fprintf(&b, "  Plan note: %s\n", rec.Reason)
	}
	return b.String()
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}
