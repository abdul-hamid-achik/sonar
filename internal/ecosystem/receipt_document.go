package ecosystem

import (
	"bytes"
	"encoding/json"
	"strings"
)

func receiptDocument(receipt RawReceipt) (json.RawMessage, bool) {
	// Any non-empty Structured value, including the host's null rejection
	// marker, proves that typed content was present. Never fall back to a
	// duplicated text block when that typed value is unsupported or oversized;
	// doing so would let arbitrary typed fields escape the parser boundary.
	if hasStructuredReceipt(receipt) {
		return exactJSONDocument(receipt.Structured)
	}
	return exactJSONDocument(json.RawMessage(strings.TrimSpace(receipt.Text)))
}

func hasStructuredReceipt(receipt RawReceipt) bool {
	return len(bytes.TrimSpace(receipt.Structured)) > 0
}

func exactJSONDocument(raw json.RawMessage) (json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') || !json.Valid(raw) {
		return nil, false
	}
	return append(json.RawMessage(nil), raw...), true
}

func jsonObjectHasValue(document json.RawMessage, key string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(document, &object) != nil {
		return false
	}
	value, exists := object[key]
	if !exists {
		return false
	}
	value = bytes.TrimSpace(value)
	return len(value) > 0 && !bytes.Equal(value, []byte("null"))
}

func jsonObjectHasKey(document json.RawMessage, key string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(document, &object) != nil {
		return false
	}
	_, exists := object[key]
	return exists
}

// receiptDocumentWithinLimit checks the selected parser input before
// receiptDocument copies or unmarshals it. StructuredContent keeps precedence
// over duplicated text, including for malformed structured values.
func receiptDocumentWithinLimit(receipt RawReceipt, maximum int) bool {
	if maximum <= 0 {
		return false
	}
	if hasStructuredReceipt(receipt) {
		return len(bytes.TrimSpace(receipt.Structured)) <= maximum
	}
	return len(strings.TrimSpace(receipt.Text)) <= maximum
}

func validLowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func jsonKind(raw json.RawMessage, kind byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == kind && json.Valid(raw)
}

func rawJSONPresent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && json.Valid(raw)
}

func rawJSONArrayLen(raw json.RawMessage) int {
	if !jsonKind(raw, '[') {
		return 0
	}
	var values []json.RawMessage
	_ = json.Unmarshal(raw, &values)
	return len(values)
}

func decodeStrictJSONString(raw json.RawMessage, required bool) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", !required
	}
	if raw[0] != '"' {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}
