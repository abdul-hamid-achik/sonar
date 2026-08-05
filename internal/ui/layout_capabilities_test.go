package ui

import "testing"

func TestLayoutCapabilityBoundariesUseResidualWidth(t *testing.T) {
	const workHeight = 24
	tests := []struct {
		name       string
		below      int
		at         int
		capability func(LayoutCapabilities) bool
	}{
		{
			name:       "diff gutter",
			below:      layoutDiffGutterColumns + layoutMinUnifiedCodeColumns - 1,
			at:         layoutDiffGutterColumns + layoutMinUnifiedCodeColumns,
			capability: func(c LayoutCapabilities) bool { return c.CanShowDiffGutters },
		},
		{
			name:       "dual diff gutter",
			below:      layoutDualGutterColumns + layoutMinUnifiedCodeColumns - 1,
			at:         layoutDualGutterColumns + layoutMinUnifiedCodeColumns,
			capability: func(c LayoutCapabilities) bool { return c.CanShowDualGutters },
		},
		{
			name: "split diff",
			below: 2*layoutDiffGutterColumns + layoutSplitGapColumns +
				2*layoutMinSplitCodeColumns - 1,
			at: 2*layoutDiffGutterColumns + layoutSplitGapColumns +
				2*layoutMinSplitCodeColumns,
			capability: func(c LayoutCapabilities) bool { return c.CanUseSplitDiff },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			below := DeriveLayoutCapabilities(
				NewCellRect(0, 0, test.below, workHeight),
				LayoutCapabilityOptions{},
			)
			if test.capability(below) {
				t.Fatalf("capability enabled before residual-width boundary at %d columns", test.below)
			}

			at := DeriveLayoutCapabilities(
				NewCellRect(0, 0, test.at, workHeight),
				LayoutCapabilityOptions{},
			)
			if !test.capability(at) {
				t.Fatalf("capability disabled at residual-width boundary of %d columns", test.at)
			}
		})
	}
}

func TestLayoutCapabilitiesProseAndWorkWidthRemainSeparate(t *testing.T) {
	tests := []struct {
		workWidth int
		wantProse int
	}{
		{workWidth: 0, wantProse: 0},
		{workWidth: 1, wantProse: 1},
		{workWidth: ProseTargetCandidate - 1, wantProse: ProseTargetCandidate - 1},
		{workWidth: ProseTargetCandidate, wantProse: ProseTargetCandidate},
		// Wide work widths grow prose toward WorkWidth (soft-capped).
		{workWidth: ProseTargetCandidate + 1, wantProse: proseWidthForWork(ProseTargetCandidate + 1)},
		{workWidth: 193, wantProse: proseWidthForWork(193)},
	}

	for _, test := range tests {
		capabilities := DeriveLayoutCapabilities(
			NewCellRect(13, 17, 13+test.workWidth, 47),
			LayoutCapabilityOptions{},
		)
		if capabilities.WorkWidth != test.workWidth {
			t.Fatalf("width %d: WorkWidth = %d", test.workWidth, capabilities.WorkWidth)
		}
		if capabilities.ProseWidth != test.wantProse {
			t.Fatalf("width %d: ProseWidth = %d, want %d", test.workWidth, capabilities.ProseWidth, test.wantProse)
		}
		if capabilities.ProseWidth > capabilities.WorkWidth {
			t.Fatalf("width %d: ProseWidth %d exceeds WorkWidth %d", test.workWidth, capabilities.ProseWidth, capabilities.WorkWidth)
		}
	}
}

func TestLayoutCapabilityTwoAxisBoundaries(t *testing.T) {
	t.Run("dock context", func(t *testing.T) {
		width := layoutContextChatColumns + layoutContextGapColumns + layoutContextPanelColumns
		assertCapabilityBoundary(
			t,
			func(c LayoutCapabilities) bool { return c.CanDockContext },
			NewCellRect(0, 0, width-1, layoutContextRows),
			NewCellRect(0, 0, width, layoutContextRows-1),
			NewCellRect(0, 0, width, layoutContextRows),
		)
	})

	t.Run("rich header", func(t *testing.T) {
		assertCapabilityBoundary(
			t,
			func(c LayoutCapabilities) bool { return c.CanShowRichHeader },
			NewCellRect(0, 0, layoutRichHeaderColumns-1, layoutRichHeaderRows),
			NewCellRect(0, 0, layoutRichHeaderColumns, layoutRichHeaderRows-1),
			NewCellRect(0, 0, layoutRichHeaderColumns, layoutRichHeaderRows),
		)
	})

	t.Run("stack auxiliary", func(t *testing.T) {
		height := minTranscriptRows + layoutAuxiliaryGapRows + layoutAuxiliaryRows
		assertCapabilityBoundary(
			t,
			func(c LayoutCapabilities) bool { return c.CanStackAuxiliary },
			NewCellRect(0, 0, 100, height-1),
			NewCellRect(0, 0, 0, height-1),
			NewCellRect(0, 0, 100, height),
		)
	})

	t.Run("agent preview", func(t *testing.T) {
		width := layoutAgentRailColumns + layoutAgentPreviewColumns
		assertCapabilityBoundary(
			t,
			func(c LayoutCapabilities) bool { return c.CanShowAgentPreview },
			NewCellRect(0, 0, width-1, layoutAgentPreviewRows),
			NewCellRect(0, 0, width, layoutAgentPreviewRows-1),
			NewCellRect(0, 0, width, layoutAgentPreviewRows),
		)
	})
}

func TestLayoutCapabilitiesAreMonotonicAndBoundedExhaustive(t *testing.T) {
	for height := 0; height <= 400; height++ {
		var previous LayoutCapabilities
		for width := 0; width <= 400; width++ {
			capabilities := DeriveLayoutCapabilities(
				NewCellRect(7, 11, 7+width, 11+height),
				LayoutCapabilityOptions{},
			)
			if capabilities.WorkRect != NewCellRect(7, 11, 7+width, 11+height) {
				t.Fatalf("%dx%d work rect changed: %+v", width, height, capabilities.WorkRect)
			}
			if capabilities.WorkWidth != width || capabilities.WorkHeight != height {
				t.Fatalf("%dx%d work size = %dx%d", width, height, capabilities.WorkWidth, capabilities.WorkHeight)
			}
			// The invariant is that prose never exceeds its pane. It used to
			// also assert prose <= ProseTargetWide, which encoded a default
			// (a fixed 140-column measure) as a structural property; prose
			// now tracks the pane unless ui.prose_width says otherwise.
			if capabilities.ProseWidth < 0 || capabilities.ProseWidth > capabilities.WorkWidth {
				t.Fatalf("%dx%d invalid prose/work width: %+v", width, height, capabilities)
			}
			if width > 0 {
				assertCapabilitiesDoNotRegress(t, width, height, previous, capabilities)
			}
			previous = capabilities
		}
	}

	for width := 0; width <= 400; width++ {
		var previous LayoutCapabilities
		for height := 0; height <= 400; height++ {
			capabilities := DeriveLayoutCapabilities(
				NewCellRect(0, 0, width, height),
				LayoutCapabilityOptions{},
			)
			if height > 0 {
				assertVerticalCapabilitiesDoNotRegress(t, width, height, previous, capabilities)
			}
			previous = capabilities
		}
	}
}

func assertCapabilityBoundary(
	t *testing.T,
	capability func(LayoutCapabilities) bool,
	belowWidth CellRect,
	belowHeight CellRect,
	atBoundary CellRect,
) {
	t.Helper()
	for name, rect := range map[string]CellRect{
		"below width":  belowWidth,
		"below height": belowHeight,
	} {
		if capability(DeriveLayoutCapabilities(rect, LayoutCapabilityOptions{})) {
			t.Fatalf("%s unexpectedly enabled capability at %+v", name, rect)
		}
	}
	if !capability(DeriveLayoutCapabilities(atBoundary, LayoutCapabilityOptions{})) {
		t.Fatalf("capability disabled at boundary %+v", atBoundary)
	}
}

func TestForceCompactChangesDensityNotMeasuredCapacity(t *testing.T) {
	rect := NewCellRect(3, 5, 203, 85)
	regular := DeriveLayoutCapabilities(rect, LayoutCapabilityOptions{})
	compact := DeriveLayoutCapabilities(rect, LayoutCapabilityOptions{ForceCompact: true})

	if regular.Density != LayoutDensitySpacious || compact.Density != LayoutDensityCompact {
		t.Fatalf("density = regular %d/forced %d", regular.Density, compact.Density)
	}
	regular.Density = LayoutDensityCompact
	if compact != regular {
		t.Fatalf("force compact changed physical capabilities:\nregular=%+v\n compact=%+v", regular, compact)
	}
}

func assertCapabilitiesDoNotRegress(
	t *testing.T,
	width int,
	height int,
	previous LayoutCapabilities,
	current LayoutCapabilities,
) {
	t.Helper()
	if current.WorkWidth < previous.WorkWidth || current.ProseWidth < previous.ProseWidth ||
		current.WidthClass < previous.WidthClass {
		t.Fatalf("%dx%d horizontal measure regressed: previous=%+v current=%+v", width, height, previous, current)
	}
	for name, values := range map[string][2]bool{
		"dock context":  {previous.CanDockContext, current.CanDockContext},
		"rich header":   {previous.CanShowRichHeader, current.CanShowRichHeader},
		"diff gutters":  {previous.CanShowDiffGutters, current.CanShowDiffGutters},
		"dual gutters":  {previous.CanShowDualGutters, current.CanShowDualGutters},
		"split diff":    {previous.CanUseSplitDiff, current.CanUseSplitDiff},
		"stack aux":     {previous.CanStackAuxiliary, current.CanStackAuxiliary},
		"agent preview": {previous.CanShowAgentPreview, current.CanShowAgentPreview},
	} {
		if values[0] && !values[1] {
			t.Fatalf("%dx%d %s regressed with added width", width, height, name)
		}
	}
}

func assertVerticalCapabilitiesDoNotRegress(
	t *testing.T,
	width int,
	height int,
	previous LayoutCapabilities,
	current LayoutCapabilities,
) {
	t.Helper()
	if current.WorkHeight < previous.WorkHeight || current.HeightClass < previous.HeightClass {
		t.Fatalf("%dx%d vertical measure regressed: previous=%+v current=%+v", width, height, previous, current)
	}
	for name, values := range map[string][2]bool{
		"dock context":  {previous.CanDockContext, current.CanDockContext},
		"rich header":   {previous.CanShowRichHeader, current.CanShowRichHeader},
		"diff gutters":  {previous.CanShowDiffGutters, current.CanShowDiffGutters},
		"dual gutters":  {previous.CanShowDualGutters, current.CanShowDualGutters},
		"split diff":    {previous.CanUseSplitDiff, current.CanUseSplitDiff},
		"stack aux":     {previous.CanStackAuxiliary, current.CanStackAuxiliary},
		"agent preview": {previous.CanShowAgentPreview, current.CanShowAgentPreview},
	} {
		if values[0] && !values[1] {
			t.Fatalf("%dx%d %s regressed with added height", width, height, name)
		}
	}
}
