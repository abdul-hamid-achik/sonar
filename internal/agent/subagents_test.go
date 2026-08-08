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
