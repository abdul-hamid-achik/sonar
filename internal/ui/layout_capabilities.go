package ui

const (
	// ProseTargetCandidate is the minimum comfortable measure for chat prose.
	// On wide terminals chatProseWidth grows toward WorkWidth so the column
	// uses available space (Grok-style) instead of leaving a dead right margin.
	// Code fences, tables, diffs, and inspectors still use full WorkWidth.
	ProseTargetCandidate = 96
	// ProseTargetWide is the soft cap for conversational prose on very wide
	// terminals. Beyond this, long lines become harder to scan.
	ProseTargetWide = 140

	// transcriptContentChromeColumns is the full horizontal chrome reserved by
	// the content grid: left accent+pad plus right slack. Prefer the split
	// tokens (contentLeftColumns / contentRightChromeColumns) when applying
	// insets; keep this sum for WorkWidth = pane − chrome contracts.
	transcriptContentChromeColumns = contentLeftColumns + contentRightChromeColumns
	transcriptMinimumWorkColumns   = 14
	layoutMinUnifiedCodeColumns    = 40
	layoutMinSplitCodeColumns      = 52
	layoutDiffGutterColumns        = 6
	layoutDualGutterColumns        = 12
	layoutSplitGapColumns          = 1
	layoutContextChatColumns       = 72
	layoutContextPanelColumns      = 30
	layoutContextGapColumns        = 1
	layoutContextRows              = 12
	layoutRichHeaderColumns        = 48
	layoutRichHeaderRows           = 6
	layoutAuxiliaryRows            = 4
	layoutAuxiliaryGapRows         = 1
	layoutAgentPreviewColumns      = 48
	layoutAgentPreviewRows         = 8
	layoutAgentRailColumns         = 2
)

// LayoutDensity is a presentation choice made after measuring a component.
// It must not be used as a substitute for a physical capability check.
type LayoutDensity uint8

const (
	LayoutDensityCompact LayoutDensity = iota
	LayoutDensityRegular
	LayoutDensitySpacious
)

// LayoutCapabilityOptions contains explicit user presentation preferences.
// ForceCompact changes density, but not the component's measured capacity.
type LayoutCapabilityOptions struct {
	ForceCompact bool
}

// LayoutCapabilities is the immutable geometry contract for one component.
// WorkRect must be the component's final allocated rectangle, after parent
// splits and insets. Every width capability is calculated from residual work
// cells rather than from the outer terminal width.
type LayoutCapabilities struct {
	WorkRect   CellRect
	WorkWidth  int
	WorkHeight int
	ProseWidth int

	WidthClass  WidthClass
	HeightClass HeightClass
	Density     LayoutDensity

	CanDockContext      bool
	CanShowRichHeader   bool
	CanShowDiffGutters  bool
	CanShowDualGutters  bool
	CanUseSplitDiff     bool
	CanStackAuxiliary   bool
	CanShowAgentPreview bool
}

// proseWidthForWork grows conversational measure with the work rectangle so
// wide terminals do not leave a large dead margin (Grok-style density).
// Small panes stay at the classic 96-col comfort cap.
func proseWidthForWork(workWidth int) int {
	if workWidth <= 0 {
		return 0
	}
	if workWidth <= ProseTargetCandidate {
		return workWidth
	}
	// Use most of the work width on large panes, soft-capped for readability.
	wide := min(ProseTargetWide, workWidth)
	// Prefer ~90% of work width between 96 and 140.
	grown := max(ProseTargetCandidate, (workWidth*9)/10)
	return min(wide, grown)
}

// DeriveLayoutCapabilities measures the final work rectangle for a component.
// The fixed numbers below are named design tokens. In particular, diff and
// split decisions require the minimum readable code width to remain after
// gutters and gaps have been subtracted.
func DeriveLayoutCapabilities(workRect CellRect, options LayoutCapabilityOptions) LayoutCapabilities {
	workRect = workRect.canonical()
	workWidth := workRect.Width()
	workHeight := workRect.Height()
	widthClass := ClassifyWidth(workWidth)
	heightClass := ClassifyHeight(workHeight)

	density := LayoutDensityRegular
	switch {
	case options.ForceCompact || widthClass <= WidthNarrow || heightClass <= HeightShort:
		density = LayoutDensityCompact
	case widthClass == WidthWide && heightClass >= HeightRegular:
		density = LayoutDensitySpacious
	}

	diffBodyWidth := residualColumns(workWidth, layoutDiffGutterColumns)
	dualGutterBodyWidth := residualColumns(workWidth, layoutDualGutterColumns)
	splitBodyWidth := residualColumns(
		workWidth,
		2*layoutDiffGutterColumns+layoutSplitGapColumns,
	)

	return LayoutCapabilities{
		WorkRect:    workRect,
		WorkWidth:   workWidth,
		WorkHeight:  workHeight,
		ProseWidth:  proseWidthForWork(workWidth),
		WidthClass:  widthClass,
		HeightClass: heightClass,
		Density:     density,

		CanDockContext: workWidth >= layoutContextChatColumns+
			layoutContextGapColumns+layoutContextPanelColumns &&
			workHeight >= layoutContextRows,
		CanShowRichHeader:  workWidth >= layoutRichHeaderColumns && workHeight >= layoutRichHeaderRows,
		CanShowDiffGutters: diffBodyWidth >= layoutMinUnifiedCodeColumns,
		CanShowDualGutters: dualGutterBodyWidth >= layoutMinUnifiedCodeColumns,
		CanUseSplitDiff:    splitBodyWidth >= 2*layoutMinSplitCodeColumns,
		CanStackAuxiliary: workHeight >= minTranscriptRows+
			layoutAuxiliaryGapRows+layoutAuxiliaryRows,
		CanShowAgentPreview: residualColumns(workWidth, layoutAgentRailColumns) >=
			layoutAgentPreviewColumns && workHeight >= layoutAgentPreviewRows,
	}
}

func residualColumns(width int, reserved ...int) int {
	for _, cells := range reserved {
		width -= max(0, cells)
	}
	return max(0, width)
}
