package goaladvisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/goal"
)

const (
	minPendingDecisionOptions         = 2
	maxPendingDecisionOptions         = 16
	maxDecisionStableIDBytes          = 256
	maxDecisionQuestionBytes          = 16 << 10
	maxDecisionRequesterBytes         = 256
	maxDecisionOptionLabelBytes       = 4 << 10
	maxDecisionOptionConsequenceBytes = 16 << 10
	maxDecisionTimestampBytes         = 64
)

// DecisionStatus is the bounded lifecycle state retained from Cortex. This
// projection represents requests only, so the parser accepts pending exactly.
type DecisionStatus string

const DecisionStatusPending DecisionStatus = "pending"

// DecisionOption is one bounded choice from a pending Cortex decision.
type DecisionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Consequence string `json:"consequence"`
}

// PendingDecision is the request-only projection of Cortex's domain.Decision.
// Answer, responder, answeredAt, and evidenceId are intentionally absent.
type PendingDecision struct {
	ID          string           `json:"id"`
	Question    string           `json:"question"`
	Options     []DecisionOption `json:"options"`
	Requester   string           `json:"requester"`
	RequestedAt time.Time        `json:"requestedAt"`
	Status      DecisionStatus   `json:"status"`
	Sensitive   bool             `json:"sensitive,omitempty"`
}

// AnswerDecisionRequest is the exact authority-bearing answer Cortex accepts.
// The adapter adds its configured workspace at dispatch time; callers cannot
// override that binding. Accepting an answer may advance Cortex's own semantic
// phase, but it grants no authority over the host Goal Runtime or provider.
type AnswerDecisionRequest struct {
	TaskID     string `json:"taskId"`
	DecisionID string `json:"decisionId"`
	OptionID   string `json:"answer"`
	Responder  string `json:"responder"`
}

const pendingDecisionRequestBindingVersion = 1

type canonicalPendingDecisionRequest struct {
	Version     int              `json:"version"`
	TaskID      string           `json:"task_id"`
	DecisionID  string           `json:"decision_id"`
	Question    string           `json:"question"`
	Options     []DecisionOption `json:"options"`
	Requester   string           `json:"requester"`
	RequestedAt string           `json:"requested_at"`
	Status      DecisionStatus   `json:"status"`
	Sensitive   bool             `json:"sensitive"`
}

// RequestBindingSHA256 returns a deterministic binding for the complete typed
// request without exposing its presentation text to durable control-plane
// payloads. The canonical timestamp is always UTC.
func (d PendingDecision) RequestBindingSHA256(taskID string) (string, error) {
	canonical, err := canonicalPendingDecision(taskID, d)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal canonical pending decision: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalPendingDecision(taskID string, decision PendingDecision) (canonicalPendingDecisionRequest, error) {
	if err := validateExactDecisionText("task id", taskID, goal.MaxCorrelationIDBytes); err != nil {
		return canonicalPendingDecisionRequest{}, err
	}
	if decision.Status != DecisionStatusPending {
		return canonicalPendingDecisionRequest{}, fmt.Errorf("pending decision status must be %q", DecisionStatusPending)
	}
	if decision.RequestedAt.IsZero() {
		return canonicalPendingDecisionRequest{}, errors.New("pending decision requestedAt must be non-zero")
	}
	if len(decision.Options) < minPendingDecisionOptions || len(decision.Options) > maxPendingDecisionOptions {
		return canonicalPendingDecisionRequest{}, fmt.Errorf("pending decision options must contain between %d and %d choices", minPendingDecisionOptions, maxPendingDecisionOptions)
	}
	if err := validateExactDecisionText("id", decision.ID, maxDecisionStableIDBytes); err != nil {
		return canonicalPendingDecisionRequest{}, err
	}
	if err := validateExactDecisionText("question", decision.Question, maxDecisionQuestionBytes); err != nil {
		return canonicalPendingDecisionRequest{}, err
	}
	if err := validateExactDecisionText("requester", decision.Requester, maxDecisionRequesterBytes); err != nil {
		return canonicalPendingDecisionRequest{}, err
	}

	options := make([]DecisionOption, 0, len(decision.Options))
	seen := make(map[string]struct{}, len(decision.Options))
	for index, option := range decision.Options {
		if err := validateExactDecisionText(fmt.Sprintf("options[%d].id", index), option.ID, maxDecisionStableIDBytes); err != nil {
			return canonicalPendingDecisionRequest{}, err
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return canonicalPendingDecisionRequest{}, errors.New("pending decision option ids must be unique")
		}
		if err := validateExactDecisionText(fmt.Sprintf("options[%d].label", index), option.Label, maxDecisionOptionLabelBytes); err != nil {
			return canonicalPendingDecisionRequest{}, err
		}
		if err := validateExactDecisionText(fmt.Sprintf("options[%d].consequence", index), option.Consequence, maxDecisionOptionConsequenceBytes); err != nil {
			return canonicalPendingDecisionRequest{}, err
		}
		seen[option.ID] = struct{}{}
		options = append(options, option)
	}

	return canonicalPendingDecisionRequest{
		Version:     pendingDecisionRequestBindingVersion,
		TaskID:      taskID,
		DecisionID:  decision.ID,
		Question:    decision.Question,
		Options:     options,
		Requester:   decision.Requester,
		RequestedAt: decision.RequestedAt.UTC().Format(time.RFC3339Nano),
		Status:      DecisionStatusPending,
		Sensitive:   decision.Sensitive,
	}, nil
}

func validateExactDecisionText(field, value string, limit int) error {
	normalized, err := normalizeDecisionText(field, value, limit)
	if err != nil {
		return err
	}
	if normalized != value {
		return fmt.Errorf("pending decision %s must not contain surrounding whitespace", field)
	}
	return nil
}

type pendingDecisionWire struct {
	ID          string           `json:"id"`
	Question    string           `json:"question"`
	Options     []DecisionOption `json:"options"`
	Requester   string           `json:"requester"`
	RequestedAt string           `json:"requestedAt"`
	Status      DecisionStatus   `json:"status"`
	Sensitive   bool             `json:"sensitive,omitempty"`
	Answer      json.RawMessage  `json:"answer"`
	Responder   json.RawMessage  `json:"responder"`
	AnsweredAt  json.RawMessage  `json:"answeredAt"`
	EvidenceID  json.RawMessage  `json:"evidenceId"`
}

func parsePendingDecision(raw json.RawMessage) (*PendingDecision, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("pending decision is not valid UTF-8")
	}

	var wire pendingDecisionWire
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode pending decision: %w", err)
	}
	if err := ensureDecisionJSONEOF(decoder); err != nil {
		return nil, err
	}
	if wire.Status != DecisionStatusPending {
		return nil, fmt.Errorf("pending decision status must be %q", DecisionStatusPending)
	}
	if len(wire.Answer) > 0 || len(wire.Responder) > 0 || len(wire.AnsweredAt) > 0 || len(wire.EvidenceID) > 0 {
		return nil, fmt.Errorf("pending decision contains answer or evidence fields")
	}
	if len(wire.Options) < minPendingDecisionOptions || len(wire.Options) > maxPendingDecisionOptions {
		return nil, fmt.Errorf("pending decision options must contain between %d and %d choices", minPendingDecisionOptions, maxPendingDecisionOptions)
	}

	id, err := normalizeDecisionText("id", wire.ID, maxDecisionStableIDBytes)
	if err != nil {
		return nil, err
	}
	question, err := normalizeDecisionText("question", wire.Question, maxDecisionQuestionBytes)
	if err != nil {
		return nil, err
	}
	requester, err := normalizeDecisionText("requester", wire.Requester, maxDecisionRequesterBytes)
	if err != nil {
		return nil, err
	}
	requestedAtText, err := normalizeDecisionText("requestedAt", wire.RequestedAt, maxDecisionTimestampBytes)
	if err != nil {
		return nil, err
	}
	requestedAt, err := time.Parse(time.RFC3339, requestedAtText)
	if err != nil || requestedAt.IsZero() {
		return nil, fmt.Errorf("pending decision requestedAt must be a non-zero RFC3339 timestamp")
	}

	options := make([]DecisionOption, 0, len(wire.Options))
	seen := make(map[string]struct{}, len(wire.Options))
	for index, rawOption := range wire.Options {
		optionID, optionErr := normalizeDecisionText(fmt.Sprintf("options[%d].id", index), rawOption.ID, maxDecisionStableIDBytes)
		if optionErr != nil {
			return nil, optionErr
		}
		if _, duplicate := seen[optionID]; duplicate {
			return nil, fmt.Errorf("pending decision option id %q is duplicated", optionID)
		}
		label, optionErr := normalizeDecisionText(fmt.Sprintf("options[%d].label", index), rawOption.Label, maxDecisionOptionLabelBytes)
		if optionErr != nil {
			return nil, optionErr
		}
		consequence, optionErr := normalizeDecisionText(fmt.Sprintf("options[%d].consequence", index), rawOption.Consequence, maxDecisionOptionConsequenceBytes)
		if optionErr != nil {
			return nil, optionErr
		}
		seen[optionID] = struct{}{}
		options = append(options, DecisionOption{ID: optionID, Label: label, Consequence: consequence})
	}

	return &PendingDecision{
		ID:          id,
		Question:    question,
		Options:     options,
		Requester:   requester,
		RequestedAt: requestedAt.UTC(),
		Status:      DecisionStatusPending,
		Sensitive:   wire.Sensitive,
	}, nil
}

func ensureDecisionJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("pending decision contains trailing JSON")
		}
		return fmt.Errorf("decode pending decision trailing JSON: %w", err)
	}
	return nil
}

func normalizeDecisionText(field, value string, limit int) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("pending decision %s is not valid UTF-8", field)
	}
	for _, r := range value {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.In(r, unicode.Cf) || r == '\u2028' || r == '\u2029' {
			return "", fmt.Errorf("pending decision %s contains unsafe control characters", field)
		}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("pending decision %s is required", field)
	}
	if len(value) > limit {
		return "", fmt.Errorf("pending decision %s exceeds %d bytes", field, limit)
	}
	return value, nil
}
