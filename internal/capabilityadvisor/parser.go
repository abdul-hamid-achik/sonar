package capabilityadvisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/mcp"
)

const (
	resolverContractVersion    = 1
	maxResolverResultBytes     = 64 * 1024
	maxIdentifierBytes         = 256
	maxRequiredFields          = 48
	maxRequiredFieldBytes      = 128
	maxRequiredFieldNamesBytes = 2048
	maxAlternatives            = resolverMaxHits
)

type resolverEnvelope struct {
	Error                     json.RawMessage `json:"error"`
	ContractVersion           *int            `json:"contract_version"`
	Status                    *string         `json:"status"`
	Recommendation            json.RawMessage `json:"recommendation"`
	Ambiguous                 *bool           `json:"ambiguous"`
	Alternatives              json.RawMessage `json:"alternatives"`
	CatalogRevision           *string         `json:"catalog_revision"`
	ArgumentTemplateTruncated *bool           `json:"argument_template_truncated"`
	AlternativesTruncated     *bool           `json:"alternatives_truncated"`
}

type recommendationWire struct {
	Namespaced                string          `json:"namespaced"`
	Server                    string          `json:"server"`
	Tool                      string          `json:"tool"`
	RequiredFields            json.RawMessage `json:"required_fields"`
	MetadataTruncated         *bool           `json:"metadata_truncated"`
	ArgumentTemplateTruncated *bool           `json:"argument_template_truncated"`
}

type alternativeWire struct {
	Namespaced string `json:"namespaced"`
}

func parseResolverResult(result *mcp.ToolResult) (*Hint, bool, string, error) {
	document, err := resolverDocument(result)
	if err != nil {
		return nil, false, "", err
	}
	var envelope resolverEnvelope
	if err := json.Unmarshal(document, &envelope); err != nil {
		return nil, false, "", fmt.Errorf("decode resolver response: %w", err)
	}
	if envelope.ContractVersion == nil || *envelope.ContractVersion != resolverContractVersion {
		return nil, false, "", errors.New("resolver contract version is unsupported")
	}
	if envelope.Status == nil {
		return nil, false, "", errors.New("resolver response is missing status")
	}
	if envelope.CatalogRevision == nil {
		return nil, false, "", errors.New("resolver response is missing catalog revision")
	}
	catalogRevision, err := safeCatalogRevision(*envelope.CatalogRevision)
	if err != nil || catalogRevision == "" {
		return nil, false, "", errors.New("resolver returned an invalid catalog revision")
	}
	if hasJSONValue(envelope.Error) {
		return nil, false, "", errors.New("resolver reported an error")
	}
	if envelope.Ambiguous == nil || len(bytes.TrimSpace(envelope.Recommendation)) == 0 ||
		len(bytes.TrimSpace(envelope.Alternatives)) == 0 {
		return nil, false, "", errors.New("resolver response is missing control fields")
	}

	alternatives, err := parseAlternatives(envelope.Alternatives)
	if err != nil {
		return nil, false, "", err
	}
	recommendation := bytes.TrimSpace(envelope.Recommendation)
	if bytes.Equal(recommendation, []byte("null")) {
		if *envelope.Ambiguous || len(alternatives) != 0 {
			return nil, false, "", errors.New("no-match response has inconsistent alternatives")
		}
		if err := validateResolverStatus(envelope.Status, false, false); err != nil {
			return nil, false, "", err
		}
		return nil, false, catalogRevision, nil
	}

	var wire recommendationWire
	if err := json.Unmarshal(recommendation, &wire); err != nil {
		return nil, false, "", fmt.Errorf("decode resolver recommendation: %w", err)
	}
	if err := validateRecommendationIdentity(wire); err != nil {
		return nil, false, "", err
	}
	required, err := parseRequiredFields(wire.RequiredFields)
	if err != nil {
		return nil, false, "", err
	}
	if err := validateAlternativeSet(wire.Namespaced, alternatives); err != nil {
		return nil, false, "", err
	}
	if err := validateResolverStatus(envelope.Status, true, *envelope.Ambiguous); err != nil {
		return nil, false, "", err
	}

	hint := &Hint{
		Namespaced:                wire.Namespaced,
		Server:                    wire.Server,
		Tool:                      wire.Tool,
		RequiredFields:            required,
		Alternatives:              alternatives,
		Ambiguous:                 *envelope.Ambiguous,
		MetadataTruncated:         boolValue(wire.MetadataTruncated),
		ArgumentTemplateTruncated: boolValue(wire.ArgumentTemplateTruncated) || boolValue(envelope.ArgumentTemplateTruncated),
		AlternativesTruncated:     boolValue(envelope.AlternativesTruncated),
	}
	return hint, true, catalogRevision, nil
}

// validateResolverStatus accepts MCPHub's versioned confidence conclusion.
// Scores, confidence values, reason prose, and matched terms remain outside
// the allowlisted host projection. parseResolverResult requires status before
// calling this helper.
func validateResolverStatus(status *string, matched, ambiguous bool) error {
	want := "confident"
	if !matched {
		want = "no_match"
	} else if ambiguous {
		want = "ambiguous"
	}
	if *status != want {
		return errors.New("resolver status is inconsistent with its recommendation")
	}
	return nil
}

func resolverDocument(result *mcp.ToolResult) ([]byte, error) {
	if result == nil {
		return nil, errors.New("missing resolver result")
	}
	structured := bytes.TrimSpace(result.Structured)
	if len(structured) > 0 && !bytes.Equal(structured, []byte("null")) {
		return exactJSONObject(structured)
	}
	return exactJSONObject([]byte(strings.TrimSpace(result.Content)))
}

func exactJSONObject(document []byte) ([]byte, error) {
	if len(document) == 0 || len(document) > maxResolverResultBytes || document[0] != '{' || !json.Valid(document) {
		return nil, errors.New("resolver result is not one bounded JSON object")
	}
	return document, nil
}

func hasJSONValue(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 0 && !bytes.Equal(value, []byte("null"))
}

func hasErrorMetadata(value json.RawMessage) bool { return hasJSONValue(value) }

func parseAlternatives(raw json.RawMessage) ([]string, error) {
	var values []alternativeWire
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, errors.New("resolver alternatives are not an array")
	}
	if len(values) > maxAlternatives {
		return nil, errors.New("resolver returned too many alternatives")
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !validNamespacedIdentifier(value.Namespaced) {
			return nil, errors.New("resolver alternative has an invalid identifier")
		}
		result = append(result, value.Namespaced)
	}
	return result, nil
}

func validateRecommendationIdentity(wire recommendationWire) error {
	if !validIdentifier(wire.Server, false) || !validIdentifier(wire.Tool, true) ||
		!validNamespacedIdentifier(wire.Namespaced) || wire.Namespaced != wire.Server+"__"+wire.Tool {
		return errors.New("resolver recommendation has an inconsistent identity")
	}
	return nil
}

func validNamespacedIdentifier(value string) bool {
	server, tool, found := strings.Cut(value, "__")
	return found && validIdentifier(server, false) && validIdentifier(tool, true)
}

func validIdentifier(value string, allowDoubleUnderscore bool) bool {
	if value == "" || len(value) > maxIdentifierBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	if !allowDoubleUnderscore && strings.Contains(value, "__") {
		return false
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("_-.:/", r) {
			continue
		}
		return false
	}
	return true
}

func parseRequiredFields(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("resolver recommendation is missing required_fields")
	}
	var fields []string
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("resolver required_fields is not an array")
	}
	if len(fields) > maxRequiredFields {
		return nil, errors.New("resolver returned too many required fields")
	}
	seen := make(map[string]struct{}, len(fields))
	total := 0
	for _, field := range fields {
		if field == "" || len(field) > maxRequiredFieldBytes || !utf8.ValidString(field) || strings.TrimSpace(field) != field {
			return nil, errors.New("resolver returned an invalid required field")
		}
		for _, r := range field {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("_-.:/$[]", r) {
				continue
			}
			return nil, errors.New("resolver required field is not a safe field identifier")
		}
		if _, duplicate := seen[field]; duplicate {
			return nil, errors.New("resolver returned duplicate required fields")
		}
		seen[field] = struct{}{}
		total += len(field)
		if total > maxRequiredFieldNamesBytes {
			return nil, errors.New("resolver required field names exceed the byte budget")
		}
	}
	return append([]string(nil), fields...), nil
}

func validateAlternativeSet(recommendation string, alternatives []string) error {
	seen := map[string]struct{}{recommendation: {}}
	for _, alternative := range alternatives {
		if _, duplicate := seen[alternative]; duplicate {
			return errors.New("resolver alternatives contain a duplicate target")
		}
		seen[alternative] = struct{}{}
	}
	return nil
}

func boolValue(value *bool) bool { return value != nil && *value }
