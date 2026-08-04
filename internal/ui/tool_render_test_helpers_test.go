package ui

import (
	"fmt"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/ecosystem"
)

// ToolCardManager is retained only as a component-test fixture for the old
// correlation behavior. Production Model state and rendering never use it.
type ToolCardManager struct {
	Cards  []ToolCard
	IsDark bool
}

func NewToolCardManager(isDark bool) ToolCardManager {
	return ToolCardManager{Cards: []ToolCard{}, IsDark: isDark}
}

func (manager *ToolCardManager) AddCardWithID(
	id, name string,
	kind ToolCardKind,
	startTime time.Time,
) {
	card := NewToolCard(name, kind, manager.IsDark, defaultThemeID)
	card.ID = id
	card.StartTime = startTime
	manager.Cards = append(manager.Cards, card)
}

func (manager *ToolCardManager) UpdateCardWithID(
	id, name string,
	state ToolCardState,
	result string,
	duration time.Duration,
) {
	manager.UpdateCardSemanticWithID(id, name, state, result, "", duration, ecosystem.ToolProjection{})
}

func (manager *ToolCardManager) UpdateCardSemanticWithID(
	id, name string,
	state ToolCardState,
	result, resultDisplay string,
	duration time.Duration,
	projection ecosystem.ToolProjection,
) {
	for index := len(manager.Cards) - 1; index >= 0; index-- {
		card := &manager.Cards[index]
		if toolCallMatches(id, name, card.ID, card.Name) && card.State == ToolCardRunning {
			card.State = state
			card.Result = result
			card.ResultDisplay = resultDisplay
			card.Duration = duration
			card.Projection = projection.Normalize()
			break
		}
	}
}

func (manager *ToolCardManager) SetDark(isDark bool, themeID string) {
	manager.IsDark = isDark
	for index := range manager.Cards {
		manager.Cards[index].SetDark(isDark, themeID)
	}
}

func testToolChatEntry(index int) ChatEntry {
	return ChatEntry{
		BlockID:   BlockID(fmt.Sprintf("block-tool-test-%d", index)),
		TurnID:    TurnID("turn-tool-test"),
		Revision:  1,
		Lifecycle: BlockSettled,
		Kind:      "tool_group",
		ToolIndex: index,
	}
}

func testProjectedToolCard(t testing.TB, model *Model, index int) ToolCard {
	t.Helper()
	if model == nil || index < 0 || index >= len(model.toolEntries) {
		t.Fatalf("tool index %d is out of range", index)
	}
	chat := testToolChatEntry(index)
	for _, entry := range model.entries {
		if entry.Kind == "tool_group" && entry.ToolIndex == index &&
			entry.BlockID.Valid() && entry.Revision > 0 {
			chat = entry
			break
		}
	}
	renderModel, err := ToolRenderModelFromEntry(chat, model.toolEntries[index])
	if err != nil {
		t.Fatalf("project tool %d: %v", index, err)
	}
	card, err := ToolCardFromRenderModel(renderModel, model.isDark, defaultThemeID, model.glyphProfile)
	if err != nil {
		t.Fatalf("construct tool card %d: %v", index, err)
	}
	return card
}
