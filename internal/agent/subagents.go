package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// Subagents.
//
// A subagent is a child `sonar -p --json-stream --plan` process: a real
// headless run with its own session in the shared store, confined to PLAN
// authority so it can read and reason but never mutate or prompt for
// approval. The parent consumes the child's JSONL stream as bounded
// presentation data — never authority — and keeps a ring of recent events so
// the host UI can show what each child is doing while it runs. The child's
// durable transcript needs no extra machinery: headless runs already persist
// sessions, so the session picker is the post-mortem viewer.
//
// `agent` starts a child and returns immediately with an id, mirroring
// bash(background=true): dispatch stays deterministic in model order while
// children run in parallel. `agent_output` is the genuinely read-only
// observation of host-owned state, exactly like bash_output.
//
// Children never spawn children: the parent marks the child environment with
// sonarSubagentEnv, and a marked process neither exposes nor admits the spawn
// tool. PLAN authority already refuses it; the environment mark keeps the
// depth guard true even if a future mode widens the child's policy.
const (
	maxSubagents          = 4
	maxSubagentEvents     = 256
	maxSubagentLineBytes  = 256 * 1024
	maxSubagentTextBytes  = 64 * 1024
	maxSubagentPromptSize = 16 * 1024
	sonarSubagentEnv      = "SONAR_SUBAGENT"
)

// SubagentEvent is one bounded, parsed line of a child's stream, retained for
// host presentation. Raw lines the parent cannot parse are counted, never
// stored: the stream crosses a process boundary and stays data.
type SubagentEvent struct {
	Kind       string
	Name       string
	Status     string
	Message    string
	DurationMS int64
	At         time.Time
}

// SubagentSnapshot is the host-facing view of one child for panels/viewers.
type SubagentSnapshot struct {
	ID          string
	Name        string
	Prompt      string
	Status      string // running · done · failed
	Started     time.Time
	Finished    time.Time
	EvalTokens  int64
	ToolCalls   int
	Events      []SubagentEvent
	Dropped     int
	Text        string
	StopReason  string
	SessionRef  string // child session public id, from the receipt
	LastEventAt time.Time
}

type subagentProcess struct {
	id     string
	name   string
	prompt string
	cmd    *exec.Cmd

	mu          sync.Mutex
	status      string
	started     time.Time
	finished    time.Time
	events      []SubagentEvent
	dropped     int
	evalTokens  int64
	toolCalls   int
	text        strings.Builder
	textOmitted bool
	stopReason  string
	sessionRef  string
	lastEventAt time.Time
}

type subagentRegistry struct {
	mu      sync.Mutex
	counter int
	procs   map[string]*subagentProcess
}

func (a *Agent) subagentRegistryHandle() *subagentRegistry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subagents == nil {
		a.subagents = &subagentRegistry{procs: make(map[string]*subagentProcess)}
	}
	return a.subagents
}

// runningSubagentProcess reports whether this process is itself a subagent.
func runningAsSubagent() bool {
	return strings.TrimSpace(os.Getenv(sonarSubagentEnv)) != ""
}

func isSubagentTool(name string) bool {
	return name == "agent" || name == "agent_output"
}

// AgentToolDef is the spawn schema. Defined here rather than internal/tools:
// that package is drift-synced byte-identical with the sibling repository, and
// subagents are (so far) sonar-only.
func agentToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "agent",
		Description: "Start a read-only subagent: a parallel worker that explores the workspace and reports back. It can read, grep, and reason; it cannot write, run commands, or use MCP. Returns an id immediately — continue other work, then collect findings with agent_output. Give it one self-contained research question.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "The complete, self-contained task. The subagent has no access to this conversation.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Short label for progress displays (e.g. 'auth-flow').",
				},
			},
			"required": []string{"prompt"},
		},
	}
}

func agentOutputToolDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        "agent_output",
		Description: "Read a subagent's progress and, once finished, its full answer. Poll sparingly: prefer doing other work between polls.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The id returned by agent.",
				},
			},
			"required": []string{"id"},
		},
	}
}

func (a *Agent) handleAgentSpawn(args map[string]any) (string, bool) {
	if runningAsSubagent() {
		return "error: a subagent cannot start subagents", true
	}
	prompt, _ := args["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "error: prompt is required", true
	}
	if len(prompt) > maxSubagentPromptSize {
		return fmt.Sprintf("error: prompt exceeds %d bytes", maxSubagentPromptSize), true
	}
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	workDir := strings.TrimSpace(a.activeWorkDir())
	if workDir == "" {
		return "error: subagents need an active workspace", true
	}
	registry := a.subagentRegistryHandle()

	registry.mu.Lock()
	running := 0
	for _, proc := range registry.procs {
		proc.mu.Lock()
		if proc.status == "running" {
			running++
		}
		proc.mu.Unlock()
	}
	if running >= maxSubagents {
		registry.mu.Unlock()
		return fmt.Sprintf("error: %d subagents already running; collect one with agent_output before starting more", running), true
	}
	registry.counter++
	id := fmt.Sprintf("a%d", registry.counter)
	registry.mu.Unlock()

	executable := a.subagentExecutable
	if executable == "" {
		resolved, err := os.Executable()
		if err != nil {
			return fmt.Sprintf("error: resolve sonar binary: %v", err), true
		}
		executable = resolved
	}
	cmd := exec.Command(executable, "-p", prompt, "--json-stream", "--plan", "--actor", "subagent:"+id)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), sonarSubagentEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Sprintf("error: subagent pipe: %v", err), true
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("error: start subagent: %v", err), true
	}

	proc := &subagentProcess{
		id: id, name: name, prompt: prompt, cmd: cmd,
		status: "running", started: time.Now(),
	}
	registry.mu.Lock()
	registry.procs[id] = proc
	registry.mu.Unlock()

	go func() {
		// Consume to EOF before Wait: the os/exec contract forbids Wait while
		// pipe reads are pending, and settling status before the receipt line
		// has been parsed would report a finished child with no answer.
		proc.consume(stdout)
		err := cmd.Wait()
		proc.mu.Lock()
		defer proc.mu.Unlock()
		proc.finished = time.Now()
		if err != nil {
			proc.status = "failed"
			if proc.stopReason == "" {
				proc.stopReason = err.Error()
			}
		} else if proc.status == "running" {
			// Exit 0 without a receipt line still counts as done; the
			// receipt is presentation, the exit code is the contract.
			proc.status = "done"
		}
	}()

	label := id
	if name != "" {
		label = id + " (" + name + ")"
	}
	return fmt.Sprintf("Started subagent %s in the background. It reads and reasons only. Collect progress or the final answer with agent_output {\"id\": %q}.", label, id), false
}

// consume parses the child's JSONL stream into the bounded event ring. The
// final receipt line (recognised by its schema field) settles status, tokens,
// text, and the child's session reference.
func (p *subagentProcess) consume(stdout interface{ Read([]byte) (int, error) }) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxSubagentLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		var probe struct {
			Event  string `json:"event"`
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			p.mu.Lock()
			p.dropped++
			p.mu.Unlock()
			continue
		}
		if probe.Schema != "" {
			var receipt TurnReceipt
			if err := json.Unmarshal(line, &receipt); err != nil {
				p.mu.Lock()
				p.dropped++
				p.mu.Unlock()
				continue
			}
			p.mu.Lock()
			p.status = "done"
			if receipt.Status != "" && receipt.Status != "settled" {
				p.status = "failed"
			}
			p.stopReason = string(receipt.StopReason)
			p.evalTokens = receipt.Usage.EvalTokens
			if receipt.Session != nil {
				p.sessionRef = receipt.Session.PublicID
			}
			if p.text.Len() == 0 && receipt.Text != "" {
				p.appendTextLocked(receipt.Text)
			}
			p.lastEventAt = time.Now()
			p.mu.Unlock()
			continue
		}
		var event streamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			p.mu.Lock()
			p.dropped++
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		switch event.Event {
		case "text":
			p.appendTextLocked(event.Text)
		case "usage":
			p.evalTokens += event.EvalTokens
		case "tool_result":
			p.toolCalls++
		}
		if len(p.events) >= maxSubagentEvents {
			copy(p.events, p.events[1:])
			p.events = p.events[:maxSubagentEvents-1]
			p.dropped++
		}
		p.events = append(p.events, SubagentEvent{
			Kind: event.Event, Name: event.Name, Status: event.Status,
			Message: event.Message, DurationMS: event.DurationMS, At: time.Now(),
		})
		p.lastEventAt = time.Now()
		p.mu.Unlock()
	}
}

func (p *subagentProcess) appendTextLocked(text string) {
	remaining := maxSubagentTextBytes - p.text.Len()
	if remaining <= 0 {
		p.textOmitted = true
		return
	}
	if len(text) > remaining {
		text = truncateApprovalUTF8(text, remaining)
		p.textOmitted = true
	}
	p.text.WriteString(text)
}

func (a *Agent) handleAgentOutput(args map[string]any) (string, bool) {
	id, _ := args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return "error: id is required", true
	}
	registry := a.subagentRegistryHandle()
	registry.mu.Lock()
	proc, ok := registry.procs[id]
	registry.mu.Unlock()
	if !ok {
		return fmt.Sprintf("error: no subagent %q in this session", id), true
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()
	var b strings.Builder
	label := proc.id
	if proc.name != "" {
		label += " (" + proc.name + ")"
	}
	fmt.Fprintf(&b, "subagent %s · %s", label, proc.status)
	if proc.evalTokens > 0 {
		fmt.Fprintf(&b, " · %d eval tokens", proc.evalTokens)
	}
	if proc.toolCalls > 0 {
		fmt.Fprintf(&b, " · %d tool calls", proc.toolCalls)
	}
	b.WriteString("\n")
	switch proc.status {
	case "running":
		recent := proc.events
		if len(recent) > 8 {
			recent = recent[len(recent)-8:]
		}
		for _, event := range recent {
			switch event.Kind {
			case "tool_start":
				fmt.Fprintf(&b, "  → %s\n", event.Name)
			case "tool_result":
				fmt.Fprintf(&b, "  ✓ %s (%s, %dms)\n", event.Name, event.Status, event.DurationMS)
			case "error":
				fmt.Fprintf(&b, "  ! %s\n", event.Message)
			}
		}
		b.WriteString("Still working. Do other useful work before polling again.")
	default:
		if proc.stopReason != "" && proc.stopReason != "completed" {
			fmt.Fprintf(&b, "stop reason: %s\n", proc.stopReason)
		}
		if proc.sessionRef != "" {
			fmt.Fprintf(&b, "full transcript: session %s\n", proc.sessionRef)
		}
		b.WriteString("\n")
		b.WriteString(proc.text.String())
		if proc.textOmitted {
			b.WriteString("\n[answer truncated at the retention bound; the child session holds the full text]")
		}
	}
	return b.String(), proc.status == "failed"
}

// SubagentSnapshots returns a bounded copy of every child for host surfaces,
// newest first.
func (a *Agent) SubagentSnapshots() []SubagentSnapshot {
	a.mu.RLock()
	registry := a.subagents
	a.mu.RUnlock()
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	procs := make([]*subagentProcess, 0, len(registry.procs))
	for _, proc := range registry.procs {
		procs = append(procs, proc)
	}
	registry.mu.Unlock()

	snapshots := make([]SubagentSnapshot, 0, len(procs))
	for _, proc := range procs {
		proc.mu.Lock()
		snapshots = append(snapshots, SubagentSnapshot{
			ID: proc.id, Name: proc.name, Prompt: proc.prompt, Status: proc.status,
			Started: proc.started, Finished: proc.finished,
			EvalTokens: proc.evalTokens, ToolCalls: proc.toolCalls,
			Events:  append([]SubagentEvent(nil), proc.events...),
			Dropped: proc.dropped, Text: proc.text.String(),
			StopReason: proc.stopReason, SessionRef: proc.sessionRef,
			LastEventAt: proc.lastEventAt,
		})
		proc.mu.Unlock()
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Started.After(snapshots[j].Started) })
	return snapshots
}

// closeSubagents terminates every child. Children are read-only, so a hard
// kill loses nothing durable; their sessions persist whatever they settled.
func (a *Agent) closeSubagents() {
	a.mu.RLock()
	registry := a.subagents
	a.mu.RUnlock()
	if registry == nil {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, proc := range registry.procs {
		proc.mu.Lock()
		running := proc.status == "running"
		proc.mu.Unlock()
		if running && proc.cmd != nil && proc.cmd.Process != nil {
			_ = proc.cmd.Process.Kill()
		}
	}
}
