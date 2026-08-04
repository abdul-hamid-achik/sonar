package ui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/goaladvisor"
)

const (
	maxCapabilityPhaseWidth  = 24
	maxCapabilityServerWidth = 32
	maxCapabilityToolWidth   = 48
	maxCapabilityRevision    = 40
	maxCapabilityCandidates  = 99
)

func sanitizeCapabilityRoute(route agent.CapabilityRoute) agent.CapabilityRoute {
	if strings.IndexFunc(route.Phase, unicode.IsControl) >= 0 ||
		strings.IndexFunc(route.Server, unicode.IsControl) >= 0 ||
		strings.IndexFunc(route.Tool, unicode.IsControl) >= 0 ||
		strings.IndexFunc(route.CatalogRevision, unicode.IsControl) >= 0 {
		return agent.CapabilityRoute{}
	}
	route.Phase = truncateDisplay(sanitizeTerminalSingleLine(route.Phase), maxCapabilityPhaseWidth)
	route.Server = truncateDisplay(safeToolIdentifier(route.Server), maxCapabilityServerWidth)
	route.Tool = truncateDisplay(safeToolIdentifier(route.Tool), maxCapabilityToolWidth)
	route.CatalogRevision = truncateDisplay(safeToolIdentifier(route.CatalogRevision), maxCapabilityRevision)
	if route.Status == "" && route.Server != "" && route.Tool != "" {
		// Source compatibility for older embeddings. Current Agent events always
		// carry a typed status.
		route.Status = agent.CapabilityRouteResolved
	}
	if !capabilityRouteStatusValid(route.Status) {
		return agent.CapabilityRoute{}
	}
	if route.Freshness != agent.CapabilityRouteFresh && route.Freshness != agent.CapabilityRouteCached {
		route.Freshness = agent.CapabilityRouteFreshnessUnknown
	}
	route.CandidateCount = min(max(0, route.CandidateCount), maxCapabilityCandidates)
	if route.Status != agent.CapabilityRouteResolved {
		route.Server = ""
		route.Tool = ""
	}
	return route
}

func capabilityRouteStatusValid(status agent.CapabilityRouteStatus) bool {
	switch status {
	case agent.CapabilityRouteResolved, agent.CapabilityRouteAmbiguous,
		agent.CapabilityRouteNoMatch, agent.CapabilityRouteUnavailable,
		agent.CapabilityRouteInvalid:
		return true
	default:
		return false
	}
}

func capabilityRouteRenderable(route agent.CapabilityRoute) bool {
	if route.Phase == "" || !capabilityRouteStatusValid(route.Status) {
		return false
	}
	if route.Status == agent.CapabilityRouteResolved {
		return route.Server != "" && route.Tool != ""
	}
	return true
}

func capabilityRouteDetail(route agent.CapabilityRoute) string {
	route = sanitizeCapabilityRoute(route)
	switch route.Status {
	case agent.CapabilityRouteAmbiguous, agent.CapabilityRouteNoMatch,
		agent.CapabilityRouteUnavailable, agent.CapabilityRouteInvalid:
		// Non-resolved state belongs in the label so it survives the working
		// line's progressive removal of optional detail at ordinary widths.
		return ""
	}
	return capabilityRouteStateDetail(route, route.Phase)
}

func capabilityRouteLabel(route agent.CapabilityRoute) string {
	label := "Suggested MCP"
	if route.Status == agent.CapabilityRouteResolved {
		server := capabilityRouteServerLabel(route.Server)
		action := friendlyRemoteAction(strings.TrimPrefix(route.Tool, route.Server+"_"))
		return label + " · " + server + " / " + action
	}
	switch route.Status {
	case agent.CapabilityRouteAmbiguous:
		label = "Route ambiguous · " + route.Phase
		if route.CandidateCount > 0 {
			label += " · " + fmt.Sprintf("%d candidates", route.CandidateCount)
		}
	case agent.CapabilityRouteNoMatch:
		label = "No capability match · " + route.Phase
	case agent.CapabilityRouteUnavailable:
		label = "Routing unavailable · " + route.Phase
	case agent.CapabilityRouteInvalid:
		label = "Routing response invalid · " + route.Phase
	}
	if route.Status != agent.CapabilityRouteResolved {
		label = capabilityRouteStateDetail(route, label)
	}
	return label
}

func capabilityRouteCompactLabel(route agent.CapabilityRoute) string {
	switch route.Status {
	case agent.CapabilityRouteResolved:
		return "MCP " + capabilityRouteServerLabel(route.Server)
	case agent.CapabilityRouteAmbiguous:
		return "Ambiguous"
	case agent.CapabilityRouteNoMatch:
		return "No match"
	case agent.CapabilityRouteUnavailable:
		return "Unavailable"
	case agent.CapabilityRouteInvalid:
		return "Invalid"
	default:
		return "MCP route"
	}
}

func capabilityRouteServerLabel(server string) string {
	if ecosystemIdentity(server) != "" {
		if label := strings.TrimSpace(describeEcosystemServer(server).label); label != "" {
			return label
		}
	}
	return humanizeToolIdentifier(server)
}

func capabilityRouteStateDetail(route agent.CapabilityRoute, detail string) string {
	if route.Freshness == agent.CapabilityRouteCached {
		detail += " · cached"
	}
	if route.Reconsidered {
		detail += " · reconsidered"
	}
	return detail
}

func goalCapabilityPhase(advice *goaladvisor.Advice) string {
	if advice == nil {
		return "implementation"
	}
	switch strings.ToLower(strings.TrimSpace(advice.Phase)) {
	case "orienting", "investigating":
		return "research"
	case "planned":
		return "planning"
	case "changing":
		return "implementation"
	case "verifying":
		return "verification"
	case "persisting":
		return "handoff"
	case "needs_human_decision", "blocked":
		return "decision"
	default:
		return "implementation"
	}
}

// handleCapabilityRoute records a renderable capability route while a turn is
// active.
func (m *Model) handleCapabilityRoute(msg CapabilityRouteMsg) {
	if m.state == StateWaiting || m.state == StateStreaming {
		m.capabilityRoute = nil
		route := sanitizeCapabilityRoute(msg.Route)
		if capabilityRouteRenderable(route) {
			m.capabilityRoute = &route
			last := route
			m.lastCapabilityRoute = &last
		}
	}
}
