package config

import (
	"os"
	"testing"
)

// TestMain isolates this package's tests from the developer's shell.
//
// The memory guards read SONAR_ALLOW_LARGE_MODELS directly, so a machine
// that exports it — a reasonable thing to do when you actually want to run a
// large model — silently turned every guard assertion into a pass-through and
// TestMemoryRiskyModelGuard / TestClampNumCtxForMemory failed with results that
// looked like hardware sensitivity rather than a leaked environment.
//
// Clearing it here, rather than in each test, means a test added later inherits
// the isolation instead of having to remember it. Tests that exercise the
// override still opt in explicitly with t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("SONAR_ALLOW_LARGE_MODELS"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
