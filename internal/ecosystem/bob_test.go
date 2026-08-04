package ecosystem

import (
	"strings"
	"testing"
)

// The fixtures below are captured verbatim (previews and hashes trimmed) from
// bob v0.3.0 `--json` output.

const bobApplyRefusedJSON = `{
  "schema_version": 1,
  "ok": false,
  "command": "apply",
  "data": {
    "conflicts": [
      {
        "code": "unmanaged_differs",
        "path": "README.md",
        "reason": "unmanaged file differs from the desired content"
      }
    ],
    "error": {
      "code": "conflicts",
      "message": "apply: plan contains conflicts; run bob plan for details"
    }
  },
  "warnings": ["1 conflict(s) block apply"],
  "next_actions": [
    "run: bob plan --json and inspect actions with kind=conflict",
    "resolve each conflict, then rerun bob apply"
  ]
}`

const bobPlanConflictJSON = `{
  "schema_version": 1,
  "ok": true,
  "command": "plan",
  "data": {
    "schema_version": 1,
    "recipe": {"id": "files", "version": 1},
    "actions": [
      {
        "path": "README.md",
        "kind": "conflict",
        "code": "unmanaged_differs",
        "reason": "unmanaged file differs from the desired content"
      },
      {
        "path": "scripts/run.sh",
        "kind": "create",
        "code": "missing",
        "reason": "destination does not exist"
      }
    ],
    "conflict_count": 1,
    "lock_changed": true
  },
  "warnings": ["1 conflict(s) block apply"],
  "next_actions": [
    "resolve unmanaged or modified-file conflicts",
    "rerun bob plan"
  ]
}`

const bobCheckDriftJSON = `{
  "schema_version": 1,
  "ok": false,
  "command": "check",
  "data": {
    "clean": false,
    "plan": {
      "schema_version": 1,
      "recipe": {"id": "files", "version": 1},
      "actions": [
        {
          "path": "a.txt",
          "kind": "update",
          "code": "content_update",
          "reason": "managed file needs the new desired content"
        }
      ],
      "conflict_count": 0,
      "lock_changed": true
    }
  },
  "warnings": [],
  "next_actions": ["run: bob apply"]
}`

const bobManifestInvalidJSON = `{
  "schema_version": 1,
  "ok": false,
  "command": "plan",
  "data": {
    "error": {
      "code": "manifest_invalid",
      "message": "plan: decode manifest: yaml: unmarshal errors:\n  line 2: field name not found in type manifest.Manifest"
    }
  },
  "warnings": [],
  "next_actions": [
    "fix the problems listed in the message",
    "run: bob recipe show <recipe-id> for the schema"
  ]
}`

const bobPlanCleanJSON = `{
  "schema_version": 1,
  "ok": true,
  "command": "plan",
  "data": {
    "schema_version": 1,
    "recipe": {"id": "files", "version": 1},
    "actions": [
      {
        "path": "a.txt",
        "kind": "unchanged",
        "code": "in_sync",
        "reason": "managed file already matches the desired content and mode"
      }
    ],
    "conflict_count": 0,
    "lock_changed": false
  },
  "warnings": [],
  "next_actions": []
}`

func TestParseBobEnvelopeRejectsNonEnvelopes(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"plain text":      "total 12\ndrwxr-xr-x",
		"json array":      `[{"ok": true}]`,
		"foreign json":    `{"status": "done", "items": []}`,
		"missing command": `{"schema_version": 1, "ok": true, "data": {"a": 1}}`,
		"missing data":    `{"schema_version": 1, "ok": true, "command": "plan"}`,
		"zero schema":     `{"schema_version": 0, "ok": true, "command": "plan", "data": {}}`,
		"truncated json":  `{"schema_version": 1, "ok": fal`,
	}
	for name, text := range cases {
		if _, ok := ParseBobEnvelope(text); ok {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestParseBobEnvelopeApplyRefused(t *testing.T) {
	env, ok := ParseBobEnvelope(bobApplyRefusedJSON)
	if !ok {
		t.Fatal("expected envelope")
	}
	if env.OK {
		t.Error("expected ok=false")
	}
	errInfo, hasError := env.ErrorInfo()
	if !hasError || errInfo.Code != "conflicts" {
		t.Fatalf("expected conflicts error code, got %+v (present=%v)", errInfo, hasError)
	}
	conflicts := env.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Code != "unmanaged_differs" || conflicts[0].Path != "README.md" {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if len(env.NextActions) != 2 {
		t.Fatalf("expected 2 next actions, got %d", len(env.NextActions))
	}
}

func TestConflictsDerivedFromPlanActions(t *testing.T) {
	env, ok := ParseBobEnvelope(bobPlanConflictJSON)
	if !ok {
		t.Fatal("expected envelope")
	}
	conflicts := env.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Code != "unmanaged_differs" {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if actions := env.Actions(); len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}
}

func TestConflictsFromEmbeddedCheckPlan(t *testing.T) {
	env, ok := ParseBobEnvelope(bobCheckDriftJSON)
	if !ok {
		t.Fatal("expected envelope")
	}
	if conflicts := env.Conflicts(); len(conflicts) != 0 {
		t.Fatalf("drift-only check should have no conflicts, got %+v", conflicts)
	}
	if actions := env.Actions(); len(actions) != 1 || actions[0].Code != "content_update" {
		t.Fatalf("expected embedded plan actions, got %+v", env.Actions())
	}
	clean, present := env.CleanFlag()
	if !present || clean {
		t.Fatalf("expected clean=false present=true, got clean=%v present=%v", clean, present)
	}
}

func TestActionIsConflictPrefersCode(t *testing.T) {
	// A future code unknown to this package must not be treated as a
	// conflict just because kind says so; code wins when present.
	action := BobAction{Kind: "conflict", Code: "content_update"}
	if action.IsConflict() {
		t.Error("code should win over kind")
	}
	// Older binaries without codes fall back to kind.
	legacy := BobAction{Kind: "conflict"}
	if !legacy.IsConflict() {
		t.Error("expected kind fallback for legacy action")
	}
	for _, code := range []string{
		"unmanaged_differs", "managed_hash_mismatch", "managed_missing",
		"unmanaged_mode_differs", "retired_owned", "symlink", "special_file",
	} {
		if !IsBobConflictCode(code) {
			t.Errorf("expected %s to be a conflict code", code)
		}
	}
	for _, code := range []string{"missing", "content_update", "mode_drift", "in_sync", "identical_content", ""} {
		if IsBobConflictCode(code) {
			t.Errorf("expected %s to be apply-safe", code)
		}
	}
}

func TestDigestApplyRefused(t *testing.T) {
	env, _ := ParseBobEnvelope(bobApplyRefusedJSON)
	digest := env.Digest()
	for _, want := range []string{
		"bob apply: conflicts",
		"unmanaged_differs: README.md",
		"next: run: bob plan --json and inspect actions with kind=conflict",
	} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest missing %q:\n%s", want, digest)
		}
	}
}

func TestDigestPlanConflicts(t *testing.T) {
	env, _ := ParseBobEnvelope(bobPlanConflictJSON)
	digest := env.Digest()
	if !strings.Contains(digest, "bob plan: 1 conflict(s) block apply") {
		t.Errorf("unexpected digest:\n%s", digest)
	}
	if strings.Contains(digest, "missing: scripts/run.sh") {
		t.Errorf("apply-safe actions must not be listed as conflicts:\n%s", digest)
	}
}

func TestDigestDriftOnly(t *testing.T) {
	env, _ := ParseBobEnvelope(bobCheckDriftJSON)
	digest := env.Digest()
	if !strings.Contains(digest, "drift without conflicts") {
		t.Errorf("unexpected digest:\n%s", digest)
	}
	if !strings.Contains(digest, "next: run: bob apply") {
		t.Errorf("digest missing next action:\n%s", digest)
	}
}

func TestDigestFailureEnvelope(t *testing.T) {
	env, _ := ParseBobEnvelope(bobManifestInvalidJSON)
	digest := env.Digest()
	if !strings.Contains(digest, "bob plan: manifest_invalid") {
		t.Errorf("unexpected digest:\n%s", digest)
	}
	// The message is multi-line; only its first line belongs in the head.
	if strings.Contains(digest, "line 2: field name") {
		t.Errorf("digest should keep only the first message line:\n%s", digest)
	}
}

func TestDigestQuietOnCleanSuccess(t *testing.T) {
	env, _ := ParseBobEnvelope(bobPlanCleanJSON)
	if digest := env.Digest(); digest != "" {
		t.Errorf("expected empty digest for clean plan, got:\n%s", digest)
	}
}

func TestDigestBoundsConflictAndNextActionLists(t *testing.T) {
	env := BobEnvelope{
		SchemaVersion: 1,
		Command:       "apply",
		Data: []byte(`{"conflicts": [
			{"code": "symlink", "path": "a"},
			{"code": "symlink", "path": "b"},
			{"code": "symlink", "path": "c"},
			{"code": "symlink", "path": "d"},
			{"code": "symlink", "path": "e"},
			{"code": "symlink", "path": "f"}
		], "error": {"code": "conflicts", "message": "refused"}}`),
		NextActions: []string{"one", "two", "three", "four", "five"},
	}
	digest := env.Digest()
	if !strings.Contains(digest, "... and 2 more conflict(s)") {
		t.Errorf("expected conflict overflow marker:\n%s", digest)
	}
	if strings.Contains(digest, "next: four") {
		t.Errorf("expected next actions bounded to %d:\n%s", maxDigestNextActions, digest)
	}
}
