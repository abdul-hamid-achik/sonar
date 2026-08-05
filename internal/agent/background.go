package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Background shell execution.
//
// A background start is an ordinary `bash` dispatch carrying background=true.
// It is deliberately not a separate tool: authorization for shell work is
// decided from the `command` string alone (permission policy, durable
// workspace rules, session bash-prefix grants, and the AUTO scoped-command
// classifier all read only that argument), so a backgrounded command travels
// the exact same approval path as the foreground one and backgrounding cannot
// become a way around the classifier.
//
// Reading a started process back is a separate tool, `bash_output`, because it
// is a genuinely read-only observation of a host-owned buffer. Folding it into
// `bash` would mean teaching the most dangerous tool in the catalog an
// argument-shaped read-only exemption; a distinct tool that cannot accept a
// command string has no such failure mode, and the durable ledger classifies
// it read-only by name instead of by argument inspection.
const (
	// maxBackgroundStreamBytes bounds the output retained per stream, per
	// background process. A watch build or a log tail can emit megabytes, so
	// retention is a ring: the newest bytes win and the count of dropped bytes
	// is reported in the read-back receipt rather than silently discarded.
	maxBackgroundStreamBytes = 128 * 1024
	// maxBackgroundReadBytes bounds one read-back delivery per stream so a
	// single poll cannot flood the provider context with retained output.
	maxBackgroundReadBytes = 16 * 1024
	// maxBackgroundProcesses bounds concurrently tracked processes per session.
	maxBackgroundProcesses = 8
	// backgroundWaitDelay mirrors the foreground shell: a descendant that
	// inherited the pipe must not keep the reaper waiting forever.
	backgroundWaitDelay = 2 * time.Second
)

// boundedStream retains at most limit bytes of one output stream while keeping
// exact accounting of everything written, so a reader can be told how much it
// missed instead of receiving a silently shortened transcript.
type boundedStream struct {
	mu      sync.Mutex
	buf     []byte
	limit   int
	start   int64 // absolute offset of buf[0] within the full stream
	written int64 // total bytes ever written
}

func (s *boundedStream) Write(p []byte) (int, error) {
	written := len(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written += int64(written)
	if s.limit <= 0 {
		return written, nil
	}
	if len(p) >= s.limit {
		s.buf = append(s.buf[:0], p[len(p)-s.limit:]...)
		s.start = s.written - int64(len(s.buf))
		return written, nil
	}
	s.buf = append(s.buf, p...)
	if overflow := len(s.buf) - s.limit; overflow > 0 {
		kept := copy(s.buf, s.buf[overflow:])
		s.buf = s.buf[:kept]
		s.start += int64(overflow)
	}
	return written, nil
}

// readSince returns the retained bytes after cursor, the cursor to use next,
// and how many bytes in that span could not be delivered — either because
// retention already dropped them or because the span exceeded one read.
func (s *boundedStream) readSince(cursor int64) (text string, next int64, omitted int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor < 0 {
		cursor = 0
	}
	if cursor > s.written {
		cursor = s.written
	}
	from := cursor
	if from < s.start {
		omitted += s.start - from
		from = s.start
	}
	slice := s.buf[from-s.start:]
	if len(slice) > maxBackgroundReadBytes {
		omitted += int64(len(slice) - maxBackgroundReadBytes)
		slice = slice[len(slice)-maxBackgroundReadBytes:]
	}
	return string(slice), s.written, omitted
}

func (s *boundedStream) totalWritten() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// backgroundProcess is one host-owned shell process plus its bounded capture.
type backgroundProcess struct {
	id      string
	command string
	pid     int
	started time.Time
	cmd     *exec.Cmd
	stdout  boundedStream
	stderr  boundedStream
	done    chan struct{}

	mu           sync.Mutex
	stdoutCursor int64
	stderrCursor int64
	exited       bool
	finishedAt   time.Time
	waitErr      error
	killedByHost bool
}

func (p *backgroundProcess) finish(err error, killedByHost bool) {
	p.mu.Lock()
	p.exited = true
	p.finishedAt = time.Now()
	p.waitErr = err
	p.killedByHost = killedByHost
	p.mu.Unlock()
}

// statusLocked renders the lifecycle phrase. The caller holds p.mu, which also
// orders the read of cmd.ProcessState after the Wait that published it.
func (p *backgroundProcess) statusLocked(now time.Time) string {
	if !p.exited {
		return fmt.Sprintf("running · pid %d · started %s ago", p.pid, roundedDuration(now.Sub(p.started)))
	}
	ran := roundedDuration(p.finishedAt.Sub(p.started))
	state := p.cmd.ProcessState
	switch {
	case p.killedByHost:
		return fmt.Sprintf("terminated by host shutdown after %s", ran)
	case state == nil:
		return fmt.Sprintf("ended without a process status after %s: %v", ran, p.waitErr)
	case state.Exited():
		return fmt.Sprintf("exited with status %d after %s", state.ExitCode(), ran)
	default:
		return fmt.Sprintf("terminated before exiting after %s (%s)", ran, state.String())
	}
}

func (p *backgroundProcess) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.exited
}

// drained reports whether every retained byte has already been delivered to a
// reader. Only drained, exited processes may be evicted to make room.
func (p *backgroundProcess) drained() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited &&
		p.stdoutCursor >= p.stdout.totalWritten() &&
		p.stderrCursor >= p.stderr.totalWritten()
}

func (p *backgroundProcess) summaryLine(now time.Time) string {
	p.mu.Lock()
	status := p.statusLocked(now)
	p.mu.Unlock()
	return fmt.Sprintf("- %s · %s · %s", p.id, status, singleLineCommand(p.command))
}

// render returns the new-since-last-read output plus whether the process is
// still running and, once known, how it ended.
func (p *backgroundProcess) render(now time.Time) string {
	p.mu.Lock()
	status := p.statusLocked(now)
	stillRunning := !p.exited
	stdoutText, stdoutNext, stdoutOmitted := p.stdout.readSince(p.stdoutCursor)
	p.stdoutCursor = stdoutNext
	stderrText, stderrNext, stderrOmitted := p.stderr.readSince(p.stderrCursor)
	p.stderrCursor = stderrNext
	p.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s\n", p.id, status)
	fmt.Fprintf(&b, "command: %s\n", singleLineCommand(p.command))
	b.WriteString(renderBackgroundStream("stdout", stdoutText, stdoutOmitted, stdoutNext))
	b.WriteString(renderBackgroundStream("stderr", stderrText, stderrOmitted, stderrNext))
	if stillRunning {
		b.WriteString("\nStill running. Read this id again for output produced since this read.")
	}
	return b.String()
}

func renderBackgroundStream(name, text string, omitted, total int64) string {
	var b strings.Builder
	if text == "" {
		fmt.Fprintf(&b, "\n--- %s: no new output (%d byte(s) produced in total) ---\n", name, total)
	} else {
		fmt.Fprintf(&b, "\n--- %s: %d new byte(s) (%d byte(s) produced in total) ---\n", name, len(text), total)
		b.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			b.WriteString("\n")
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "... (%d earlier %s byte(s) omitted: the host retains at most %d byte(s) per stream and delivers at most %d byte(s) per read)\n",
			omitted, name, maxBackgroundStreamBytes, maxBackgroundReadBytes)
	}
	return b.String()
}

func singleLineCommand(command string) string {
	const limit = 200
	collapsed := strings.Join(strings.Fields(command), " ")
	if len(collapsed) > limit {
		return collapsed[:limit] + "…"
	}
	return collapsed
}

func roundedDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// backgroundRegistry owns every background process started by one Agent. Its
// context is cancelled exactly once, at Agent.Close, so no background process
// can outlive the harness that started it.
type backgroundRegistry struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	procs  map[string]*backgroundProcess
	order  []string
	seq    int
	closed bool
}

func newBackgroundRegistry() *backgroundRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	return &backgroundRegistry{ctx: ctx, cancel: cancel, procs: make(map[string]*backgroundProcess)}
}

var errBackgroundRegistryClosed = errors.New("the session is shutting down; no new background process was started")

func (r *backgroundRegistry) start(command, workDir string, env []string) (*backgroundProcess, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errBackgroundRegistryClosed
	}
	r.evictDrainedLocked()
	if len(r.procs) >= maxBackgroundProcesses {
		retained := r.idsLocked()
		r.mu.Unlock()
		return nil, fmt.Errorf(
			"this session already tracks %d background process(es) (%s); read one to completion with bash_output before starting another",
			maxBackgroundProcesses, strings.Join(retained, ", "))
	}
	r.seq++
	id := fmt.Sprintf("bg_%d", r.seq)
	ctx := r.ctx
	r.mu.Unlock()

	proc := &backgroundProcess{id: id, command: command, started: time.Now(), done: make(chan struct{})}
	proc.stdout.limit = maxBackgroundStreamBytes
	proc.stderr.limit = maxBackgroundStreamBytes

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	configureCommandProcessGroup(cmd)
	cmd.WaitDelay = backgroundWaitDelay
	cmd.Dir = workDir
	// Match the foreground shell exactly: LLM-generated commands never receive
	// the parent process environment.
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = &proc.stdout
	cmd.Stderr = &proc.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	proc.cmd = cmd
	proc.pid = cmd.Process.Pid

	r.mu.Lock()
	if r.closed {
		// close() raced this start. The registry context is already cancelled,
		// so reap synchronously here rather than registering a process no
		// shutdown path is waiting for.
		r.mu.Unlock()
		_ = cmd.Wait()
		cleanupCommandProcessGroup(cmd)
		return nil, errBackgroundRegistryClosed
	}
	r.procs[id] = proc
	r.order = append(r.order, id)
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		err := cmd.Wait()
		proc.finish(err, ctx.Err() != nil)
		close(proc.done)
	}()
	return proc, nil
}

// evictDrainedLocked frees slots held by exited processes whose output has
// already been delivered. Undelivered output is never discarded to make room:
// the start fails with an actionable message instead.
func (r *backgroundRegistry) evictDrainedLocked() {
	retained := r.order[:0]
	for _, id := range r.order {
		proc, ok := r.procs[id]
		if !ok {
			continue
		}
		if proc.drained() {
			delete(r.procs, id)
			continue
		}
		retained = append(retained, id)
	}
	r.order = retained
}

func (r *backgroundRegistry) lookup(id string) (*backgroundProcess, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	proc, ok := r.procs[id]
	return proc, ok
}

func (r *backgroundRegistry) idsLocked() []string {
	ids := make([]string, 0, len(r.order))
	ids = append(ids, r.order...)
	sort.Strings(ids)
	return ids
}

func (r *backgroundRegistry) list(now time.Time) string {
	r.mu.Lock()
	procs := make([]*backgroundProcess, 0, len(r.order))
	for _, id := range r.order {
		if proc, ok := r.procs[id]; ok {
			procs = append(procs, proc)
		}
	}
	r.mu.Unlock()
	if len(procs) == 0 {
		return "No background processes have been started in this session. Start one with bash and background=true."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d background process(es) in this session:\n", len(procs))
	for _, proc := range procs {
		b.WriteString(proc.summaryLine(now))
		b.WriteString("\n")
	}
	b.WriteString("Read one with bash_output and its id.")
	return b.String()
}

// close cancels every tracked process group and joins its reaper. It is
// idempotent and safe to call concurrently with start.
func (r *backgroundRegistry) close() {
	r.mu.Lock()
	alreadyClosed := r.closed
	r.closed = true
	procs := make([]*backgroundProcess, 0, len(r.procs))
	for _, id := range r.order {
		if proc, ok := r.procs[id]; ok {
			procs = append(procs, proc)
		}
	}
	r.mu.Unlock()
	if !alreadyClosed {
		r.cancel()
	}
	r.wg.Wait()
	// The reaper above has waited each group leader, so it can no longer fork.
	// Repeat the uncatchable group kill briefly so a descendant that was
	// concurrently forking during cancellation cannot escape a one-shot signal.
	for _, proc := range procs {
		cleanupCommandProcessGroup(proc.cmd)
	}
}

// backgroundShells returns the Agent's process registry, creating it on first
// use so an Agent that never backgrounds anything starts no goroutines.
func (a *Agent) backgroundShells() *backgroundRegistry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.background == nil {
		a.background = newBackgroundRegistry()
	}
	return a.background
}

// closeBackgroundShells terminates every background process group started by
// this Agent. Called from Agent.Close so a backgrounded dev server cannot
// outlive the session that started it.
func (a *Agent) closeBackgroundShells() {
	a.mu.Lock()
	registry := a.background
	a.mu.Unlock()
	if registry == nil {
		return
	}
	registry.close()
}

// startBackgroundShellCommand launches command detached from the current turn
// and returns the host receipt the model reads. Authorization already happened
// on the ordinary `bash` path before this is reached.
func (a *Agent) startBackgroundShellCommand(command string) (string, bool) {
	proc, err := a.backgroundShells().start(command, a.activeWorkDir(), sanitizedEnv())
	if err != nil {
		// Start failure means the shell never executed: this is an answered
		// failure, not an unknown outcome.
		return fmt.Sprintf("error: background command was not started: %v", err), true
	}
	return fmt.Sprintf(
		"Started background process %s (pid %d).\ncommand: %s\n"+
			"It keeps running after this tool call returns. Read new output with bash_output and id %q.\n"+
			"At most %d byte(s) per stream are retained; the process is terminated when this session ends.",
		proc.id, proc.pid, singleLineCommand(command), proc.id, maxBackgroundStreamBytes), false
}

// handleBashOutput reads a background process back. It is read-only: it never
// starts, signals, or otherwise touches a process.
func (a *Agent) handleBashOutput(args map[string]any) (string, bool) {
	registry := a.backgroundShells()
	id := strings.TrimSpace(a.getArgString(args, "id", ""))
	now := time.Now()
	if id == "" {
		return registry.list(now), false
	}
	proc, ok := registry.lookup(id)
	if !ok {
		registry.mu.Lock()
		known := registry.idsLocked()
		registry.mu.Unlock()
		if len(known) == 0 {
			return fmt.Sprintf("error: no background process %q exists in this session; none have been started", id), true
		}
		return fmt.Sprintf("error: no background process %q exists in this session; known ids: %s", id, strings.Join(known, ", ")), true
	}
	return proc.render(now), false
}
