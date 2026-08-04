package main

import "testing"

func TestResolveEffectiveVersion(t *testing.T) {
	tests := []struct {
		name           string
		linked         string
		module         string
		sourceCheckout bool
		want           string
	}{
		{name: "goreleaser linker value", linked: "0.18.0", module: "v0.17.0", want: "0.18.0"},
		{name: "linker value with prefix", linked: "v0.18.0", module: "v0.17.0", want: "0.18.0"},
		{name: "go install module fallback", linked: "dev", module: "v0.17.0", want: "0.17.0"},
		{name: "pseudo version fallback", linked: "dev", module: "v0.18.1-0.20260718120000-deadbeef", want: "0.18.1-0.20260718120000-deadbeef"},
		{name: "dirty source checkout pseudo version", linked: "dev", module: "v0.18.1-0.20260718120000-deadbeef+dirty", sourceCheckout: true, want: "dev"},
		{name: "linked release wins in checkout", linked: "0.18.0", module: "v0.17.1-0.20260718120000-deadbeef+dirty", sourceCheckout: true, want: "0.18.0"},
		{name: "source checkout", linked: "dev", module: "(devel)", want: "dev"},
		{name: "empty metadata", want: "dev"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveEffectiveVersion(
				test.linked,
				test.module,
				test.sourceCheckout,
			); got != test.want {
				t.Fatalf("effective version = %q, want %q", got, test.want)
			}
		})
	}
}
