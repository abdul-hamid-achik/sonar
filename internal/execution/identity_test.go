package execution

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

// Terminal decides whether an event closes an execution. The durable ledger's
// terminal-event invariants, the snapshot cursor, and the outcome_unknown
// latch all read it, and none of them had a test that would notice it drifting.
//
// Every event type is listed explicitly rather than looped, so adding a new
// one forces a deliberate answer here instead of silently defaulting to
// non-terminal — the direction that leaves an execution open forever.
func TestTerminalClassifiesEveryEventType(t *testing.T) {
	terminal := map[EventType]bool{
		EventRequested:         false,
		EventApprovalRequested: false,
		EventApproved:          false,
		EventStarted:           false,
		EventDenied:            true,
		EventCompleted:         true,
		EventFailed:            true,
		EventCancelled:         true,
		EventOutcomeUnknown:    true,
	}
	for eventType, want := range terminal {
		if got := eventType.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", eventType, got, want)
		}
		if got := eventType.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", eventType, got, want)
		}
	}

	// Every type the package considers valid must appear above. A new event
	// that nobody classified would default to non-terminal and leave its
	// execution open, which is the failure that latches a session.
	for eventType := range terminal {
		if !eventType.Valid() {
			t.Errorf("%s is classified here but not accepted by Valid()", eventType)
		}
	}
	if got := EventType("invented").Terminal(); got {
		t.Error("an unknown event type reported itself terminal")
	}
}

// Durable identities key the execution ledger. A collision or a malformed
// prefix corrupts the record a session is later recovered from.
func TestDurableIdentitiesAreDistinctAndWellFormed(t *testing.T) {
	for _, test := range []struct {
		name   string
		prefix string
		gen    func() (string, error)
	}{
		{"run", "run_", NewRunID},
		{"execution", "exec_", NewExecutionID},
		{"idempotency", "idem_", NewIdempotencyKey},
		{"turn", "turn_", NewTurnID},
	} {
		t.Run(test.name, func(t *testing.T) {
			shape := regexp.MustCompile(`^` + regexp.QuoteMeta(test.prefix) + `[0-9a-f]{32}$`)
			seen := make(map[string]struct{}, 512)
			for i := 0; i < 512; i++ {
				id, err := test.gen()
				if err != nil {
					t.Fatalf("generate: %v", err)
				}
				if !shape.MatchString(id) {
					t.Fatalf("id %q does not match %s + 32 hex characters", id, test.prefix)
				}
				if _, duplicate := seen[id]; duplicate {
					t.Fatalf("generated %q twice in 512 draws", id)
				}
				seen[id] = struct{}{}
			}
		})
	}
}

// The prefixes must stay distinct, or a run ID and an execution ID become
// interchangeable in a durable row and the record stops meaning anything.
func TestIdentityPrefixesDoNotCollide(t *testing.T) {
	ids := map[string]string{}
	for name, gen := range map[string]func() (string, error){
		"run": NewRunID, "execution": NewExecutionID,
		"idempotency": NewIdempotencyKey, "turn": NewTurnID,
	} {
		id, err := gen()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		prefix := id[:strings.Index(id, "_")+1]
		if other, taken := ids[prefix]; taken {
			t.Errorf("%s and %s share the prefix %q", name, other, prefix)
		}
		ids[prefix] = name
	}
}

// HashBytes backs the result digests the projection boundary compares. Its
// contract is plain SHA-256 in lowercase hex, and nothing asserted that.
func TestHashBytesIsStableLowercaseSHA256(t *testing.T) {
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := HashBytes(nil); got != empty {
		t.Errorf("HashBytes(nil) = %s, want the SHA-256 of no bytes", got)
	}
	if got := HashBytes([]byte{}); got != empty {
		t.Errorf("HashBytes(empty) = %s, want the SHA-256 of no bytes", got)
	}

	digest := HashBytes([]byte("receipt"))
	if len(digest) != 64 {
		t.Errorf("digest is %d characters, want 64", len(digest))
	}
	if _, err := hex.DecodeString(digest); err != nil {
		t.Errorf("digest is not hex: %v", err)
	}
	if digest != strings.ToLower(digest) {
		t.Errorf("digest %q is not lowercase; a case difference breaks equality comparison", digest)
	}
	if HashBytes([]byte("receipt")) != digest {
		t.Error("HashBytes is not deterministic")
	}
	if HashBytes([]byte("receipts")) == digest {
		t.Error("HashBytes collided on a one-character difference")
	}

	// The text and byte helpers must agree, or two paths that hash the same
	// content produce receipts the projection boundary reads as different.
	if HashText("receipt") != digest {
		t.Error("HashText and HashBytes disagree on identical content")
	}
}
