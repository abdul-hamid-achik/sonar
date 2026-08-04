package mcp

import (
	"errors"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

func TestConnectionStatusesAreDeterministicAndFailureWins(t *testing.T) {
	r := NewRegistry()
	r.mu.Lock()
	r.clients["monitor"] = &MCPClient{}
	r.serverTools["monitor"] = nil
	r.clients["cortex"] = &MCPClient{}
	r.serverTools["cortex"] = make([]llm.ToolDef, 3)
	r.setFailedServerLocked("cortex", "connection refused")
	r.mu.Unlock()

	statuses := r.ConnectionStatuses()
	if len(statuses) != 2 || statuses[0].Name != "cortex" || statuses[1].Name != "monitor" {
		t.Fatalf("statuses = %#v", statuses)
	}
	if statuses[0].Connected || statuses[0].LastError != "connection refused" || statuses[0].ToolCount != 3 {
		t.Fatalf("failed status = %#v", statuses[0])
	}
	if !statuses[1].Connected || statuses[1].LastError != "" {
		t.Fatalf("connected status = %#v", statuses[1])
	}
}

func TestConnectionStatusesReturnsDetachedSnapshot(t *testing.T) {
	r := NewRegistry()
	r.setFailedServer("bob", errors.New("boom").Error())

	first := r.ConnectionStatuses()
	first[0].LastError = "changed"
	second := r.ConnectionStatuses()
	if second[0].LastError != "boom" {
		t.Fatalf("status snapshot aliases registry state: %#v", second)
	}
}
