package main

import (
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/ui"
)

type resumeFlagValue struct {
	set   bool
	value string
}

func (f *resumeFlagValue) String() string {
	if f == nil {
		return ""
	}
	return f.value
}

// Set records presence separately from value so --resume= fails closed while
// an omitted --resume remains the ordinary fresh-session startup.
func (f *resumeFlagValue) Set(value string) error {
	f.set = true
	f.value = value
	return nil
}

func (f resumeFlagValue) selector() (ui.SessionResumeSelector, bool, error) {
	if !f.set {
		return ui.SessionResumeSelector{}, false, nil
	}
	selector, err := ui.ParseSessionResumeSelector(f.value)
	if err != nil {
		return ui.SessionResumeSelector{}, true, err
	}
	return selector, true, nil
}

func validateResumeInvocation(resumeRequested, promptFlagProvided bool) error {
	if resumeRequested && promptFlagProvided {
		return fmt.Errorf("--resume is available only for the interactive TUI and cannot be combined with -p/--prompt")
	}
	return nil
}
