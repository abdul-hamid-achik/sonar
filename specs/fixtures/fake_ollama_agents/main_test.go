package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConfiguredExpertDelay(t *testing.T) {
	if got, err := configuredExpertDelay(""); err != nil || got != defaultExpertDelay {
		t.Fatalf("default delay = %s, %v", got, err)
	}
	if got, err := configuredExpertDelay("25ms"); err != nil || got != 25*time.Millisecond {
		t.Fatalf("configured delay = %s, %v", got, err)
	}
	for _, value := range []string{"invalid", "-1s", "31s"} {
		if _, err := configuredExpertDelay(value); err == nil {
			t.Fatalf("configuredExpertDelay(%q) succeeded", value)
		}
	}
}

func TestFixturePerformsOneConsultationProtocol(t *testing.T) {
	state := newFixtureState()
	server := httptest.NewServer(fixtureHandler(state, 0))
	defer server.Close()

	parent := chatRequest{
		Model: fixtureModel,
		Tools: []chatTool{{Function: struct {
			Name string `json:"name"`
		}{Name: "consult_experts"}}},
	}
	first := postChat(t, server.URL, parent)
	if !strings.Contains(first, expertCallID) || !strings.Contains(first, "consult_experts") {
		t.Fatalf("parent response = %q", first)
	}

	disabled := false
	expert := chatRequest{
		Model: fixtureModel,
		Messages: []chatMessage{
			{Role: "system", Content: "You are one member of a " + expertContractProbe + "."},
			{Role: "user", Content: "Review the Agent Hub interaction contract."},
		},
		Think: &disabled,
	}
	second := postChat(t, server.URL, expert)
	if !strings.Contains(second, "Advisory report") {
		t.Fatalf("expert response = %q", second)
	}

	followup := chatRequest{
		Model: fixtureModel,
		Messages: []chatMessage{{
			Role: "tool", Content: "safe bounded result",
			ToolCallID: expertCallID, ToolName: "consult_experts",
		}},
	}
	third := postChat(t, server.URL, followup)
	if !strings.Contains(third, "completed") {
		t.Fatalf("follow-up response = %q", third)
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.protocolError != "" ||
		state.requests[requestParentInitial] != 1 ||
		state.requests[requestExpert] != 1 ||
		state.requests[requestParentFollowup] != 1 {
		t.Fatalf("fixture state = %#v", state)
	}
}

func postChat(t *testing.T, baseURL string, request chatRequest) string {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+"/api/chat", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var body bytes.Buffer
	if _, err := body.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/chat = %d: %s", response.StatusCode, body.String())
	}
	return body.String()
}
