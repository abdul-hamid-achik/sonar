package ui

import (
	"testing"

	"charm.land/bubbles/v2/viewport"
)

// These tests used to assert truncation budgets from a layoutConfig that no
// production code ever consulted — passing forever while the values could not
// reach a single rendered cell. What is real is the density classification and
// the measured rectangle it is derived from, which is the seam a working
// compact mode would attach to.
func TestDerivedDensityFollowsTheMeasuredWorkRect(t *testing.T) {
	for _, test := range []struct {
		name    string
		rect    CellRect
		options LayoutCapabilityOptions
		want    LayoutDensity
	}{
		{name: "narrow work rect is compact", rect: NewCellRect(0, 0, 71, 30), want: LayoutDensityCompact},
		{name: "short work rect is compact", rect: NewCellRect(0, 0, 100, 23), want: LayoutDensityCompact},
		{name: "regular work rect", rect: NewCellRect(0, 0, 100, 24), want: LayoutDensityRegular},
		{name: "wide work rect is spacious", rect: NewCellRect(0, 0, 112, 24), want: LayoutDensitySpacious},
		{
			name:    "explicit compact wins over roomy rect",
			rect:    NewCellRect(0, 0, 200, 80),
			options: LayoutCapabilityOptions{ForceCompact: true},
			want:    LayoutDensityCompact,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := DeriveLayoutCapabilities(test.rect, test.options).Density; got != test.want {
				t.Fatalf("density = %v, want %v", got, test.want)
			}
		})
	}
}

// Capabilities are measured from the transcript's own final rectangle, not the
// outer terminal — a docked panel or a tall footer must not make the transcript
// believe it is roomier than it is.
func TestTranscriptCapabilitiesUseTheFinalViewportRect(t *testing.T) {
	m := &Model{
		width:  240,
		height: 80,
		viewport: viewport.New(
			viewport.WithWidth(77),
			viewport.WithHeight(23),
		),
	}
	capabilities := m.transcriptLayoutCapabilities()
	if got, want := capabilities.WorkWidth, 71; got != want {
		t.Fatalf("work width = %d, want viewport width minus transcript chrome = %d", got, want)
	}
	if got, want := capabilities.WorkHeight, 23; got != want {
		t.Fatalf("work height = %d, want final viewport height %d", got, want)
	}
	if capabilities.Density != LayoutDensityCompact {
		t.Fatalf("outer terminal leaked into the density choice: %+v", capabilities)
	}
}

func TestTranscriptCapabilitiesRespectForceCompact(t *testing.T) {
	m := &Model{
		width:        240,
		height:       80,
		forceCompact: true,
		viewport: viewport.New(
			viewport.WithWidth(200),
			viewport.WithHeight(40),
		),
	}
	capabilities := m.transcriptLayoutCapabilities()
	if capabilities.WorkWidth != 194 || capabilities.WorkHeight != 40 {
		t.Fatalf("force compact changed the measured work rect: %+v", capabilities)
	}
	if capabilities.Density != LayoutDensityCompact {
		t.Fatalf("force compact was not applied: %+v", capabilities)
	}
}
