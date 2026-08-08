package ui

import (
	"fmt"
	"strings"
)

// /context status, as text. The bar-chart overlay (bare /context) is
// contextdoctor.go; this is the same question answered in a copyable form.
//
// This file once implemented auto/set/save num_ctx tuning inherited from
// local-agent. All of it is gone: every provider sonar can run against is
// hosted (ProviderProfile.IsRemote is constant true), so auto and set always
// errored on the RemoteProvider check, and save wrote an ollama.num_ctx that
// SetNumCtx no-ops for remote clients — persisted config with no effect on any
// request. The report below states what is actually true instead: the
// provider owns the window, and sonar's job is to compact before hitting it.
func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func (m *Model) contextWindowStatusReport() string {
	var b strings.Builder
	b.WriteString("Context window\n")
	fmt.Fprintf(&b, "  Model:   %s\n", emptyDash(m.model))
	if m.numCtx > 0 {
		fmt.Fprintf(&b, "  Window:  %d tokens\n", m.numCtx)
		if m.promptTokens > 0 {
			fmt.Fprintf(&b, "  Used:    %d tokens (%d%%)\n",
				m.promptTokens, min(100, max(0, m.promptTokens*100/m.numCtx)))
		}
	} else {
		b.WriteString("  Window:  unknown until the first response reports it\n")
	}
	b.WriteString("  The provider owns this window. As a session approaches it, sonar\n" +
		"  compacts earlier turns rather than truncating them. Bare /context\n" +
		"  draws where the next turn's prompt goes.\n")
	return b.String()
}
