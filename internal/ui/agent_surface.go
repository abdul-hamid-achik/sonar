package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
	"github.com/abdul-hamid-achik/sonar/internal/expertselector"
	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	// The Agent Hub is an inspection surface, not an unbounded second copy of
	// the transcript. Keep every live group and the newest settled groups within
	// this ceiling; OmittedGroups lets the presentation disclose the truncation.
	maxAgentSurfaceGroups = 64
	agentNodeIDEntropy    = 16
)

// AgentSurfaceProjection is the complete bounded, presentation-safe view of
// agent work derivable from the semantic transcript. It deliberately carries
// no tool arguments/results, provider payloads, child prompts, reasoning,
// reports, paths, or arbitrary metadata.
type AgentSurfaceProjection struct {
	Groups        []AgentGroupProjection `json:"groups,omitempty"`
	OmittedGroups int                    `json:"omitted_groups,omitempty"`
}

// AgentGroupProjection binds one expert consultation to its causal transcript
// identity. ToolIndex is an ephemeral UI correlation only and is intentionally
// excluded from persistence; ID is the durable host-admitted identity.
//
// Lifecycle says whether the group is live or terminal. It does not claim
// domain success: per-node host-authored status counts remain separate, and a
// missing progress projection never becomes success.
type AgentGroupProjection struct {
	ID                BlockID                 `json:"id"`
	TurnID            TurnID                  `json:"turn_id"`
	Revision          uint64                  `json:"revision"`
	Lifecycle         BlockLifecycle          `json:"lifecycle"`
	Elapsed           time.Duration           `json:"elapsed,omitempty"`
	Strategy          expertselector.Strategy `json:"strategy,omitempty"`
	Total             int                     `json:"total,omitempty"`
	Queued            int                     `json:"queued,omitempty"`
	Running           int                     `json:"running,omitempty"`
	Waiting           int                     `json:"waiting,omitempty"`
	Completed         int                     `json:"completed,omitempty"`
	Attention         int                     `json:"attention,omitempty"`
	Failed            int                     `json:"failed,omitempty"`
	Cancelled         int                     `json:"cancelled,omitempty"`
	ProgressAvailable bool                    `json:"progress_available"`
	Interrupted       bool                    `json:"interrupted,omitempty"`
	Nodes             []WorkNode              `json:"nodes,omitempty"`
	ToolIndex         int                     `json:"-"`
}

type indexedAgentGroup struct {
	entryIndex int
	group      AgentGroupProjection
}

// projectAgentSurface builds the generic agent projection from existing
// transcript/tool state without mutating either input. Groups retain causal
// transcript order; nodes retain scheduler index order. When history exceeds
// the bounded surface, all live groups survive and the newest terminal groups
// fill the remaining slots.
func projectAgentSurface(entries []ChatEntry, tools []ToolEntry) (AgentSurfaceProjection, error) {
	return projectAgentSurfaceAt(entries, tools, time.Time{})
}

// projectAgentSurfaceAt derives live consultation elapsed time from the
// host-owned tool start receipt. Passing a zero clock preserves the last
// admitted duration and keeps pure callers deterministic.
func projectAgentSurfaceAt(
	entries []ChatEntry,
	tools []ToolEntry,
	now time.Time,
) (AgentSurfaceProjection, error) {
	var (
		active         []indexedAgentGroup
		recentSettled  []indexedAgentGroup
		candidateCount int
	)
	seenGroups := make(map[BlockID]struct{})
	seenTools := make(map[int]struct{})

	for entryIndex, entry := range entries {
		if entry.Kind != "tool_group" {
			continue
		}
		if entry.ToolIndex < 0 || entry.ToolIndex >= len(tools) {
			return AgentSurfaceProjection{}, fmt.Errorf(
				"agent surface entry %d references missing tool index %d",
				entryIndex,
				entry.ToolIndex,
			)
		}
		tool := tools[entry.ToolIndex]
		if !isExpertConsultTool(tool.Name) {
			continue
		}
		if _, duplicate := seenGroups[entry.BlockID]; duplicate {
			return AgentSurfaceProjection{}, fmt.Errorf("agent surface repeats a group identity at entry %d", entryIndex)
		}
		if _, duplicate := seenTools[entry.ToolIndex]; duplicate {
			return AgentSurfaceProjection{}, fmt.Errorf("agent surface repeats tool index %d", entry.ToolIndex)
		}

		group, err := projectAgentGroupAt(entry, tool, now)
		if err != nil {
			return AgentSurfaceProjection{}, fmt.Errorf("agent surface entry %d: %w", entryIndex, err)
		}
		seenGroups[group.ID] = struct{}{}
		seenTools[entry.ToolIndex] = struct{}{}
		candidateCount++

		indexed := indexedAgentGroup{entryIndex: entryIndex, group: group}
		if group.Lifecycle == BlockLive {
			if len(active) >= maxAgentSurfaceGroups {
				return AgentSurfaceProjection{}, fmt.Errorf(
					"agent surface has more than %d live groups",
					maxAgentSurfaceGroups,
				)
			}
			active = append(active, indexed)
			continue
		}
		recentSettled = appendBoundedAgentGroup(recentSettled, indexed, maxAgentSurfaceGroups)
	}

	settledSlots := max(0, maxAgentSurfaceGroups-len(active))
	if len(recentSettled) > settledSlots {
		recentSettled = recentSettled[len(recentSettled)-settledSlots:]
	}
	retained := append(active, recentSettled...)
	sort.SliceStable(retained, func(i, j int) bool {
		return retained[i].entryIndex < retained[j].entryIndex
	})

	surface := AgentSurfaceProjection{
		Groups:        make([]AgentGroupProjection, len(retained)),
		OmittedGroups: candidateCount - len(retained),
	}
	for index := range retained {
		surface.Groups[index] = retained[index].group
	}
	if !surface.valid() {
		return AgentSurfaceProjection{}, fmt.Errorf("agent surface projection is inconsistent")
	}
	return surface, nil
}

func appendBoundedAgentGroup(groups []indexedAgentGroup, group indexedAgentGroup, limit int) []indexedAgentGroup {
	if limit <= 0 {
		return nil
	}
	if len(groups) < limit {
		return append(groups, group)
	}
	copy(groups, groups[1:])
	groups[len(groups)-1] = group
	return groups
}

func projectAgentGroupAt(
	entry ChatEntry,
	tool ToolEntry,
	now time.Time,
) (AgentGroupProjection, error) {
	if !entry.BlockID.Valid() {
		return AgentGroupProjection{}, fmt.Errorf("group block identity is missing or invalid")
	}
	if !entry.TurnID.Valid() {
		return AgentGroupProjection{}, fmt.Errorf("group turn identity is missing or invalid")
	}
	if entry.Revision == 0 {
		return AgentGroupProjection{}, fmt.Errorf("group revision must start at one")
	}

	lifecycle, interrupted, err := projectedAgentGroupLifecycle(entry, tool)
	if err != nil {
		return AgentGroupProjection{}, err
	}
	group := AgentGroupProjection{
		ID:          entry.BlockID,
		TurnID:      entry.TurnID,
		Revision:    entry.Revision,
		Lifecycle:   lifecycle,
		Elapsed:     projectedAgentGroupElapsed(tool, now),
		Interrupted: interrupted,
		ToolIndex:   entry.ToolIndex,
	}
	if group.Elapsed < 0 || group.Elapsed > maxToolViewDuration {
		return AgentGroupProjection{}, fmt.Errorf("agent group elapsed time is invalid")
	}

	if tool.ExpertProgress == nil {
		if !group.valid() {
			return AgentGroupProjection{}, fmt.Errorf("agent group without progress is inconsistent")
		}
		return group, nil
	}
	if interrupted {
		// Restore cannot prove how each formerly live/queued child settled. Keep
		// the group interruption visible without fabricating child cancellation.
		return AgentGroupProjection{}, fmt.Errorf("interrupted agent group retained stale child progress")
	}

	requireSettled := tool.Status != ToolStatusRunning
	candidate := cloneExpertProgressState(tool.ExpertProgress)
	for index := range candidate.Experts {
		item := &candidate.Experts[index]
		if item.Expert == "" && item.Model == "" && item.Phase == "" && item.Location == "" {
			item.Location = llm.OllamaModelLocationUnknown
		}
	}
	safe := sanitizeExpertProgressState(candidate, requireSettled)
	if safe == nil {
		return AgentGroupProjection{}, fmt.Errorf("expert progress projection is invalid")
	}
	nodes, ok := workNodesFromExpertProgressForParent(safe, group.ID)
	if !ok || len(nodes) != safe.Total {
		return AgentGroupProjection{}, fmt.Errorf("expert progress could not be projected")
	}

	group.Strategy = safe.Strategy
	group.Total = safe.Total
	group.ProgressAvailable = true
	group.Nodes = make([]WorkNode, len(nodes))
	seenNodeIDs := make(map[string]struct{}, len(nodes))
	for index, source := range nodes {
		node := source
		node.ID = agentNodeID(group.ID, node.Index)
		if !node.valid(group.Total) || node.ParentID != group.ID ||
			node.Unread != 0 || node.ReportRef != nil || node.Elapsed != 0 {
			return AgentGroupProjection{}, fmt.Errorf("projected work node %d is invalid", index)
		}
		if _, duplicate := seenNodeIDs[node.ID]; duplicate {
			return AgentGroupProjection{}, fmt.Errorf("projected work node %d repeats an identity", index)
		}
		seenNodeIDs[node.ID] = struct{}{}
		group.Nodes[index] = node
		switch node.Status {
		case WorkNodeQueued:
			group.Queued++
		case WorkNodeRunning:
			group.Running++
		case WorkNodeWaiting:
			group.Waiting++
		case WorkNodeCompleted:
			group.Completed++
		case WorkNodeAttention:
			group.Attention++
		case WorkNodeFailed:
			group.Failed++
		case WorkNodeCancelled:
			group.Cancelled++
		}
	}
	if group.Running+group.Waiting != safe.Running ||
		group.Queued != safe.Queued ||
		group.Completed != safe.Completed ||
		group.Attention+group.Failed+group.Cancelled != safe.Failed {
		return AgentGroupProjection{}, fmt.Errorf("projected work-node counts do not match expert progress")
	}
	if !group.valid() {
		return AgentGroupProjection{}, fmt.Errorf("agent group projection is inconsistent")
	}
	return group, nil
}

func projectedAgentGroupElapsed(tool ToolEntry, now time.Time) time.Duration {
	if tool.Duration < 0 || tool.Duration > maxToolViewDuration {
		return -1
	}
	elapsed := tool.Duration
	if tool.Status == ToolStatusRunning && !tool.StartTime.IsZero() && !now.IsZero() {
		elapsed = now.Sub(tool.StartTime)
	}
	if elapsed < 0 {
		return -1
	}
	return min(elapsed, maxToolViewDuration)
}

func projectedAgentGroupLifecycle(entry ChatEntry, tool ToolEntry) (BlockLifecycle, bool, error) {
	switch tool.Status {
	case ToolStatusRunning:
		if entry.Lifecycle != BlockLive {
			return BlockPending, false, fmt.Errorf("running consultation is not a live transcript block")
		}
		return BlockLive, false, nil
	case ToolStatusDone:
		if entry.Lifecycle != BlockSettled {
			return BlockPending, false, fmt.Errorf("settled consultation is not a settled transcript block")
		}
		return BlockSettled, false, nil
	case ToolStatusError:
		if restoredInterruptedAgentGroup(entry, tool) {
			return BlockFailed, true, nil
		}
		if entry.Lifecycle == BlockFailed {
			return BlockFailed, false, nil
		}
		return BlockPending, false, fmt.Errorf("failed consultation is not a failed transcript block")
	case ToolStatusCancelled:
		if entry.Lifecycle != BlockCancelled {
			return BlockPending, false, fmt.Errorf("cancelled consultation is not a cancelled transcript block")
		}
		return BlockCancelled, false, nil
	default:
		return BlockPending, false, fmt.Errorf("consultation has invalid tool status %d", tool.Status)
	}
}

// restoredInterruptedAgentGroup recognizes only the exact safe shape produced
// by restoreToolEntries after a formerly live tool is settled. Transport
// failure remains distinct from domain success and from verified evidence.
func restoredInterruptedAgentGroup(entry ChatEntry, tool ToolEntry) bool {
	if (entry.Lifecycle != BlockLive && entry.Lifecycle != BlockFailed) ||
		tool.Status != ToolStatusError || tool.ExpertProgress != nil {
		return false
	}
	projection := tool.Projection.Normalize()
	return projection.Transport == ecosystem.TransportFailed &&
		projection.Domain == ecosystem.DomainUnknown &&
		projection.Evidence == ecosystem.EvidenceNone
}

func agentNodeID(groupID BlockID, index int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf(
		"sonar.work-node.v1\x00%s\x00%d",
		groupID,
		index,
	)))
	return "node_" + hex.EncodeToString(digest[:agentNodeIDEntropy])
}

func (surface AgentSurfaceProjection) valid() bool {
	if surface.OmittedGroups < 0 || len(surface.Groups) > maxAgentSurfaceGroups {
		return false
	}
	seen := make(map[BlockID]struct{}, len(surface.Groups))
	seenNodeIDs := make(map[string]struct{})
	for _, group := range surface.Groups {
		if !group.valid() {
			return false
		}
		if _, duplicate := seen[group.ID]; duplicate {
			return false
		}
		seen[group.ID] = struct{}{}
		for _, node := range group.Nodes {
			if _, duplicate := seenNodeIDs[node.ID]; duplicate {
				return false
			}
			seenNodeIDs[node.ID] = struct{}{}
		}
	}
	return true
}

func (group AgentGroupProjection) valid() bool {
	if !group.ID.Valid() || !group.TurnID.Valid() || group.Revision == 0 ||
		group.ToolIndex < 0 || group.Elapsed < 0 || group.Elapsed > maxToolViewDuration {
		return false
	}
	switch group.Lifecycle {
	case BlockLive, BlockSettled, BlockFailed, BlockCancelled:
	default:
		return false
	}
	if group.Interrupted && (group.Lifecycle != BlockFailed || group.ProgressAvailable) {
		return false
	}
	if !group.ProgressAvailable {
		return group.Strategy == "" && group.Total == 0 && group.Queued == 0 &&
			group.Running == 0 && group.Waiting == 0 && group.Completed == 0 &&
			group.Attention == 0 && group.Failed == 0 && group.Cancelled == 0 &&
			len(group.Nodes) == 0
	}
	if group.Total < 1 || group.Total > maxExpertProgressItems || len(group.Nodes) != group.Total ||
		(group.Strategy != expertselector.StrategyTeam &&
			group.Strategy != expertselector.StrategySwarm &&
			group.Strategy != expertselector.StrategyMoE) ||
		group.Queued < 0 || group.Running < 0 || group.Waiting < 0 ||
		group.Completed < 0 || group.Attention < 0 || group.Failed < 0 || group.Cancelled < 0 ||
		group.Queued+group.Running+group.Waiting+group.Completed+
			group.Attention+group.Failed+group.Cancelled != group.Total {
		return false
	}
	if group.Lifecycle != BlockLive && (group.Queued != 0 || group.Running != 0 || group.Waiting != 0) {
		return false
	}
	seenIDs := make(map[string]struct{}, len(group.Nodes))
	seenIndexes := make(map[int]struct{}, len(group.Nodes))
	for index, node := range group.Nodes {
		if !node.valid(group.Total) || node.Index != index ||
			node.ID != agentNodeID(group.ID, node.Index) ||
			node.ParentID != group.ID {
			return false
		}
		if _, duplicate := seenIDs[node.ID]; duplicate {
			return false
		}
		if _, duplicate := seenIndexes[node.Index]; duplicate {
			return false
		}
		seenIDs[node.ID] = struct{}{}
		seenIndexes[node.Index] = struct{}{}
	}
	return true
}
