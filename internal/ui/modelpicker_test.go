package ui

import (
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

func TestModelPicker_OpenClose(t *testing.T) {
	t.Run("open_without_router_noop", func(t *testing.T) {
		m := newTestModel(t)
		m.openModelPicker()
		if m.overlay == OverlayModelPicker {
			t.Error("should not open picker without router")
		}
	})

	t.Run("open_with_router", func(t *testing.T) {
		m := newTestModel(t)
		modelCfg := config.DefaultModelConfig()
		m.router = config.NewRouter(&modelCfg)
		m.model = "qwen3.5:0.8b"

		m.openModelPicker()

		if m.overlay != OverlayModelPicker {
			t.Errorf("expected OverlayModelPicker, got %d", m.overlay)
		}
		if m.modelPickerState == nil {
			t.Fatal("modelPickerState should not be nil")
		}
		if len(m.modelPickerState.Models) == 0 {
			t.Error("should have models in picker")
		}
		if m.modelPickerState.CurrentModel != "qwen3.5:0.8b" {
			t.Errorf("expected current model 'qwen3.5:0.8b', got %q", m.modelPickerState.CurrentModel)
		}
	})

	t.Run("close_resets_state", func(t *testing.T) {
		m := newTestModel(t)
		modelCfg := config.DefaultModelConfig()
		m.router = config.NewRouter(&modelCfg)
		m.model = "qwen3.5:0.8b"
		m.openModelPicker()

		m.closeModelPicker()

		if m.modelPickerState != nil {
			t.Error("modelPickerState should be nil after close")
		}
		if m.overlay != OverlayNone {
			t.Errorf("overlay should be OverlayNone, got %d", m.overlay)
		}
	})
}

func TestModelPicker_Navigation(t *testing.T) {
	setup := func(t *testing.T) *Model {
		t.Helper()
		m := newTestModel(t)
		modelCfg := config.DefaultModelConfig()
		m.router = config.NewRouter(&modelCfg)
		m.model = config.DefaultModels()[0].Name
		m.openModelPicker()
		return m
	}

	t.Run("down_moves_index", func(t *testing.T) {
		m := setup(t)

		updated, _ := m.Update(downKey())
		m = updated.(*Model)

		if m.modelPickerState.List.Index() != 1 {
			t.Errorf("expected index 1, got %d", m.modelPickerState.List.Index())
		}
	})

	t.Run("up_at_zero_stays", func(t *testing.T) {
		m := setup(t)

		updated, _ := m.Update(upKey())
		m = updated.(*Model)

		if m.modelPickerState.List.Index() != 0 {
			t.Errorf("expected index 0, got %d", m.modelPickerState.List.Index())
		}
	})

	t.Run("down_clamped_at_end", func(t *testing.T) {
		m := setup(t)
		lastIdx := len(m.modelPickerState.Models) - 1
		m.modelPickerState.List.Select(lastIdx)

		updated, _ := m.Update(downKey())
		m = updated.(*Model)

		if m.modelPickerState.List.Index() != lastIdx {
			t.Errorf("expected index to stay at end, got %d", m.modelPickerState.List.Index())
		}
	})

	t.Run("esc_closes", func(t *testing.T) {
		m := setup(t)

		updated, _ := m.Update(escKey())
		m = updated.(*Model)

		if m.modelPickerState != nil {
			t.Error("ESC should close picker")
		}
		if m.overlay != OverlayNone {
			t.Errorf("overlay should be OverlayNone, got %d", m.overlay)
		}
	})
}

func TestModelPicker_CtrlO(t *testing.T) {
	t.Run("opens_with_ctrl_o", func(t *testing.T) {
		m := newTestModel(t)
		modelCfg := config.DefaultModelConfig()
		m.router = config.NewRouter(&modelCfg)
		m.model = "qwen3.5:0.8b"
		m.state = StateIdle

		updated, _ := m.Update(ctrlKey('o'))
		m = updated.(*Model)

		if m.overlay != OverlayModelPicker {
			t.Errorf("ctrl+o should open model picker, got overlay %d", m.overlay)
		}
	})

	t.Run("no_open_when_streaming", func(t *testing.T) {
		m := newTestModel(t)
		modelCfg := config.DefaultModelConfig()
		m.router = config.NewRouter(&modelCfg)
		m.state = StateStreaming

		updated, _ := m.Update(ctrlKey('o'))
		m = updated.(*Model)

		if m.overlay == OverlayModelPicker {
			t.Error("should not open picker when streaming")
		}
	})
}
