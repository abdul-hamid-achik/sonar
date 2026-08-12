package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/abdul-hamid-achik/sonar/internal/agent"
)

const reducedMotionActivityHeartbeatInterval = time.Second

// activityHeartbeatMsg is the receipt for the single low-frequency repaint
// chain used when reduced motion disables the spinner and scramble clocks.
// Tokening makes a late receipt from an older chain harmless.
type activityHeartbeatMsg struct {
	Token uint64
}

type workingActivity struct {
	label        string
	compactLabel string
	detail       string
	elapsed      time.Duration
	cancellable  bool
	waiting      bool
	static       bool
}

func reducedMotionRequested() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("SONAR_REDUCED_MOTION")))
	return value != "" && value != "0" && value != "false" && value != "off"
}

func (m *Model) nowTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Model) turnElapsed() time.Duration {
	if m.turnStartedAt.IsZero() {
		return 0
	}
	elapsed := m.nowTime().Sub(m.turnStartedAt)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func formatWorkingElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed < 10*time.Second {
		return fmt.Sprintf("%.1fs", elapsed.Seconds())
	}
	if elapsed < time.Minute {
		return fmt.Sprintf("%.0fs", elapsed.Seconds())
	}
	minutes := int(elapsed / time.Minute)
	seconds := int(elapsed/time.Second) % 60
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

func (m *Model) currentWorkingActivity() (workingActivity, bool) {
	// Interactive prompts own the footer while they are open. Reporting work
	// behind an approval would imply progress even though the operation is
	// deliberately paused on the user's decision.
	if m.pendingApproval != nil || m.pendingPaste != nil {
		return workingActivity{}, false
	}

	switch {
	// An open microphone outranks everything. It is the one state where the
	// harness is capturing the room rather than reporting on itself, and a
	// person must be able to notice it without going looking — so it takes the
	// rail even from a running turn, whose progress is one line below anyway.
	case m.listeningForVoice():
		return workingActivity{
			// The detail is built rather than written: it names the binding this
			// build actually has, and it changes to say so when the microphone
			// has been open long enough to be sure it is hearing nothing. A hint
			// naming a key is useless to someone whose real problem is a muted
			// input.
			label: "Listening", compactLabel: "Listening",
			detail: m.voiceListeningDetail(), cancellable: false,
		}, true
	case m.transcribingVoice():
		return workingActivity{
			label: "Transcribing", compactLabel: "Transcribing", static: true,
		}, true
	case m.shuttingDown:
		return workingActivity{label: "Stopping safely", detail: "waiting for receipts"}, true
	case m.initializing && (!m.turnReady || m.state == StateIdle):
		settled := 0
		for _, item := range m.startupItems {
			if item.Status == "connected" || item.Status == "failed" {
				settled++
			}
		}
		// Name the provider being contacted. "local runtime" described the
		// Ollama-only past and is simply false for a hosted model.
		detail := sanitizeTerminalSingleLine(m.activeProviderName())
		if detail == "" {
			detail = "provider"
		}
		if len(m.startupItems) > 0 {
			detail += fmt.Sprintf(" · %d/%d", settled, len(m.startupItems))
		}
		return workingActivity{label: "Starting", detail: detail}, true
	case m.providerSwitchRunning:
		detail := sanitizeTerminalSingleLine(m.providerSwitchName)
		if detail == "" {
			detail = "runtime profile"
		}
		return workingActivity{label: "Switching provider", detail: detail, cancellable: true}, true
	case m.sessionListing:
		return workingActivity{label: "Loading sessions", cancellable: true}, true
	case m.sessionLoading:
		return workingActivity{label: "Restoring session", cancellable: true}, true
	case m.fileLoading:
		return workingActivity{label: "Reading local file", cancellable: true}, true
	case m.imageAttachRunning:
		return workingActivity{label: "Attaching image", detail: "validating private copy", cancellable: true}, true
	case m.readScopeOpRunning:
		label := m.readScopeOpLabel
		if label == "" {
			label = "Updating read-only scope"
		}
		return workingActivity{label: label, detail: "writes remain workspace-only"}, true
	case m.commitRunning:
		return workingActivity{label: "Generating commit", cancellable: true}, true
	case m.exportRunning:
		return workingActivity{label: "Publishing export"}, true
	case m.goalOperation != "":
		return workingActivity{label: m.goalOperation, detail: "Cortex", cancellable: true}, true
	case m.compactingContext:
		return workingActivity{label: "Preparing context", detail: "summarizing earlier turns", cancellable: true}, true
	case m.toolsPending > 0:
		elapsed := m.runningToolElapsed()
		// Running ToolCards are stable transcript receipts. Keep animation,
		// elapsed time, and the global cancellation affordance in this one
		// footer-owned activity row.
		activity := workingActivity{label: "Tool running", elapsed: elapsed, cancellable: true}
		if m.toolsPending > 1 {
			activity.label = fmt.Sprintf("%d tools running", m.toolsPending)
		}
		return activity, true
	case m.autoCheckpoints.SegmentsContinued > 0 && (m.state == StateWaiting || m.state == StateStreaming):
		return workingActivity{
			label:        "Continuing automatically",
			compactLabel: "AUTO continuing",
			detail:       m.autoContinuationBudgetDetail(),
			cancellable:  true,
		}, true
	case m.capabilityRoute != nil && (m.state == StateWaiting || m.state == StateStreaming):
		route := *m.capabilityRoute
		if route.Status != agent.CapabilityRouteResolved {
			// A resolver miss or failure is advisory metadata, not the state of
			// the provider turn. Keep the active execution as the primary label
			// and expose the typed route state as progressive detail. Runtime
			// retains the complete advisory after the turn settles.
			return workingActivity{
				label: "Running", detail: capabilityRouteLabel(route),
				elapsed: m.turnElapsed(), cancellable: true,
			}, true
		}
		return workingActivity{
			label: capabilityRouteLabel(route), compactLabel: capabilityRouteCompactLabel(route),
			detail:  capabilityRouteDetail(route),
			elapsed: m.turnElapsed(), cancellable: true,
		}, true
	case m.state == StateWaiting:
		return workingActivity{
			label: "Running", elapsed: m.turnElapsed(), cancellable: true, waiting: true,
		}, true
	case m.state == StateStreaming:
		return workingActivity{
			label: "Running", elapsed: m.turnElapsed(), cancellable: true,
		}, true
	case m.ollamaInventoryCommitting:
		return workingActivity{label: "Updating Ollama inventory", detail: "verifying model authority"}, true
	case m.standaloneRecovery != nil && m.standaloneRecovery.loading:
		// Inspection is read-only and normally completes quickly. A static status
		// both locks the composer and avoids introducing another animation clock
		// for a one-shot durable receipt lookup.
		return workingActivity{label: "Inspecting recovery", detail: "read-only durable receipt", static: true}, true
	// Reading the answer out loud ranks last on purpose, which is the opposite
	// of the open microphone above. Speech overlaps a running turn for almost
	// all of its duration, so anything higher would replace real progress with
	// a state the listener can already hear. What it does cover is the gap the
	// rail had nothing for: the turn is finished, the text is on screen, and the
	// harness is still talking — where an idle composer otherwise reads as done.
	case m.speakingAloud():
		return workingActivity{
			label: "Speaking", compactLabel: "Speaking",
			detail: "typing stops it", cancellable: false,
		}, true
	default:
		return workingActivity{}, false
	}
}

// hasRunningToolReceipt reports whether the transcript is painting a card that
// would advance with the activity clock. It shares runningToolElapsed's window
// — this turn's receipts — because an interrupted receipt from an earlier turn
// is reconciled to cancelled rather than left animating forever.
//
// Reduced motion answers false: the static cue never advances, so a repaint
// would produce a byte-identical frame at the spinner's cadence.
func (m *Model) hasRunningToolReceipt() bool {
	if m == nil || m.reducedMotion {
		return false
	}
	start := min(max(0, m.turnToolStartIndex), len(m.toolEntries))
	for index := start; index < len(m.toolEntries); index++ {
		if m.toolEntries[index].Status == ToolStatusRunning {
			return true
		}
	}
	return false
}

func (m *Model) runningToolElapsed() time.Duration {
	var startedAt time.Time
	start := min(max(0, m.turnToolStartIndex), len(m.toolEntries))
	for index := start; index < len(m.toolEntries); index++ {
		entry := m.toolEntries[index]
		if entry.Status != ToolStatusRunning || entry.StartTime.IsZero() {
			continue
		}
		if startedAt.IsZero() || entry.StartTime.Before(startedAt) {
			startedAt = entry.StartTime
		}
	}
	if startedAt.IsZero() {
		return 0
	}
	elapsed := m.nowTime().Sub(startedAt)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func (m *Model) composerIsBusy() bool {
	_, busy := m.currentWorkingActivity()
	return busy
}

// needsSpinner reports whether the parent model currently owns an animated
// Bubbles spinner. Waiting uses the separate scramble animation so only one
// motion clock owns each phase.
func (m *Model) needsSpinner() bool {
	if m.reducedMotion {
		return false
	}
	activity, active := m.currentWorkingActivity()
	return active && (!activity.waiting || m.glyphProfile == GlyphASCII) && !activity.static
}

func (m *Model) needsScramble() bool {
	if m.reducedMotion || m.glyphProfile == GlyphASCII {
		return false
	}
	activity, active := m.currentWorkingActivity()
	return active && activity.waiting
}

func (m *Model) startActivityCmd() tea.Cmd {
	if m.reducedMotion {
		return m.startActivityHeartbeatCmd()
	}
	if m.needsScramble() {
		return m.scramble.Tick()
	}
	return m.startSpinnerCmd()
}

func (m *Model) needsActivityHeartbeat() bool {
	if m == nil || !m.reducedMotion {
		return false
	}
	_, active := m.currentWorkingActivity()
	return active
}

func (m *Model) startActivityHeartbeatCmd() tea.Cmd {
	if !m.needsActivityHeartbeat() || m.activityHeartbeatPending {
		return nil
	}
	m.activityHeartbeatToken++
	token := m.activityHeartbeatToken
	m.activityHeartbeatPending = true
	return tea.Tick(reducedMotionActivityHeartbeatInterval, func(time.Time) tea.Msg {
		return activityHeartbeatMsg{Token: token}
	})
}

// handleActivityHeartbeat repaints only footer information that changes with
// time. The static reduced-motion glyph never advances, transcript receipts
// remain event-driven, and the next receipt is scheduled only while an
// activity still owns the footer.
func (m *Model) handleActivityHeartbeat(msg activityHeartbeatMsg) tea.Cmd {
	if !m.activityHeartbeatPending || msg.Token != m.activityHeartbeatToken {
		return nil
	}
	m.activityHeartbeatPending = false
	if !m.needsActivityHeartbeat() {
		return nil
	}
	return m.startActivityHeartbeatCmd()
}

func (m *Model) startSpinnerCmd() tea.Cmd {
	if !m.needsSpinner() {
		return nil
	}
	return m.spin.Tick
}

func (m *Model) renderWorkingLine() string {
	activity, ok := m.currentWorkingActivity()
	if !ok {
		return ""
	}
	activity.label = sanitizeTerminalSingleLine(activity.label)
	activity.compactLabel = sanitizeTerminalSingleLine(activity.compactLabel)
	activity.detail = sanitizeTerminalSingleLine(activity.detail)
	if activity.label == "" {
		activity.label = "Working"
	}

	// Motion sits after content-grid Prefix (OriginX pad). Do not use StatusDot's
	// PaddingLeft here — that pad was for the old density-dependent left spacer
	// and would double-indent, stealing cells needed for esc/queue at 30 cols.
	motionStyle := m.styles.StatusDot.UnsetPaddingLeft()
	// A single-cell ellipsis communicates unfinished work even when animation is
	// disabled. Unlike a filled dot it cannot be mistaken for a settled status
	// marker, and it keeps reduced-motion and static operations width-stable.
	motion := motionStyle.Render(glyphEllipsis(m.glyphProfile))
	if m.glyphProfile == GlyphASCII {
		motion = motionStyle.Render(glyphSet(GlyphASCII).Running)
	}
	if !m.reducedMotion && !activity.static {
		if activity.waiting {
			if m.glyphProfile == GlyphASCII {
				motion = m.spin.View()
			} else {
				cells := 1
				if m.chatPaneWidth() >= 58 {
					cells = 6
				}
				// Once a first-echo baseline exists, the waiting cells encode
				// elapsed-vs-typical (sonar_ping.go) instead of random
				// shimmer. The first wait of a model keeps the scramble: with
				// no baseline the trace would have nothing honest to say.
				motion = m.sonarWaitTrace(cells)
				if motion == "" {
					motion = m.scramble.ViewN(cells)
				}
				if motion == "" {
					motion = motionStyle.Render(glyphEllipsis(m.glyphProfile))
				}
			}
		} else if meter := m.voiceMeter(m.voiceMeterCells()); meter != "" {
			// An open microphone spends the motion cells on a level meter rather
			// than on a pulse. The pulse would animate identically for a muted
			// input, a wrong device and a permission never granted — which is
			// precisely the state somebody sits in talking to a harness that is
			// recording silence. This moves only when the microphone hears
			// something, so a flat meter is information rather than a stall.
			motion = meter
		} else if pulse := m.sonarPulseGlyph(); pulse != "" {
			// Every active phase that is not a wait: a tool running, a stream
			// arriving, a session restoring. The waiting phase keeps the trace
			// above, because there a fact exists to show and identity is the
			// weaker thing to spend the cells on.
			motion = pulse
		} else {
			motion = m.spin.View()
		}
	}

	longCancel := ""
	shortCancel := ""
	if activity.cancellable {
		// Grok-style: stop is a first-class control next to the live timer.
		longCancel = " · esc stop"
		shortCancel = " · esc"
	}

	elapsed := ""
	// Sub-second timers flicker between otherwise identical frames and add no
	// useful progress signal. Start showing elapsed time after one full second;
	// longer operations still keep the compact live timer.
	if activity.elapsed >= time.Second {
		elapsed = " · " + formatWorkingElapsed(activity.elapsed)
	}
	// Zero-style token delta while a turn is live (output tokens this turn).
	tokens := ""
	if m.evalCount > 0 && (m.state == StateWaiting || m.state == StateStreaming) {
		tokens = " · ↑ " + formatTokens(m.evalCount)
	}
	detail := ""
	if activity.detail != "" {
		detail = " · " + activity.detail
	}
	if len(m.pendingImages) > 0 && m.queuedFollowUp == nil {
		detail = fmt.Sprintf(" · + %d image%s", len(m.pendingImages), pluralSuffix(len(m.pendingImages))) + detail
	}
	authority := ""
	// The rail owns authority only when this frame assigned it here; otherwise
	// the shortcuts row is already showing it one line below.
	plan := m.planStatus()
	ownsMode := plan.owns(factMode, surfaceActivityRail)
	switch m.presentedMode() {
	case ModeAuto:
		// The ordinary idle footer is replaced while work is active. Keep AUTO's
		// authority visible in the activity rail so a long-running turn never
		// leaves the user guessing whether it is operating autonomously. A
		// multi-segment continuation also surfaces which segment is running.
		if ownsMode {
			authority = " · AUTO"
		}
		// The continuation counter is progress, not authority: it survives even
		// when the badge itself belongs to another surface.
		if continued := m.autoCheckpoints.SegmentsContinued; continued > 0 {
			authority = fmt.Sprintf("%s · seg %d", authority, continued+1)
		}
	case ModePlan:
		// PLAN is also an authority boundary: it may inspect and reason, but must
		// not be mistaken for an ordinary implementation turn while the idle
		// status row is replaced by live activity.
		if ownsMode {
			authority = " · PLAN"
		}
	}
	queueAction := ""
	if m.queuedFollowUp == nil && m.goalTurnID == "" && m.goalOperation == "" &&
		(m.state == StateWaiting || m.state == StateStreaming) {
		queueAction = " · enter queue"
	}

	candidates := make([]string, 0, 24)
	if authority != "" {
		candidates = append(candidates,
			activity.label+authority+detail+elapsed+tokens+longCancel+queueAction,
			activity.label+authority+elapsed+tokens+longCancel+queueAction,
			activity.label+authority+elapsed+longCancel+queueAction,
			activity.label+authority+longCancel+queueAction,
		)
		if queueAction != "" {
			candidates = append(candidates,
				activity.label+authority+elapsed+tokens+shortCancel+" · queue",
				activity.label+authority+elapsed+shortCancel+" · queue",
				activity.label+authority+shortCancel+" · queue",
			)
			if activity.compactLabel != "" {
				candidates = append(candidates,
					activity.compactLabel+authority+elapsed+shortCancel+" · queue",
					activity.compactLabel+authority+shortCancel+" · queue",
				)
			}
			candidates = append(candidates,
				"Run"+authority+shortCancel+" · queue",
				// At the 30-column tier a visible session handle and the queue
				// affordance cannot both fit with the execution authority. Preserve
				// AUTO/PLAN and cancellation first; the focused composer still
				// advertises Enter as the queue action.
				"Run"+authority+shortCancel,
				"Run"+authority,
			)
		} else {
			if activity.compactLabel != "" {
				candidates = append(candidates,
					activity.compactLabel+authority+elapsed+tokens+shortCancel,
					activity.compactLabel+authority+elapsed+shortCancel,
					activity.compactLabel+authority+shortCancel,
				)
			}
			candidates = append(candidates, "Run"+authority+shortCancel)
		}
	}
	candidates = append(candidates,
		activity.label+detail+elapsed+tokens+longCancel+queueAction,
		activity.label+elapsed+tokens+longCancel+queueAction,
		activity.label+elapsed+longCancel+queueAction,
		activity.label+longCancel+queueAction,
	)
	if queueAction != "" {
		// Preserve the semantic activity label by shortening controls before
		// falling back to the compact identity. This keeps typed routing states
		// inspectable at ordinary widths while still fitting the 30-column tier.
		candidates = append(candidates,
			activity.label+elapsed+shortCancel+" · queue",
			activity.label+shortCancel+" · queue",
		)
		if activity.compactLabel != "" {
			candidates = append(candidates,
				activity.compactLabel+elapsed+shortCancel+" · queue",
				activity.compactLabel+shortCancel+" · queue",
			)
		}
		candidates = append(candidates, "Run"+shortCancel+" · queue")
	}
	if activity.compactLabel != "" {
		candidates = append(candidates,
			activity.compactLabel+elapsed+shortCancel,
			activity.compactLabel+shortCancel,
			activity.compactLabel,
		)
	}
	candidates = append(candidates,
		activity.label+elapsed+longCancel,
		activity.label+longCancel,
		activity.label+elapsed+shortCancel,
		activity.label+shortCancel,
		activity.label,
	)
	if m.followPaused() {
		candidates = []string{
			activity.label + authority + detail + elapsed + longCancel + " · end latest",
			activity.label + authority + elapsed + longCancel + " · end latest",
			activity.label + authority + longCancel + " · end latest",
			activity.label + authority + shortCancel + " · end",
			"Paused" + authority + shortCancel + " · end",
			"Paused" + authority + " · end",
		}
	}

	// Align the activity rail with transcript OriginX via content-grid Prefix.
	// Ordinary motion (spinner / ellipsis) occupies the accent cell so text
	// begins at OriginX=3 with the same budget as transcript content. Bubble Tea
	// spinners often emit a leading pad space (display width 2); trim that pad
	// so the glyph fits the accent cell without Prefix collapsing it to "…",
	// and without stealing the 30-column esc/queue budget. Multi-cell scramble
	// keeps motion after the pad and budgets the remainder.
	grid := m.contentGrid()
	motionWidth := lipgloss.Width(motion)
	accentMotion := motion
	if trimmed := strings.TrimSpace(ansi.Strip(motion)); motionWidth > contentAccentColumns &&
		trimmed != "" && lipgloss.Width(trimmed) <= contentAccentColumns {
		// Preserve styling when the frame is only a pad space plus one glyph.
		// Most Bubbles frames are plain; fall back to the stripped glyph.
		if plain := strings.TrimSpace(motion); lipgloss.Width(plain) <= contentAccentColumns {
			accentMotion = plain
		} else {
			accentMotion = trimmed
		}
		motionWidth = lipgloss.Width(accentMotion)
	}
	var lead string
	var textWidth int
	if wave := m.workingPulseWave(motion); wave != "" {
		// The pulse fills the chrome the grid already reserves — accent cell plus
		// the two pad cells — instead of sitting alone in column 1 with a gap on
		// either side. The text origin is unchanged: this is the same
		// contentLeftColumns the Prefix below would have produced, painted rather
		// than left blank.
		lead = wave
		textWidth = max(1, m.chatPaneWidth()-grid.OriginX())
	} else if motionWidth <= contentAccentColumns {
		lead = grid.Prefix(accentMotion)
		textWidth = max(1, m.chatPaneWidth()-lipgloss.Width(lead))
	} else {
		lead = grid.Prefix(" ")
		textWidth = max(1, m.chatPaneWidth()-lipgloss.Width(lead)-motionWidth-1)
	}
	session := ""
	selectionWidth := textWidth
	// Sticky user strip already owns the prompt; activity keeps only the
	// compact public handle (no title echo like "hey chat!").
	titleLimit := 0
	if m.chatPaneWidth() >= 72 && !m.stickyUserActive() {
		titleLimit = 24
	}
	sessionPublicID := m.sessionPublicID
	sessionTitle := m.activeSessionTitle
	if pending := m.pendingSessionSwitch; m.sessionLoading && pending != nil &&
		pending.Choice != sessionSwitchUndecided && pending.LoadToken == m.sessionLoadToken {
		// Until the tokened receipt commits, m.sessionID still names the source
		// conversation. The activity rail must identify the target being restored,
		// not imply that the source session is being reloaded.
		sessionPublicID = pending.TargetPublicID
		sessionTitle = pending.TargetTitle
		titleLimit = 24
	}
	// While the sticky strip is live, skip session chrome entirely — the prompt
	// is already painted above and the handle is discoverable in Runtime.
	if !m.stickyUserActive() || m.sessionLoading {
		session = sessionDisplayLabel(sessionPublicID, sessionTitle, titleLimit)
	}
	if session != "" {
		selectionWidth = max(1, textWidth-lipgloss.Width(" · ")-lipgloss.Width(session))
	}
	// Degrading is silent today, and silence here is a small lie: at 60 columns
	// the rail reads "Running · AUTO" and at 90 the same turn reads "Running ·
	// AUTO · 2m42s · ↑ 2.2k", with nothing to say the first one dropped
	// anything. A reader cannot tell a turn with no elapsed time from a pane too
	// narrow to show it.
	//
	// The marker is opportunistic: it is added only when it fits AFTER the
	// candidate is chosen, never budgeted before. Budgeting it first cost a
	// whole tier — at the 30-column tier the rail dropped "Hitspec" to make
	// room for a mark saying something had been dropped, which is a worse trade
	// than the silence it was fixing. Where the pane is too narrow for even the
	// marker, the compression is self-evident anyway.
	marker := glyphEllipsis(m.glyphProfile)
	chosen := candidates[len(candidates)-1]
	degraded := len(candidates) > 1
	for index, candidate := range candidates {
		if lipgloss.Width(candidate) <= selectionWidth {
			chosen, degraded = candidate, index > 0
			break
		}
	}
	truncated := lipgloss.Width(chosen) > selectionWidth
	chosen = truncateDisplayWithGlyphProfile(chosen, selectionWidth, m.glyphProfile)
	// Truncation already ends in an ellipsis, which says the same thing; two in
	// a row would only look like a rendering bug.
	if degraded && !truncated && lipgloss.Width(chosen)+lipgloss.Width(marker)+1 <= selectionWidth {
		chosen += " " + marker
	}
	if session != "" {
		chosen += " · " + session
	}
	if motionWidth <= contentAccentColumns {
		return lead + m.renderWorkingCandidate(chosen)
	}
	return lead + motion + " " + m.renderWorkingCandidate(chosen)
}

// renderWorkingCandidate separates live state, authority, metadata, and keys
// without changing the carefully selected responsive text budget above. This
// keeps the footer scannable in both light and dark terminals while NO_COLOR
// still receives the exact same plain-text grammar.
func (m *Model) renderWorkingCandidate(candidate string) string {
	segments := strings.Split(sanitizeTerminalSingleLine(candidate), " · ")
	for index, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		switch {
		case index == 0:
			segments[index] = m.styles.ToolRunningText.Render(segment)
		case segment == "AUTO":
			segments[index] = m.styles.ModeBuild.Render(segment)
		case segment == "PLAN":
			segments[index] = m.styles.ModePlan.Render(segment)
		case workingControlKey(segment) != "":
			keyLabel := workingControlKey(segment)
			action := strings.TrimSpace(strings.TrimPrefix(segment, keyLabel))
			segments[index] = m.styles.FocusIndicator.Render(keyLabel)
			if action != "" {
				segments[index] += " " + m.styles.StreamHint.Render(action)
			}
		default:
			segments[index] = m.styles.StreamHint.Render(segment)
		}
	}
	return strings.Join(segments, m.styles.StreamHint.Render(glyphSeparator(m.glyphProfile)))
}

func workingControlKey(segment string) string {
	keyLabel, _, _ := strings.Cut(strings.TrimSpace(segment), " ")
	switch keyLabel {
	case "esc", "enter", "end":
		return keyLabel
	default:
		return ""
	}
}

func (m *Model) renderContextStatus() string {
	// Zero-style ambient instrumentation: show the context meter whenever the
	// host knows a window, even at 0 used tokens (first frame / empty session).
	// Percent display may be spring-smoothed; warning colors use true occupancy.
	if m.numCtx <= 0 {
		return ""
	}
	return m.formatContextStatusWithPercent(m.displayContextPercent())
}

// autoContinuationBudgetDetail reports how much of the AUTO turn budget is
// spent, in the two dimensions that actually end the run.
//
// The segment counter alone was enough while the whole-turn ceiling was a
// fixed 8 segments and 90 minutes. Now that both are configurable and a run
// can be given hours, "checkpoint 3/40" says almost nothing: the wall clock is
// what bites first on a long job, and the row carried no time at all.
//
// Elapsed here is the logical turn's, not the segment's — the ceiling it is
// measured against is the turn's — so it does not duplicate the per-activity
// elapsed counter, which times the current operation.
func (m *Model) autoContinuationBudgetDetail() string {
	// The nil guard has to precede the first dereference. It sat below the
	// Sprintf that reads m.autoCheckpoints, so a nil receiver would have
	// panicked one line before the check that exists to prevent it.
	if m == nil {
		return ""
	}
	segments := fmt.Sprintf("checkpoint %d/%d",
		m.autoCheckpoints.SegmentsContinued, m.autoCheckpoints.segmentCeiling())
	if m.autoCheckpoints.StartedAt.IsZero() {
		return segments
	}
	ceiling := m.autoCheckpoints.elapsedCeiling()
	if ceiling <= 0 {
		return segments
	}
	spent := m.nowTime().Sub(m.autoCheckpoints.StartedAt)
	if spent < 0 {
		spent = 0
	}
	return segments + " · " + formatBudgetSpan(spent) + "/" + formatBudgetSpan(ceiling)
}

// formatBudgetSpan renders a duration at the coarsest unit that still
// distinguishes it, so a six-hour ceiling reads as "6h" rather than "360m" and
// a two-minute run does not read as "0h".
func formatBudgetSpan(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	hours := d / time.Hour
	minutes := (d % time.Hour) / time.Minute
	if minutes == 0 {
		return fmt.Sprintf("%dh", int(hours))
	}
	return fmt.Sprintf("%dh%02dm", int(hours), int(minutes))
}

// workingPulseWave returns the multi-cell pulse when it is what the rail should
// paint, and "" when the motion cell already holds something better.
//
// Guarded on the motion the caller resolved rather than re-deriving it: the
// waiting phase owns those cells with a trace that carries a fact, and an open
// microphone owns them with a level meter that carries a measurement. Both
// outrank identity, which is all the pulse is.
func (m *Model) workingPulseWave(motion string) string {
	if m == nil || m.reducedMotion || m.glyphProfile == GlyphASCII {
		return ""
	}
	if motion != m.sonarPulseGlyph() {
		return ""
	}
	return m.sonarPulseWave()
}
