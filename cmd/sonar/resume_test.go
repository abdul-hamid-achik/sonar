package main

import (
	"strings"
	"testing"
)

func TestResumeFlagValueTracksOmittedAndEmpty(t *testing.T) {
	var omitted resumeFlagValue
	if _, configured, err := omitted.selector(); err != nil || configured {
		t.Fatalf("omitted selector configured=%v error=%v", configured, err)
	}

	var empty resumeFlagValue
	if err := empty.Set(""); err != nil {
		t.Fatal(err)
	}
	if _, configured, err := empty.selector(); err == nil || !configured {
		t.Fatalf("empty selector configured=%v error=%v", configured, err)
	}

	var latest resumeFlagValue
	if err := latest.Set("latest"); err != nil {
		t.Fatal(err)
	}
	if _, configured, err := latest.selector(); err != nil || !configured {
		t.Fatalf("latest selector configured=%v error=%v", configured, err)
	}
}

func TestValidateResumeInvocationRejectsHeadlessPrompt(t *testing.T) {
	if err := validateResumeInvocation(true, true); err == nil || !strings.Contains(err.Error(), "interactive TUI") {
		t.Fatalf("resume/headless error = %v", err)
	}
	for _, invocation := range []struct {
		resume bool
		prompt bool
	}{
		{resume: false, prompt: false},
		{resume: false, prompt: true},
		{resume: true, prompt: false},
	} {
		if err := validateResumeInvocation(invocation.resume, invocation.prompt); err != nil {
			t.Fatalf("valid invocation %#v: %v", invocation, err)
		}
	}
}
