package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One spawn drives the whole contract: the id comes back immediately, the
// stream is parsed into bounded host state, the receipt settles status,
// tokens, text, and the child session reference, and the depth guard refuses
// a marked process.
func TestSubagentSpawnConsumesStreamAndSettles(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake-sonar")
	script := `#!/bin/sh
echo '{"event":"tool_start","call_id":"c1","name":"read","kind":"builtin"}'
echo '{"event":"tool_result","call_id":"c1","name":"read","kind":"builtin","status":"ok","duration_ms":3}'
echo 'this line is not JSON and must be counted, never stored'
echo '{"event":"text","text":"Hallazgo."}'
echo '{"event":"usage","eval_tokens":7,"prompt_tokens":50}'
echo '{"schema":"sonar.turn-receipt.v1","status":"settled","stop_reason":"completed","usage":{"prompt_tokens":50,"eval_tokens":7},"session":{"id":9,"public_id":"abc1234","workspace":"/w"},"text":"Hallazgo."}'
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	ag.subagentExecutable = fake

	started, isErr := ag.handleAgentSpawn(map[string]any{"prompt": "investiga la cosa", "name": "explorer"})
	if isErr || !strings.Contains(started, `"a1"`) {
		t.Fatalf("spawn = %q, %v", started, isErr)
	}

	deadline := time.Now().Add(5 * time.Second)
	var final string
	for {
		final, isErr = ag.handleAgentOutput(map[string]any{"id": "a1"})
		if isErr {
			t.Fatalf("agent_output errored: %q", final)
		}
		if strings.Contains(final, "done") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("subagent never settled: %q", final)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(final, "Hallazgo.") || !strings.Contains(final, "abc1234") {
		t.Fatalf("settled output lost the answer or the session ref: %q", final)
	}

	snapshots := ag.SubagentSnapshots()
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.Status != "done" || snapshot.EvalTokens != 7 || snapshot.ToolCalls != 1 ||
		snapshot.SessionRef != "abc1234" || snapshot.Name != "explorer" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Dropped == 0 {
		t.Fatal("the non-JSON line was stored instead of counted")
	}
	for _, event := range snapshot.Events {
		if strings.Contains(event.Message, "not JSON") {
			t.Fatal("unparseable stream content crossed into host state")
		}
	}

	if _, isErr := ag.handleAgentOutput(map[string]any{"id": "zz"}); !isErr {
		t.Fatal("unknown subagent id did not error")
	}

	t.Setenv(sonarSubagentEnv, "1")
	if refused, isErr := ag.handleAgentSpawn(map[string]any{"prompt": "recurse"}); !isErr {
		t.Fatalf("marked process spawned a child: %q", refused)
	}
}

// A failing child settles as failed with its exit error, and the tool result
// reports it as an error so the model routes around it.
func TestSubagentFailureSettlesAsFailed(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake-sonar")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"event\":\"error\",\"message\":\"boom\"}'\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	ag.subagentExecutable = fake

	if _, isErr := ag.handleAgentSpawn(map[string]any{"prompt": "va a fallar"}); isErr {
		t.Fatal("spawn itself failed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		output, isErr := ag.handleAgentOutput(map[string]any{"id": "a1"})
		if strings.Contains(output, "failed") {
			if !isErr {
				t.Fatalf("failed child was not reported as a tool error: %q", output)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never settled: %q", output)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Each vendor parser is fed the exact line shapes measured live against the
// installed CLIs (claude 2.1.226, codex-cli 0.147.0, grok 1.0.0): the answer
// text, tool activity, usage, and the settled status must all survive, and
// unknown lines must count as drops rather than crossing into host state.
func TestVendorSubagentParsersConsumeMeasuredStreams(t *testing.T) {
	run := func(provider string, lines []string) SubagentSnapshot {
		t.Helper()
		fake := filepath.Join(t.TempDir(), "fake-cli")
		script := "#!/bin/sh\n"
		for _, line := range lines {
			script += "echo '" + line + "'\n"
		}
		if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		ag := New(nil, nil, 4096)
		ag.SetWorkDir(t.TempDir())
		ag.subagentExecutable = fake
		if started, isErr := ag.handleAgentSpawn(map[string]any{"prompt": "investiga", "provider": provider}); isErr {
			t.Fatalf("%s spawn failed: %q", provider, started)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			snapshots := ag.SubagentSnapshots()
			if len(snapshots) == 1 && snapshots[0].Status != "running" {
				return snapshots[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s child never settled: %+v", provider, snapshots)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	claude := run("claude", []string{
		`{"type":"system","subtype":"hook_started","hook_id":"x"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"},{"type":"text","text":"hola desde claude"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"hola desde claude","usage":{"output_tokens":8}}`,
	})
	if claude.Provider != "claude" || claude.Status != "done" || claude.Text != "hola desde claude" || claude.EvalTokens != 8 {
		t.Fatalf("claude snapshot = %+v", claude)
	}

	codex := run("codex", []string{
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"command_execution"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"hola desde codex"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":16961,"output_tokens":8}}`,
	})
	if codex.Status != "done" || codex.Text != "hola desde codex" || codex.EvalTokens != 8 || codex.ToolCalls != 1 {
		t.Fatalf("codex snapshot = %+v", codex)
	}

	grok := run("grok", []string{
		`{"type":"available_commands","tools":["read_file"]}`,
		`{"type":"thought","data":"pensando"}`,
		`{"type":"tool_call","toolCallId":"c1","toolName":"read_file","status":"pending"}`,
		`{"type":"tool_call_update","toolCallId":"c1","status":"completed"}`,
		`{"type":"text","data":"hola "}`,
		`{"type":"text","data":"desde grok"}`,
		`{"type":"usage","usage":{"input_tokens":21805,"output_tokens":45}}`,
		`{"type":"end","stopReason":"end_turn","sessionId":"s"}`,
		`garbage line that must be counted`,
	})
	if grok.Status != "done" || grok.Text != "hola desde grok" || grok.EvalTokens != 45 || grok.ToolCalls != 1 {
		t.Fatalf("grok snapshot = %+v", grok)
	}
	if grok.Dropped == 0 {
		t.Fatal("the garbage line was not counted as a drop")
	}
	if strings.Contains(grok.Text, "pensando") {
		t.Fatal("thought deltas leaked into the answer text")
	}
}

// TestLiveVendorSubagent spends real subscription quota and is therefore
// opt-in: set SONAR_LIVE_SUBAGENT to claude, codex, or grok. It proves the
// whole path — argv confinement, live stream parsing, settlement — against
// the actually-installed CLI.
func TestLiveVendorSubagent(t *testing.T) {
	provider := strings.TrimSpace(os.Getenv("SONAR_LIVE_SUBAGENT"))
	if provider == "" {
		t.Skip("set SONAR_LIVE_SUBAGENT=claude|codex|grok to run against the real CLI")
	}
	ag := New(nil, nil, 4096)
	ag.SetWorkDir(t.TempDir())
	started, isErr := ag.handleAgentSpawn(map[string]any{
		"prompt": "Reply with exactly: subagente vivo", "provider": provider, "name": "live",
	})
	if isErr {
		t.Fatalf("live spawn failed: %q", started)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		snapshots := ag.SubagentSnapshots()
		if len(snapshots) == 1 && snapshots[0].Status != "running" {
			snapshot := snapshots[0]
			if snapshot.Status != "done" || !strings.Contains(snapshot.Text, "subagente vivo") {
				t.Fatalf("live %s child = %+v", provider, snapshot)
			}
			t.Logf("live %s: %q · %d tokens", provider, snapshot.Text, snapshot.EvalTokens)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("live %s child never settled", provider)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
