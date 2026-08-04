package ui

import (
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// setCurrentModelProjection commits the presentation-side half of an already
// successful ModelManager switch. Context occupancy belongs to one model, so
// it must not be carried across a denominator change.
func (m *Model) setCurrentModelProjection(name string) {
	previous := config.CanonicalModelName(m.model)
	changed := previous != "" && previous != config.CanonicalModelName(name)
	m.model = name
	if changed && m.agent != nil {
		m.agent.CommitModelSwitch()
	}
	m.syncEffectiveContext(changed)
}

// syncEffectiveContext refreshes the context denominator from whichever
// authority currently owns it, and clears occupancy when the model changed.
func (m *Model) syncEffectiveContext(resetOccupancy bool) {
	if m.modelManager != nil {
		m.numCtx = m.modelManager.NumCtx()
	} else if m.agent != nil {
		if effective := m.agent.NumCtx(); effective > 0 {
			m.numCtx = effective
		}
	}
	if resetOccupancy {
		m.promptTokens = 0
	}
}
