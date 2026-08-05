package mcp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
)

func mcpTraceLogger(t *testing.T, level log.Level) (*log.Logger, *bytes.Buffer) {
	t.Helper()
	buffer := &bytes.Buffer{}
	return log.NewWithOptions(buffer, log.Options{Level: level}), buffer
}

// Transport success, domain success, and verified evidence are independent.
// Collapsing them into one "ok" is the standard way to misread an MCP session,
// so the trace must report them as separate fields — a call can complete over
// the wire and still be a refusal.
func TestMCPTraceSeparatesTransportFromDomain(t *testing.T) {
	tests := []struct {
		name         string
		result       *ToolResult
		transportErr error
		want         []string
		absent       []string
	}{
		{
			name:   "completed and accepted",
			result: &ToolResult{Content: "ok"},
			want:   []string{"transport=ok", "domain=ok", "server=bob", "remote=check"},
		},
		{
			name:   "completed but refused by the server",
			result: &ToolResult{Content: "no such recipe", IsError: true},
			want:   []string{"transport=ok", "domain=error"},
		},
		{
			name:         "never completed",
			transportErr: errors.New("dial tcp: connection refused"),
			want:         []string{"transport=failed", "connection refused"},
			// A call that did not complete has no domain outcome to report,
			// and inventing one would claim the server was reached.
			absent: []string{"domain="},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, buffer := mcpTraceLogger(t, log.DebugLevel)
			traceCallOutcome(logger, "exec_1", "bob__check", "bob", "check",
				12*time.Millisecond, test.result, test.transportErr)

			line := buffer.String()
			for _, want := range test.want {
				if !strings.Contains(line, want) {
					t.Errorf("trace is missing %q: %s", want, line)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(line, absent) {
					t.Errorf("trace should not claim %q: %s", absent, line)
				}
			}
		})
	}
}

// The correlation tag is what lets an MCP line be joined to the agent's tool
// lifecycle trace. Matching by tool name and timing stops working exactly when
// it matters: concurrent health checks, retries, or one tool called twice.
func TestMCPTraceCarriesTheExecutionCorrelation(t *testing.T) {
	logger, buffer := mcpTraceLogger(t, log.DebugLevel)
	traceCallOutcome(logger, "exec_abc", "bob__check", "bob", "check", time.Millisecond, &ToolResult{}, nil)
	if !strings.Contains(buffer.String(), "execution=exec_abc") {
		t.Errorf("trace cannot be joined to the lifecycle: %s", buffer.String())
	}

	ctx := WithCallCorrelation(context.Background(), "exec_xyz")
	if got := callCorrelation(ctx); got != "exec_xyz" {
		t.Errorf("correlation = %q, want exec_xyz", got)
	}
	if got := callCorrelation(context.Background()); got != "" {
		t.Errorf("an untagged context reported %q", got)
	}
	//nolint:staticcheck // deliberately probing the nil-context guard
	if WithCallCorrelation(nil, "x") != nil {
		t.Error("a nil context was given a value instead of being passed through")
	}
	if WithCallCorrelation(ctx, "") != ctx {
		t.Error("an empty correlation replaced the context")
	}
}

// Arguments are whatever the model chose to send and result content is remote
// text. Neither belongs in a log line; the shape of the payload is enough to
// debug with, and internal/ecosystem owns anything more.
func TestMCPTraceDoesNotPrintPayloads(t *testing.T) {
	logger, buffer := mcpTraceLogger(t, log.DebugLevel)
	secret := "sk-live-do-not-log-this"
	traceCallOutcome(logger, "exec_1", "bob__check", "bob", "check", time.Millisecond,
		&ToolResult{Content: secret, Structured: []byte(`{"token":"` + secret + `"}`)}, nil)

	line := buffer.String()
	if strings.Contains(line, secret) {
		t.Fatalf("trace printed result content: %s", line)
	}
	for _, want := range []string{"content_bytes=", "structured=true"} {
		if !strings.Contains(line, want) {
			t.Errorf("trace is missing the payload shape %q: %s", want, line)
		}
	}
}

// A refusal is worth surfacing above the routine traffic; a normal completion
// is not, or the levels stop meaning anything.
func TestMCPTraceLevels(t *testing.T) {
	atWarn, warnBuffer := mcpTraceLogger(t, log.WarnLevel)
	traceCallOutcome(atWarn, "", "bob__check", "bob", "check", 0, &ToolResult{IsError: true}, nil)
	if warnBuffer.Len() == 0 {
		t.Error("a server refusal was not raised above debug")
	}

	quiet, quietBuffer := mcpTraceLogger(t, log.InfoLevel)
	traceCallOutcome(quiet, "", "bob__check", "bob", "check", 0, &ToolResult{}, nil)
	if quietBuffer.Len() != 0 {
		t.Errorf("a routine completion was logged above debug: %s", quietBuffer.String())
	}

	atError, errorBuffer := mcpTraceLogger(t, log.ErrorLevel)
	traceCallOutcome(atError, "", "bob__check", "bob", "check", 0, nil, errors.New("refused"))
	if errorBuffer.Len() == 0 {
		t.Error("a transport failure was not raised to error level")
	}
}

// A nil result with a nil error is a client bug. Logging it as a success would
// hide it; the trace has to say the shape was impossible.
func TestMCPTraceRejectsAnImpossibleOutcome(t *testing.T) {
	logger, buffer := mcpTraceLogger(t, log.DebugLevel)
	traceCallOutcome(logger, "", "bob__check", "bob", "check", 0, nil, nil)
	if !strings.Contains(buffer.String(), "neither a result nor an error") {
		t.Errorf("an impossible outcome was logged as ordinary: %s", buffer.String())
	}
}

// A registry without a logger is the ordinary embedding and test case.
func TestMCPTraceWithoutALoggerIsANoop(t *testing.T) {
	traceCallOutcome(nil, "", "bob__check", "bob", "check", 0, nil, errors.New("boom"))
	var registry *Registry
	registry.SetLogger(nil)
	if registry.traceLogger() != nil {
		t.Error("a nil registry produced a logger")
	}
}
