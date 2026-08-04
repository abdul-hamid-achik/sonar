package ui

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// capabilityHealth is the shared user-facing vocabulary for optional tool
// connections. Individual connections are either connected or unavailable;
// the aggregate becomes degraded when both are present.
type capabilityHealth string

const (
	capabilityConnected   capabilityHealth = "connected"
	capabilityDegraded    capabilityHealth = "degraded"
	capabilityUnavailable capabilityHealth = "unavailable"
)

type ecosystemConnection struct {
	Name     string
	Label    string
	Role     string
	Health   capabilityHealth
	Detail   string
	Recovery string
}

type ecosystemDescriptor struct {
	label string
	role  string
	order int
}

var ecosystemDescriptors = map[string]ecosystemDescriptor{
	"mcphub":     {label: "MCPHub", role: "gateway and discovery", order: 0},
	"cortex":     {label: "Cortex", role: "goals and evidence", order: 1},
	"bob":        {label: "Bob", role: "repository contracts", order: 2},
	"monitor":    {label: "Monitor", role: "system diagnostics", order: 3},
	"codemap":    {label: "Codemap", role: "structural evidence", order: 4},
	"vecgrep":    {label: "Vecgrep", role: "semantic discovery", order: 5},
	"glyphrun":   {label: "Glyphrun", role: "terminal verification", order: 6},
	"glyph":      {label: "Glyphrun", role: "terminal verification", order: 6},
	"cairntrace": {label: "Cairntrace", role: "browser verification", order: 7},
	"cairn":      {label: "Cairntrace", role: "browser verification", order: 7},
	"vidtrace":   {label: "Vidtrace", role: "video evidence", order: 8},
	"hitspec":    {label: "Hitspec", role: "bounded web and HTTP", order: 9},
	"filecheap":  {label: "file.cheap", role: "artifact storage", order: 10},
	"fcheap":     {label: "file.cheap", role: "artifact storage", order: 10},
	"tinyvault":  {label: "TinyVault", role: "secret boundary", order: 11},
	"tvault":     {label: "TinyVault", role: "secret boundary", order: 11},
	"veclite":    {label: "VecLite", role: "vector storage", order: 12},
}

// projectEcosystemConnections reconciles the live registry names with the
// startup failure snapshot. A successful reconnect wins over a stale startup
// failure, so Runtime never reports the same server as both connected and
// unavailable.
func projectEcosystemConnections(connected []string, failed []FailedServer) []ecosystemConnection {
	live := make(map[string]string, len(connected))
	for _, name := range connected {
		name = strings.TrimSpace(name)
		if name != "" {
			live[strings.ToLower(name)] = name
		}
	}

	connections := make([]ecosystemConnection, 0, len(live)+len(failed))
	for _, name := range live {
		descriptor := describeEcosystemServer(name)
		connections = append(connections, ecosystemConnection{
			Name: name, Label: descriptor.label, Role: descriptor.role, Health: capabilityConnected,
		})
	}

	seenFailed := make(map[string]struct{}, len(failed))
	for _, failure := range failed {
		name := strings.TrimSpace(failure.Name)
		key := strings.ToLower(name)
		if name == "" {
			continue
		}
		if _, reconnected := live[key]; reconnected {
			continue
		}
		if _, duplicate := seenFailed[key]; duplicate {
			continue
		}
		seenFailed[key] = struct{}{}
		descriptor := describeEcosystemServer(name)
		connections = append(connections, ecosystemConnection{
			Name: name, Label: descriptor.label, Role: descriptor.role, Health: capabilityUnavailable,
			Detail: compactConnectionFailure(failure.Reason), Recovery: connectionRecovery(name, failure.Reason),
		})
	}

	sort.SliceStable(connections, func(i, j int) bool {
		left, right := describeEcosystemServer(connections[i].Name), describeEcosystemServer(connections[j].Name)
		if left.order != right.order {
			return left.order < right.order
		}
		return strings.ToLower(connections[i].Label) < strings.ToLower(connections[j].Label)
	})
	return connections
}

func describeEcosystemServer(name string) ecosystemDescriptor {
	identity := ecosystemIdentity(name)
	if descriptor, ok := ecosystemDescriptors[identity]; ok {
		return descriptor
	}
	// Unknown servers keep their exact safe configuration identity so recovery
	// instructions point at the name users can actually find and edit.
	label := safeToolIdentifier(name)
	if label == "" {
		label = "MCP server"
	}
	return ecosystemDescriptor{label: label, role: "MCP tools", order: 100}
}

func ecosystemIdentity(name string) string {
	canonical := strings.ToLower(strings.TrimSpace(name))
	compact := strings.NewReplacer("-", "", "_", "", " ", "").Replace(canonical)
	switch compact {
	case "mcphub":
		return "mcphub"
	case "file.cheap", "filecheap":
		return "filecheap"
	case "tinyvault":
		return "tinyvault"
	case "cairntrace":
		return "cairntrace"
	case "glyphrun":
		return "glyphrun"
	}
	for _, part := range strings.FieldsFunc(canonical, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if _, ok := ecosystemDescriptors[part]; ok {
			return part
		}
	}
	return ""
}

func summarizeConnectionHealth(connections []ecosystemConnection) string {
	connected, unavailable := 0, 0
	for _, connection := range connections {
		switch connection.Health {
		case capabilityConnected:
			connected++
		case capabilityUnavailable:
			unavailable++
		}
	}
	switch {
	case connected > 0 && unavailable > 0:
		return fmt.Sprintf("%s · %d connected · %d unavailable", capabilityDegraded, connected, unavailable)
	case connected > 0:
		return fmt.Sprintf("%s · %d %s", capabilityConnected, connected, pluralizeServer(connected))
	case unavailable > 0:
		return fmt.Sprintf("%s · 0 connected · %d unavailable", capabilityUnavailable, unavailable)
	default:
		// Optional MCP not being configured is an absence of transport, not a
		// failed transport or domain outcome.
		return "not configured · 0 servers"
	}
}

func (h capabilityHealth) String() string { return string(h) }

func pluralizeServer(count int) string {
	return pluralizeNoun(count, "server", "servers")
}

func pluralizeNoun(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func compactConnectionFailure(reason string) string {
	reason = sanitizeVisibleText(reason)
	if reason == "" {
		return "connection failed"
	}
	return truncateDisplay(reason, 96)
}

func connectionRecovery(name, reason string) string {
	lower := strings.ToLower(reason)
	label := describeEcosystemServer(name).label
	switch {
	case strings.Contains(lower, "executable file not found"), strings.Contains(lower, "command not found"), strings.Contains(lower, "no such file"):
		return fmt.Sprintf("Install %s or update its MCP command; sonar will reconnect.", label)
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "authentication"), strings.Contains(lower, "credential"):
		return fmt.Sprintf("Refresh %s credentials in its server config; sonar will reconnect.", label)
	case strings.Contains(lower, "connection refused"), strings.Contains(lower, "broken pipe"), strings.Contains(lower, "disconnected"):
		return fmt.Sprintf("Start %s or verify its URL; sonar will reconnect.", label)
	case strings.Contains(lower, "deadline exceeded"), strings.Contains(lower, "timed out"), strings.Contains(lower, "timeout"):
		return fmt.Sprintf("Check %s health and retry; sonar will reconnect in the background.", label)
	default:
		return fmt.Sprintf("Check %s in sonar.yaml or the XDG config; sonar will reconnect.", label)
	}
}

func sanitizeVisibleText(text string) string {
	text = strings.TrimSpace(ansi.Strip(text))
	if text == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			// Drop terminal controls from health and compact error surfaces.
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
