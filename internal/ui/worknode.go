package ui

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/expertteam"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	maxWorkNodeIDBytes    = 64
	maxWorkNodeLabelBytes = maxExpertProgressNameBytes
	maxWorkNodeModelBytes = maxExpertProgressModelBytes
	maxWorkNodeRevision   = 1 << 20
)

// WorkNodeKind is a host-owned discriminator for one generic unit of work.
// Provider/runtime strings never become kinds implicitly.
type WorkNodeKind string

const (
	WorkNodeKindExpert WorkNodeKind = "expert"
)

// WorkNodeLocation is the UI-owned execution-location projection. The source
// expert adapter currently receives Ollama inventory facts, but presentation
// code must not depend on an Ollama-specific type.
type WorkNodeLocation string

const (
	WorkNodeLocationUnknown    WorkNodeLocation = "unknown"
	WorkNodeLocationLocal      WorkNodeLocation = "local"
	WorkNodeLocationCloud      WorkNodeLocation = "cloud"
	WorkNodeLocationRemoteHost WorkNodeLocation = "remote-host"
)

// WorkNodeStatus is the bounded presentation lifecycle for one unit of agent
// work. It does not mirror provider events or grant arbitrary status text
// authority to a child agent.
type WorkNodeStatus uint8

const (
	WorkNodeQueued WorkNodeStatus = iota + 1
	WorkNodeRunning
	WorkNodeWaiting
	WorkNodeCompleted
	WorkNodeAttention
	WorkNodeFailed
	WorkNodeCancelled
)

// WorkNodeActivity is a host-authored, bounded description of what a work
// node is doing. It deliberately does not admit provider prose, child status
// text, filenames, prompts, or other arbitrary activity payloads.
type WorkNodeActivity string

const (
	WorkNodeActivityQueued    WorkNodeActivity = "awaiting-assignment"
	WorkNodeActivityRunning   WorkNodeActivity = "consulting"
	WorkNodeActivityWaiting   WorkNodeActivity = "waiting"
	WorkNodeActivityCompleted WorkNodeActivity = "consultation-complete"
	WorkNodeActivityAttention WorkNodeActivity = "needs-attention"
	WorkNodeActivityFailed    WorkNodeActivity = "consultation-failed"
	WorkNodeActivityCancelled WorkNodeActivity = "cancelled"
)

// WorkNode is the safe, host-owned projection rendered for agent work.
//
// ReportRef is a typed durable artifact digest, never report text. The current
// consult_experts runtime emits advisory text but no durable artifact receipt,
// so its adapter leaves ReportRef nil. Unread is process-local presentation
// state and is deliberately excluded from persistence.
type WorkNode struct {
	ID          string                    `json:"id"`
	ParentID    BlockID                   `json:"parent_id,omitempty"`
	Index       int                       `json:"index"`
	Kind        WorkNodeKind              `json:"kind"`
	Label       string                    `json:"label,omitempty"`
	Model       string                    `json:"model,omitempty"`
	Location    WorkNodeLocation          `json:"location"`
	Status      WorkNodeStatus            `json:"status"`
	Activity    WorkNodeActivity          `json:"activity"`
	Elapsed     time.Duration             `json:"elapsed,omitempty"`
	Unread      int                       `json:"-"`
	Revision    uint64                    `json:"revision"`
	ReportRef   *ecosystem.ArtifactDigest `json:"report_ref,omitempty"`
	FailureCode string                    `json:"failure_code,omitempty"`
	EvalTokens  int                       `json:"eval_tokens,omitempty"`
}

func (node WorkNode) valid(total int) bool {
	if total < 1 || total > maxExpertProgressItems || node.Index < 0 || node.Index >= total ||
		!validWorkNodeID(node.ID) || node.EvalTokens < 0 || node.EvalTokens > maxExpertProgressTokens ||
		node.Kind != WorkNodeKindExpert || !validWorkNodeLocation(node.Location) ||
		(node.ParentID != "" && !node.ParentID.Valid()) ||
		node.Elapsed < 0 || node.Elapsed > maxToolViewDuration ||
		node.Revision == 0 || node.Revision > maxWorkNodeRevision ||
		node.Unread < 0 || uint64(node.Unread) > node.Revision ||
		node.Activity != workNodeActivityForStatus(node.Status) ||
		node.Revision != workNodeRevisionForStatus(node.Status) ||
		(node.ReportRef != nil &&
			(node.Status != WorkNodeCompleted || !validWorkReportRef(*node.ReportRef))) {
		return false
	}

	switch node.Status {
	case WorkNodeQueued:
		return node.Label == "" && node.Model == "" &&
			node.Location == WorkNodeLocationUnknown &&
			node.FailureCode == "" && node.EvalTokens == 0
	case WorkNodeRunning, WorkNodeWaiting:
		return validWorkNodeIdentity(node) && node.FailureCode == "" && node.EvalTokens == 0
	case WorkNodeCompleted:
		return validWorkNodeIdentity(node) && node.FailureCode == ""
	case WorkNodeAttention, WorkNodeFailed:
		return validWorkNodeIdentity(node) && validExpertFailureCode(node.FailureCode)
	case WorkNodeCancelled:
		return validWorkNodeIdentity(node) && node.FailureCode == "cancelled"
	default:
		return false
	}
}

func workNodeActivityForStatus(status WorkNodeStatus) WorkNodeActivity {
	switch status {
	case WorkNodeQueued:
		return WorkNodeActivityQueued
	case WorkNodeRunning:
		return WorkNodeActivityRunning
	case WorkNodeWaiting:
		return WorkNodeActivityWaiting
	case WorkNodeCompleted:
		return WorkNodeActivityCompleted
	case WorkNodeAttention:
		return WorkNodeActivityAttention
	case WorkNodeFailed:
		return WorkNodeActivityFailed
	case WorkNodeCancelled:
		return WorkNodeActivityCancelled
	default:
		return ""
	}
}

func workNodeRevisionForStatus(status WorkNodeStatus) uint64 {
	switch status {
	case WorkNodeQueued:
		return 1
	case WorkNodeRunning, WorkNodeWaiting:
		return 2
	case WorkNodeCompleted, WorkNodeAttention, WorkNodeFailed, WorkNodeCancelled:
		return 3
	default:
		return 0
	}
}

func workNodeActivityLabel(activity WorkNodeActivity) string {
	switch activity {
	case WorkNodeActivityQueued:
		return "awaiting assignment"
	case WorkNodeActivityRunning:
		return "consulting"
	case WorkNodeActivityWaiting:
		return "waiting"
	case WorkNodeActivityCompleted:
		return "consultation complete"
	case WorkNodeActivityAttention:
		return "needs attention"
	case WorkNodeActivityFailed:
		return "consultation failed"
	case WorkNodeActivityCancelled:
		return "cancelled"
	default:
		return ""
	}
}

func workNodeActivitySummary(node WorkNode, profiles ...GlyphProfile) string {
	parts := make([]string, 0, 3)
	if activity := workNodeActivityLabel(node.Activity); activity != "" {
		parts = append(parts, activity)
	}
	if node.Elapsed > 0 {
		parts = append(parts, formatWorkingElapsed(node.Elapsed))
	}
	if node.Unread > 0 {
		parts = append(parts, fmt.Sprintf("%d unread", node.Unread))
	}
	return strings.Join(parts, glyphSeparator(resolveGlyphProfile(profiles...)))
}

// workReportRefFromProjection admits only an exact normalized artifact receipt
// carrying supported evidence. It never accepts a URI or artifact ID on its
// own, because those scalars do not prove that a durable artifact exists.
func workReportRefFromProjection(projection ecosystem.ToolProjection) (*ecosystem.ArtifactDigest, bool) {
	normalized := projection.Normalize()
	if !reflect.DeepEqual(projection, normalized) ||
		normalized.Transport != ecosystem.TransportSucceeded ||
		(normalized.Domain != ecosystem.DomainSucceeded &&
			normalized.Domain != ecosystem.DomainAttention) ||
		normalized.Evidence != ecosystem.EvidenceSupported ||
		normalized.Artifact == nil {
		return nil, false
	}
	artifact := *normalized.Artifact
	return &artifact, true
}

func validWorkReportRef(artifact ecosystem.ArtifactDigest) bool {
	projection := ecosystem.ToolProjection{
		Role:      ecosystem.RoleArtifact,
		Transport: ecosystem.TransportSucceeded,
		Domain:    ecosystem.DomainSucceeded,
		Evidence:  ecosystem.EvidenceSupported,
		Artifact:  &artifact,
	}
	switch artifact.Kind {
	case ecosystem.ArtifactDigestFileCheapStash:
		projection.Specialist = "filecheap"
		projection.Operation = "filecheap_save"
	case ecosystem.ArtifactDigestHitspecCapture:
		projection.Specialist = "hitspec"
		projection.Operation = "hitspec_capture_webpage"
	default:
		return false
	}
	ref, ok := workReportRefFromProjection(projection)
	return ok && ref != nil && reflect.DeepEqual(*ref, artifact)
}

func validWorkNodeLocation(location WorkNodeLocation) bool {
	switch location {
	case WorkNodeLocationUnknown, WorkNodeLocationLocal,
		WorkNodeLocationCloud, WorkNodeLocationRemoteHost:
		return true
	default:
		return false
	}
}

func workNodeLocationFromExpertSource(location llm.OllamaModelLocation) (WorkNodeLocation, bool) {
	switch location {
	case llm.OllamaModelLocationUnknown:
		return WorkNodeLocationUnknown, true
	case llm.OllamaModelLocationLocal:
		return WorkNodeLocationLocal, true
	case llm.OllamaModelLocationCloud:
		return WorkNodeLocationCloud, true
	case llm.OllamaModelLocationRemote:
		return WorkNodeLocationRemoteHost, true
	default:
		return "", false
	}
}

func validWorkNodeIdentity(node WorkNode) bool {
	return boundedExpertProgressIdentifier(node.Label, maxWorkNodeLabelBytes) &&
		boundedExpertProgressIdentifier(node.Model, maxWorkNodeModelBytes)
}

func validWorkNodeID(value string) bool {
	if !utf8.ValidString(value) || value == "" || len(value) > maxWorkNodeIDBytes ||
		strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || (r < 'a' || r > 'z') &&
			(r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '-' && r != '_' && r != ':' {
			return false
		}
	}
	return true
}

// workNodesFromExpertProgress is a pure adapter from the consultation-specific
// state into the generic work projection. It clones and validates its input,
// assigns only host-derived IDs, and returns nodes in canonical scheduler
// order. Presentation code may derive a different, non-authoritative order
// without changing this identity-bearing projection.
func workNodesFromExpertProgress(state *ExpertProgressState) ([]WorkNode, bool) {
	return workNodesFromExpertProgressForParent(state, "")
}

func workNodesFromExpertProgressForParent(
	state *ExpertProgressState,
	parentID BlockID,
) ([]WorkNode, bool) {
	if parentID != "" && !parentID.Valid() {
		return nil, false
	}
	candidate := cloneExpertProgressState(state)
	if candidate != nil {
		// ExpertProgressState historically represents untouched queue slots with
		// the Go zero value. Normalize that internal absence to the explicit
		// public "unknown" location before applying the strict snapshot gate.
		for index := range candidate.Experts {
			item := &candidate.Experts[index]
			if item.Expert == "" && item.Model == "" && item.Phase == "" && item.Location == "" {
				item.Location = llm.OllamaModelLocationUnknown
			}
		}
	}
	safe := sanitizeExpertProgressState(candidate, false)
	if safe == nil {
		return nil, false
	}

	nodes := make([]WorkNode, safe.Total)
	for index, item := range safe.Experts {
		node := WorkNode{
			ID:       fmt.Sprintf("expert-%02d", index),
			ParentID: parentID,
			Index:    index,
			Kind:     WorkNodeKindExpert,
			Status:   WorkNodeQueued,
			Location: WorkNodeLocationUnknown,
		}
		if item.Expert != "" {
			location, ok := workNodeLocationFromExpertSource(item.Location)
			if !ok {
				return nil, false
			}
			node.Label = item.Expert
			node.Model = item.Model
			node.Location = location
			node.FailureCode = item.FailureCode
			node.EvalTokens = item.EvalTokens
			switch item.Phase {
			case expertteam.ProgressStarted:
				node.Status = WorkNodeRunning
			case expertteam.ProgressCompleted:
				node.Status = WorkNodeCompleted
			case expertteam.ProgressFailed:
				if item.FailureCode == "cancelled" {
					node.Status = WorkNodeCancelled
				} else {
					node.Status = WorkNodeFailed
				}
			default:
				return nil, false
			}
		}
		node.Activity = workNodeActivityForStatus(node.Status)
		node.Revision = workNodeRevisionForStatus(node.Status)
		if !node.valid(safe.Total) {
			return nil, false
		}
		nodes[index] = node
	}
	return orderedWorkNodes(nodes), true
}

func orderedWorkNodes(nodes []WorkNode) []WorkNode {
	ordered := append([]WorkNode(nil), nodes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.Index != right.Index {
			return left.Index < right.Index
		}
		return left.ID < right.ID
	})
	return ordered
}

// presentedWorkNodes derives the bounded inline reading order without changing
// scheduler identity. Live work and work needing attention must be considered
// before queued and terminal work so an old settled prefix cannot crowd the
// currently useful rows out of a capped surface. Index and ID keep each class
// deterministic across refreshes and resize.
func presentedWorkNodes(nodes []WorkNode) []WorkNode {
	presented := append([]WorkNode(nil), nodes...)
	sort.SliceStable(presented, func(i, j int) bool {
		left, right := presented[i], presented[j]
		leftRank, rightRank := workNodePresentationRank(left.Status), workNodePresentationRank(right.Status)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Index != right.Index {
			return left.Index < right.Index
		}
		return left.ID < right.ID
	})
	return presented
}

func workNodePresentationRank(status WorkNodeStatus) int {
	switch status {
	case WorkNodeRunning, WorkNodeWaiting, WorkNodeAttention:
		return 0
	case WorkNodeQueued:
		return 1
	case WorkNodeCompleted, WorkNodeFailed, WorkNodeCancelled:
		return 2
	default:
		// Invalid states should already have failed the projection gate. Keep an
		// impossible value last if this pure helper is exercised independently.
		return 3
	}
}
