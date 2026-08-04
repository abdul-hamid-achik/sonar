package ui

// CellRect describes a half-open rectangle of terminal cells:
// [MinX, MaxX) x [MinY, MaxY). The zero value is an empty rectangle.
type CellRect struct {
	MinX int
	MinY int
	MaxX int
	MaxY int
}

// NewCellRect creates a canonical half-open rectangle from its bounds.
// Inverted axes collapse at their minimum coordinate instead of producing a
// negative extent.
func NewCellRect(minX, minY, maxX, maxY int) CellRect {
	return CellRect{
		MinX: minX,
		MinY: minY,
		MaxX: max(maxX, minX),
		MaxY: max(maxY, minY),
	}
}

// Width returns the rectangle's non-negative horizontal extent.
func (r CellRect) Width() int {
	return max(0, r.MaxX-r.MinX)
}

// Height returns the rectangle's non-negative vertical extent.
func (r CellRect) Height() int {
	return max(0, r.MaxY-r.MinY)
}

// Empty reports whether the rectangle contains no terminal cells.
func (r CellRect) Empty() bool {
	return r.Width() == 0 || r.Height() == 0
}

// Contains reports whether the terminal cell at (x, y) is inside the
// rectangle. Cells on MaxX or MaxY are outside by the half-open contract.
func (r CellRect) Contains(x, y int) bool {
	return x >= r.MinX && x < r.MaxX && y >= r.MinY && y < r.MaxY
}

func (r CellRect) canonical() CellRect {
	return NewCellRect(r.MinX, r.MinY, r.MaxX, r.MaxY)
}

// Insets reserves terminal cells along the four edges of a CellRect. Negative
// values are treated as zero.
type Insets struct {
	Top    int
	Right  int
	Bottom int
	Left   int
}

// Inset applies insets without allowing either axis to cross. If opposing
// insets exceed the available extent, the top or left inset is satisfied
// first and the result collapses to an empty axis.
func Inset(rect CellRect, insets Insets) CellRect {
	rect = rect.canonical()

	left := clampExtent(insets.Left, rect.Width())
	right := clampExtent(insets.Right, rect.Width()-left)
	top := clampExtent(insets.Top, rect.Height())
	bottom := clampExtent(insets.Bottom, rect.Height()-top)

	return NewCellRect(
		rect.MinX+left,
		rect.MinY+top,
		rect.MaxX-right,
		rect.MaxY-bottom,
	)
}

// TakeTop removes up to n rows from the top of rect. It returns the removed
// rectangle first and the remaining rectangle second.
func TakeTop(rect CellRect, n int) (taken, remain CellRect) {
	rect = rect.canonical()
	n = clampExtent(n, rect.Height())
	cut := rect.MinY + n
	return NewCellRect(rect.MinX, rect.MinY, rect.MaxX, cut),
		NewCellRect(rect.MinX, cut, rect.MaxX, rect.MaxY)
}

// TakeBottom removes up to n rows from the bottom of rect. It returns the
// removed rectangle first and the remaining rectangle second.
func TakeBottom(rect CellRect, n int) (taken, remain CellRect) {
	rect = rect.canonical()
	n = clampExtent(n, rect.Height())
	cut := rect.MaxY - n
	return NewCellRect(rect.MinX, cut, rect.MaxX, rect.MaxY),
		NewCellRect(rect.MinX, rect.MinY, rect.MaxX, cut)
}

// TakeLeft removes up to n columns from the left of rect. It returns the
// removed rectangle first and the remaining rectangle second.
func TakeLeft(rect CellRect, n int) (taken, remain CellRect) {
	rect = rect.canonical()
	n = clampExtent(n, rect.Width())
	cut := rect.MinX + n
	return NewCellRect(rect.MinX, rect.MinY, cut, rect.MaxY),
		NewCellRect(cut, rect.MinY, rect.MaxX, rect.MaxY)
}

// TakeRight removes up to n columns from the right of rect. It returns the
// removed rectangle first and the remaining rectangle second.
func TakeRight(rect CellRect, n int) (taken, remain CellRect) {
	rect = rect.canonical()
	n = clampExtent(n, rect.Width())
	cut := rect.MaxX - n
	return NewCellRect(cut, rect.MinY, rect.MaxX, rect.MaxY),
		NewCellRect(rect.MinX, rect.MinY, cut, rect.MaxY)
}

func clampExtent(value, extent int) int {
	if value <= 0 || extent <= 0 {
		return 0
	}
	return min(value, extent)
}

// WidthClass is the horizontal density tier for a terminal frame. Recovery is
// the zero value so an unmeasured terminal fails safe.
type WidthClass uint8

const (
	WidthRecovery WidthClass = iota
	WidthCompact
	WidthNarrow
	WidthRegular
	WidthWide
)

// ClassifyWidth maps a terminal width to its horizontal density tier.
func ClassifyWidth(width int) WidthClass {
	switch {
	case width < minTerminalWidth:
		return WidthRecovery
	case width < 40:
		return WidthCompact
	case width < 72:
		return WidthNarrow
	case width < 112:
		return WidthRegular
	default:
		return WidthWide
	}
}

// HeightClass is the vertical density tier for a terminal frame. Recovery is
// the zero value so an unmeasured terminal fails safe.
type HeightClass uint8

const (
	HeightRecovery HeightClass = iota
	HeightCompact
	HeightShort
	HeightRegular
	HeightTall
)

// ClassifyHeight maps a terminal height to its vertical density tier.
func ClassifyHeight(height int) HeightClass {
	switch {
	case height < minTerminalHeight:
		return HeightRecovery
	case height < 16:
		return HeightCompact
	case height < 24:
		return HeightShort
	case height < 40:
		return HeightRegular
	default:
		return HeightTall
	}
}
