// Package ecosystem interprets machine output from companion local-first
// tools. It owns no execution, scheduling, or approval authority: callers
// obtain tool output through their own boundaries (MCP results, reviewed
// shell commands) and use this package only to read it.
package ecosystem

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Bob process exit codes. This contract is stable from Bob v0.3.0 and is the
// supported way to branch on a `bob` invocation: 2 means conflicts that need
// remediation, 3 means drift that apply can safely converge, and 4 means the
// input itself must be fixed before retrying. `bob plan` always exits 0
// because plan is a read-only report.
const (
	BobExitSuccess       = 0
	BobExitInternalError = 1
	BobExitConflicts     = 2
	BobExitDrift         = 3
	BobExitInvalidInput  = 4
)

// BobEnvelope mirrors the stable envelope every `bob --json` command emits.
type BobEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	OK            bool            `json:"ok"`
	Command       string          `json:"command"`
	Data          json.RawMessage `json:"data"`
	Warnings      []string        `json:"warnings"`
	NextActions   []string        `json:"next_actions"`
}

// BobError is the classified failure carried by `data.error` on every
// `ok:false` envelope. Code is one of Bob's stable error codes
// (missing_manifest, manifest_invalid, conflicts, input_invalid,
// workspace_invalid, command_failed); branch on it, never on Message text.
type BobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BobConflict is one entry of `data.conflicts` on a refused apply.
type BobConflict struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// BobAction is one plan or check action. Code is the stable machine field;
// Kind and Reason remain populated by older binaries that predate codes.
type BobAction struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// bobData is the union of the data fields this package reads. Absent fields
// stay zero, which keeps parsing tolerant of both older and newer binaries.
type bobData struct {
	Error         *BobError     `json:"error"`
	Conflicts     []BobConflict `json:"conflicts"`
	Actions       []BobAction   `json:"actions"`
	Clean         *bool         `json:"clean"`
	LockChanged   *bool         `json:"lock_changed"`
	ConflictCount *int          `json:"conflict_count"`
	Plan          *struct {
		Actions       []BobAction `json:"actions"`
		LockChanged   *bool       `json:"lock_changed"`
		ConflictCount *int        `json:"conflict_count"`
	} `json:"plan"`
}

// bobConflictCodes is Bob's stable conflict taxonomy. Every other action code
// (missing, content_update, mode_drift, in_sync, identical_content) is
// apply-safe.
var bobConflictCodes = map[string]bool{
	"unmanaged_differs":      true,
	"managed_hash_mismatch":  true,
	"managed_missing":        true,
	"unmanaged_mode_differs": true,
	"retired_owned":          true,
	"symlink":                true,
	"special_file":           true,
}

// IsBobConflictCode reports whether a plan action code blocks apply.
func IsBobConflictCode(code string) bool {
	return bobConflictCodes[code]
}

// IsConflict prefers the stable code when present and falls back to the
// legacy kind field for binaries that predate action codes.
func (a BobAction) IsConflict() bool {
	if a.Code != "" {
		return IsBobConflictCode(a.Code)
	}
	return a.Kind == "conflict"
}

// ParseBobEnvelope parses text as a Bob JSON envelope. The second return is
// false when the text is not one, so callers can pass arbitrary tool output.
func ParseBobEnvelope(text string) (BobEnvelope, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") {
		return BobEnvelope{}, false
	}
	var env BobEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return BobEnvelope{}, false
	}
	if env.SchemaVersion != 1 || env.Command == "" || len(env.Data) == 0 {
		return BobEnvelope{}, false
	}
	return env, true
}

func (e BobEnvelope) data() bobData {
	var d bobData
	// Tolerate partial decode failures: any fields decoded before an error
	// are still usable, and absent fields simply stay zero.
	_ = json.Unmarshal(e.Data, &d)
	return d
}

// ErrorInfo returns the classified failure from data.error, if present.
func (e BobEnvelope) ErrorInfo() (BobError, bool) {
	d := e.data()
	if d.Error == nil || d.Error.Code == "" {
		return BobError{}, false
	}
	return *d.Error, true
}

// Actions returns the plan actions from data.actions (plan) or
// data.plan.actions (check), whichever the command populated.
func (e BobEnvelope) Actions() []BobAction {
	d := e.data()
	if len(d.Actions) > 0 {
		return d.Actions
	}
	if d.Plan != nil {
		return d.Plan.Actions
	}
	return nil
}

// Conflicts returns the blocking conflicts: data.conflicts when the envelope
// carries them directly (a refused apply), otherwise the conflict actions of
// the embedded plan.
func (e BobEnvelope) Conflicts() []BobConflict {
	d := e.data()
	if len(d.Conflicts) > 0 {
		return d.Conflicts
	}
	var conflicts []BobConflict
	for _, action := range e.Actions() {
		if action.IsConflict() {
			conflicts = append(conflicts, BobConflict{
				Path:   action.Path,
				Code:   action.Code,
				Reason: action.Reason,
			})
		}
	}
	return conflicts
}

// CleanFlag returns data.clean when the command reports it (check).
func (e BobEnvelope) CleanFlag() (clean, present bool) {
	d := e.data()
	if d.Clean == nil {
		return false, false
	}
	return *d.Clean, true
}

// LockChangedFlag returns the plan's lockfile drift marker when present.
func (e BobEnvelope) LockChangedFlag() (changed, present bool) {
	d := e.data()
	if d.LockChanged != nil {
		return *d.LockChanged, true
	}
	if d.Plan != nil && d.Plan.LockChanged != nil {
		return *d.Plan.LockChanged, true
	}
	return false, false
}

// ConflictCount returns the plan's explicit conflict count when present.
func (e BobEnvelope) ConflictCount() (count int, present bool) {
	d := e.data()
	if d.ConflictCount != nil {
		return *d.ConflictCount, true
	}
	if d.Plan != nil && d.Plan.ConflictCount != nil {
		return *d.Plan.ConflictCount, true
	}
	return 0, false
}

const (
	maxDigestConflicts   = 4
	maxDigestNextActions = 3
)

// Digest renders a compact, terminal-friendly summary of what needs
// attention: the stable error code, each blocking conflict with its code, a
// drift note, and Bob's copy-pasteable corrective next actions. It returns
// an empty string when the envelope reports nothing actionable, so clean
// successes stay quiet.
func (e BobEnvelope) Digest() string {
	conflicts := e.Conflicts()
	errInfo, hasError := e.ErrorInfo()

	var lines []string
	switch {
	case hasError:
		head := "bob " + e.Command + ": " + errInfo.Code
		if msg := firstLine(errInfo.Message); msg != "" {
			head += " - " + msg
		}
		lines = append(lines, head)
	case len(conflicts) > 0:
		lines = append(lines, fmt.Sprintf("bob %s: %d conflict(s) block apply", e.Command, len(conflicts)))
	case !e.OK:
		if clean, present := e.CleanFlag(); present && !clean {
			lines = append(lines, "bob "+e.Command+": drift without conflicts; bob apply converges it")
		} else {
			lines = append(lines, "bob "+e.Command+": failed")
		}
	default:
		return ""
	}

	for i, conflict := range conflicts {
		if i == maxDigestConflicts {
			lines = append(lines, fmt.Sprintf("  ... and %d more conflict(s)", len(conflicts)-maxDigestConflicts))
			break
		}
		code := conflict.Code
		if code == "" {
			code = "conflict"
		}
		lines = append(lines, "  "+code+": "+conflict.Path)
	}
	for i, action := range e.NextActions {
		if i == maxDigestNextActions {
			break
		}
		lines = append(lines, "  next: "+action)
	}
	return strings.Join(lines, "\n")
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}
