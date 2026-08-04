package main

import (
	"fmt"
	"strings"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

func parseHeadlessMode(value string, headless bool) (ui.Mode, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "normal"
	}
	if !headless && value != "normal" {
		return ui.ModeNormal, fmt.Errorf("--mode, --auto, and --plan require a headless prompt via -p/--prompt")
	}
	switch value {
	case "normal":
		return ui.ModeNormal, nil
	case "plan":
		return ui.ModePlan, nil
	case "auto":
		return ui.ModeAuto, nil
	default:
		return ui.ModeNormal, fmt.Errorf("unknown authority %q (want normal, plan, or auto)", value)
	}
}

func resolveAuthorityShortcut(value string, modeExplicit, auto, plan bool) (string, error) {
	if auto && plan {
		return "", fmt.Errorf("--auto and --plan are mutually exclusive")
	}
	if !auto && !plan {
		return value, nil
	}

	shortcut := "plan"
	flagName := "--plan"
	if auto {
		shortcut = "auto"
		flagName = "--auto"
	}
	if !modeExplicit {
		return shortcut, nil
	}
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == shortcut {
		return shortcut, nil
	}
	return "", fmt.Errorf("%s conflicts with --mode %q", flagName, mode)
}

func headlessAuthorityMode(mode ui.Mode) agent.AuthorityMode {
	switch mode {
	case ui.ModePlan:
		return agent.AuthorityPlan
	case ui.ModeAuto:
		return agent.AuthorityAutoScoped
	default:
		return agent.AuthorityNormal
	}
}
