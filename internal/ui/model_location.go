package ui

import "strings"

// currentModelSurfaceLabel keeps a non-local execution boundary ahead of the
// model name so narrow surfaces cannot truncate away the fact that prompts
// leave the machine. Unknown inventory retains the legacy name-only display.
func (m *Model) currentModelSurfaceLabel(compact bool) string {
	if m == nil {
		return ""
	}
	name := sanitizeTerminalSingleLine(m.model)
	if m.modelManager != nil && m.modelManager.RemoteProvider() {
		provider := sanitizeTerminalSingleLine(m.activeProviderName())
		if provider == "" {
			provider = "remote"
		}
		boundary := strings.ToUpper(provider) + " · remote prompts"
		if compact || name == "" {
			return boundary
		}
		return strings.Join([]string{boundary, name}, " · ")
	}
	descriptor, ok := m.ollamaModelDescriptor(m.model)
	if !ok {
		return name
	}
	boundary := ""
	switch descriptor.Source {
	case OllamaModelCloud:
		boundary = "CLOUD · remote prompts"
	case OllamaModelRemote:
		boundary = "REMOTE · remote prompts"
	}
	if boundary == "" || compact || name == "" {
		if boundary != "" {
			return boundary
		}
		return name
	}
	return strings.Join([]string{boundary, name}, " · ")
}

// currentModelReachabilityLabel is the model label with its reachability, and
// it is what every ambient surface paints.
//
// "offline" on its own has no subject, so it used to be appended to the welcome
// while the model name was printed somewhere else entirely — the reader had to
// join them. Keeping reachability attached to the label means the pair
// "qwen3.5:2b · offline" appears once, wherever this frame put identity.
func (m *Model) currentModelReachabilityLabel(compact bool) string {
	if m == nil {
		return ""
	}
	label := m.currentModelSurfaceLabel(compact)
	if !m.providerOffline {
		return label
	}
	if label == "" {
		return "offline"
	}
	return label + " · offline"
}

func (m *Model) currentModelIsNonLocal() bool {
	if m == nil {
		return false
	}
	if m.modelManager != nil && m.modelManager.RemoteProvider() {
		return true
	}
	descriptor, ok := m.ollamaModelDescriptor(m.model)
	return ok && descriptor.Source != OllamaModelLocal
}
