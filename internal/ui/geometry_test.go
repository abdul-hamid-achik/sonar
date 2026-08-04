package ui

import "testing"

func TestCellRectHalfOpenContractExhaustive(t *testing.T) {
	const originX, originY = 7, 11

	for width := 0; width <= 400; width++ {
		for height := 0; height <= 400; height++ {
			rect := NewCellRect(originX, originY, originX+width, originY+height)
			if got := rect.Width(); got != width {
				t.Fatalf("%dx%d width = %d", width, height, got)
			}
			if got := rect.Height(); got != height {
				t.Fatalf("%dx%d height = %d", width, height, got)
			}
			if got, want := rect.Empty(), width == 0 || height == 0; got != want {
				t.Fatalf("%dx%d Empty() = %v, want %v", width, height, got, want)
			}
			if rect.Contains(rect.MaxX, rect.MinY) || rect.Contains(rect.MinX, rect.MaxY) {
				t.Fatalf("%dx%d includes an exclusive maximum: %#v", width, height, rect)
			}
			if rect.Contains(rect.MinX-1, rect.MinY) || rect.Contains(rect.MinX, rect.MinY-1) {
				t.Fatalf("%dx%d includes a cell before its minimum: %#v", width, height, rect)
			}
			if width > 0 && height > 0 {
				if !rect.Contains(rect.MinX, rect.MinY) {
					t.Fatalf("%dx%d excludes its minimum cell: %#v", width, height, rect)
				}
				if !rect.Contains(rect.MaxX-1, rect.MaxY-1) {
					t.Fatalf("%dx%d excludes its last cell: %#v", width, height, rect)
				}
			}
		}
	}
}

func TestCellRectCanonicalizesInvertedBounds(t *testing.T) {
	want := CellRect{MinX: 10, MinY: 20, MaxX: 10, MaxY: 20}
	if got := NewCellRect(10, 20, 5, 15); got != want {
		t.Fatalf("NewCellRect inverted bounds = %#v, want %#v", got, want)
	}

	malformed := CellRect{MinX: 10, MinY: 20, MaxX: 5, MaxY: 15}
	if malformed.Width() != 0 || malformed.Height() != 0 || !malformed.Empty() {
		t.Fatalf("malformed rectangle exposed a negative extent: %#v", malformed)
	}
	if malformed.Contains(7, 17) {
		t.Fatalf("malformed rectangle contains a cell: %#v", malformed)
	}
	taken, remain := TakeBottom(malformed, 1)
	if taken != want || remain != want {
		t.Fatalf("split did not canonicalize malformed parent: taken=%#v remain=%#v", taken, remain)
	}
}

func TestCellRectSplitsExhaustive(t *testing.T) {
	const originX, originY = 5, 9

	for width := 0; width <= 400; width++ {
		for height := 0; height <= 400; height++ {
			parent := NewCellRect(originX, originY, originX+width, originY+height)

			for _, n := range representativeExtents(height) {
				clamped := testClampExtent(n, height)

				taken, remain := TakeTop(parent, n)
				wantTaken := NewCellRect(parent.MinX, parent.MinY, parent.MaxX, parent.MinY+clamped)
				wantRemain := NewCellRect(parent.MinX, parent.MinY+clamped, parent.MaxX, parent.MaxY)
				assertSplit(t, "top", parent, taken, remain, wantTaken, wantRemain)

				taken, remain = TakeBottom(parent, n)
				wantTaken = NewCellRect(parent.MinX, parent.MaxY-clamped, parent.MaxX, parent.MaxY)
				wantRemain = NewCellRect(parent.MinX, parent.MinY, parent.MaxX, parent.MaxY-clamped)
				assertSplit(t, "bottom", parent, taken, remain, wantTaken, wantRemain)
			}

			for _, n := range representativeExtents(width) {
				clamped := testClampExtent(n, width)

				taken, remain := TakeLeft(parent, n)
				wantTaken := NewCellRect(parent.MinX, parent.MinY, parent.MinX+clamped, parent.MaxY)
				wantRemain := NewCellRect(parent.MinX+clamped, parent.MinY, parent.MaxX, parent.MaxY)
				assertSplit(t, "left", parent, taken, remain, wantTaken, wantRemain)

				taken, remain = TakeRight(parent, n)
				wantTaken = NewCellRect(parent.MaxX-clamped, parent.MinY, parent.MaxX, parent.MaxY)
				wantRemain = NewCellRect(parent.MinX, parent.MinY, parent.MaxX-clamped, parent.MaxY)
				assertSplit(t, "right", parent, taken, remain, wantTaken, wantRemain)
			}
		}
	}
}

func TestInsetExhaustive(t *testing.T) {
	const originX, originY = 3, 13

	for width := 0; width <= 400; width++ {
		for height := 0; height <= 400; height++ {
			parent := NewCellRect(originX, originY, originX+width, originY+height)
			cases := []Insets{
				{},
				{Top: 1, Right: 1, Bottom: 1, Left: 1},
				{Top: height / 2, Right: width / 2, Bottom: height / 2, Left: width / 2},
				{Top: height, Right: width, Bottom: height, Left: width},
				{Top: height + 1, Right: width + 1, Bottom: height + 1, Left: width + 1},
				{Top: -1, Right: -1, Bottom: -1, Left: -1},
				{Top: height / 3, Right: width, Bottom: height, Left: width / 3},
			}

			for _, insets := range cases {
				left := testClampExtent(insets.Left, width)
				right := testClampExtent(insets.Right, width-left)
				top := testClampExtent(insets.Top, height)
				bottom := testClampExtent(insets.Bottom, height-top)
				want := NewCellRect(
					parent.MinX+left,
					parent.MinY+top,
					parent.MaxX-right,
					parent.MaxY-bottom,
				)
				got := Inset(parent, insets)
				if got != want {
					t.Fatalf("%dx%d Inset(%+v) = %#v, want %#v", width, height, insets, got, want)
				}
				if !rectWithin(got, parent) {
					t.Fatalf("%dx%d Inset(%+v) escaped parent: got=%#v parent=%#v", width, height, insets, got, parent)
				}
			}
		}
	}
}

func TestWidthClassBoundaries(t *testing.T) {
	tests := []struct {
		width int
		want  WidthClass
	}{
		{width: -1, want: WidthRecovery},
		{width: 0, want: WidthRecovery},
		{width: 29, want: WidthRecovery},
		{width: 30, want: WidthCompact},
		{width: 39, want: WidthCompact},
		{width: 40, want: WidthNarrow},
		{width: 71, want: WidthNarrow},
		{width: 72, want: WidthRegular},
		{width: 111, want: WidthRegular},
		{width: 112, want: WidthWide},
		{width: 400, want: WidthWide},
	}
	for _, tt := range tests {
		if got := ClassifyWidth(tt.width); got != tt.want {
			t.Fatalf("ClassifyWidth(%d) = %d, want %d", tt.width, got, tt.want)
		}
	}

	previous := WidthRecovery
	for width := 0; width <= 400; width++ {
		got := ClassifyWidth(width)
		if got < previous {
			t.Fatalf("width class decreased at %d: %d -> %d", width, previous, got)
		}
		previous = got
	}
}

func TestHeightClassBoundaries(t *testing.T) {
	tests := []struct {
		height int
		want   HeightClass
	}{
		{height: -1, want: HeightRecovery},
		{height: 0, want: HeightRecovery},
		{height: 11, want: HeightRecovery},
		{height: 12, want: HeightCompact},
		{height: 15, want: HeightCompact},
		{height: 16, want: HeightShort},
		{height: 23, want: HeightShort},
		{height: 24, want: HeightRegular},
		{height: 39, want: HeightRegular},
		{height: 40, want: HeightTall},
		{height: 400, want: HeightTall},
	}
	for _, tt := range tests {
		if got := ClassifyHeight(tt.height); got != tt.want {
			t.Fatalf("ClassifyHeight(%d) = %d, want %d", tt.height, got, tt.want)
		}
	}

	previous := HeightRecovery
	for height := 0; height <= 400; height++ {
		got := ClassifyHeight(height)
		if got < previous {
			t.Fatalf("height class decreased at %d: %d -> %d", height, previous, got)
		}
		previous = got
	}
}

func representativeExtents(extent int) []int {
	return []int{-1, 0, 1, extent / 2, extent - 1, extent, extent + 1, extent + 17}
}

func testClampExtent(value, extent int) int {
	if value <= 0 || extent <= 0 {
		return 0
	}
	return min(value, extent)
}

func assertSplit(
	t *testing.T,
	name string,
	parent, taken, remain, wantTaken, wantRemain CellRect,
) {
	if taken != wantTaken || remain != wantRemain {
		t.Fatalf(
			"%s split mismatch: parent=%#v taken=%#v wantTaken=%#v remain=%#v wantRemain=%#v",
			name,
			parent,
			taken,
			wantTaken,
			remain,
			wantRemain,
		)
	}
	if !rectWithin(taken, parent) || !rectWithin(remain, parent) {
		t.Fatalf("%s split escaped parent: parent=%#v taken=%#v remain=%#v", name, parent, taken, remain)
	}
	if !intersection(taken, remain).Empty() {
		t.Fatalf("%s split overlaps: parent=%#v taken=%#v remain=%#v", name, parent, taken, remain)
	}
	if cellArea(taken)+cellArea(remain) != cellArea(parent) {
		t.Fatalf("%s split changed area: parent=%#v taken=%#v remain=%#v", name, parent, taken, remain)
	}
}

func rectWithin(inner, outer CellRect) bool {
	return inner.MinX >= outer.MinX &&
		inner.MaxX <= outer.MaxX &&
		inner.MinY >= outer.MinY &&
		inner.MaxY <= outer.MaxY
}

func intersection(a, b CellRect) CellRect {
	return NewCellRect(
		max(a.MinX, b.MinX),
		max(a.MinY, b.MinY),
		min(a.MaxX, b.MaxX),
		min(a.MaxY, b.MaxY),
	)
}

func cellArea(rect CellRect) int {
	return rect.Width() * rect.Height()
}
