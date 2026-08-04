package llm

import (
	"os"
	"testing"
)

// TestMain isolates this package's tests from the developer's shell. The model
// admission path consults the same SONAR_ALLOW_LARGE_MODELS override that
// config's memory guards use, so an exported value made
// TestModelManagerRejectsOversizedInventoryWeights accept weights it exists to
// reject. See internal/config/main_test.go for the same reasoning.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("SONAR_ALLOW_LARGE_MODELS"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
