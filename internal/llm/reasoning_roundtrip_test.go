package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

// DeepSeek's reasoning_content round-trip, verified against the live API rather
// than inferred from the docs. Two requests, identical except for one field:
//
//	assistant tool-call message WITHOUT reasoning_content -> HTTP 400
//	  "The `reasoning_content` in the thinking mode must be passed back to the
//	   API."
//	same message WITH reasoning_content: ""                -> HTTP 200
//
// The 400 fires on the field's *absence*, not on an empty value. That single
// observation is what fixes the wire type: the API is checking that the key is
// present, and is content with an empty string.
//
// It matters because llm.Message.ReasoningContent is `json:"-"`. Private
// chain-of-thought must never cross a session, transcript, or checkpoint
// boundary, so every assistant tool-call message restored from a saved session
// arrives with empty reasoning — which is exactly the case the 200 above
// covers and the 400 above forbids. A resumed session's very next turn is a
// tool-call turn, so this is the ordinary path, not an edge case.
//
// openAIMessage.ReasoningContent is therefore a *string. A plain string with
// omitempty cannot express "present but empty": encoding/json drops the key,
// producing the 400. A plain string without omitempty would send the key on
// every message, widening how far reasoning travels for no protocol benefit.
// The pointer is the only shape that says both things.
//
// These tests exist so that reasoning is not "simplified" into a string. The
// simplification compiles, passes a casual reading, and breaks every resumed
// tool-call session against a thinking-mode provider.

// The wire field must stay a pointer. This is the structural half: a change to
// the field's type or tag fails here with the reason attached, before anyone
// has to rediscover the 400 empirically.
func TestReasoningContentWireFieldStaysAPointer(t *testing.T) {
	field, ok := reflect.TypeOf(openAIMessage{}).FieldByName("ReasoningContent")
	if !ok {
		t.Fatal("openAIMessage has no ReasoningContent field")
	}
	if field.Type.Kind() != reflect.Pointer || field.Type.Elem().Kind() != reflect.String {
		t.Fatalf(
			"ReasoningContent is %s, want *string: DeepSeek answers 400 when an assistant "+
				"tool-call message omits reasoning_content and 200 when it carries \"\", "+
				"and only a pointer can send an empty value while still omitting the key elsewhere",
			field.Type,
		)
	}
	if got := field.Tag.Get("json"); got != "reasoning_content,omitempty" {
		t.Fatalf("json tag = %q, want %q", got, "reasoning_content,omitempty")
	}
}

// The behavioral half, from both sides of the pointer: a nil pointer omits the
// key, and a pointer to "" emits it. These are the two live HTTP outcomes
// expressed as encoding, and they are what a plain string cannot separate.
func TestReasoningContentPointerSeparatesAbsentFromEmpty(t *testing.T) {
	empty := ""
	tests := []struct {
		name        string
		reasoning   *string
		wantPresent bool
		wantValue   any
	}{
		{
			// The 400 case. A message that must not carry reasoning at all
			// (a plain assistant answer, or any non-DeepSeek host) omits it.
			name:        "nil omits the key",
			reasoning:   nil,
			wantPresent: false,
		},
		{
			// The 200 case. A restored tool-call turn has no reasoning text to
			// echo, and must still send the key.
			name:        "a pointer to the empty string still emits the key",
			reasoning:   &empty,
			wantPresent: true,
			wantValue:   "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(openAIMessage{Role: "assistant", ReasoningContent: test.reasoning})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			value, present := decoded["reasoning_content"]
			if present != test.wantPresent {
				t.Fatalf("reasoning_content present = %v, want %v (payload %s)", present, test.wantPresent, encoded)
			}
			if present && value != test.wantValue {
				t.Fatalf("reasoning_content = %#v, want %#v", value, test.wantValue)
			}
		})
	}
}

// Why not a plain string: the simplification anyone would reach for produces
// exactly the payload DeepSeek answers 400 to. Pinning the failure mode keeps
// the pointer from looking like an accident of style.
func TestOmitemptyStringCannotExpressPresentButEmptyReasoning(t *testing.T) {
	simplified := struct {
		Role             string `json:"role"`
		ReasoningContent string `json:"reasoning_content,omitempty"`
	}{Role: "assistant", ReasoningContent: ""}

	encoded, err := json.Marshal(simplified)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := decoded["reasoning_content"]; present {
		t.Fatalf(
			"an omitempty string emitted reasoning_content for an empty value (%s); "+
				"if encoding/json ever changes this, the pointer is still correct for "+
				"the messages that must omit the key entirely",
			encoded,
		)
	}
}

// End to end on the wire, through the real client: a session restored from disk
// holds an assistant tool-call message whose ReasoningContent is empty, because
// llm.Message.ReasoningContent is `json:"-"` and no persisted session can carry
// it. That request must still put "reasoning_content": "" on the wire, or
// DeepSeek answers 400 and the resumed turn never runs.
func TestRestoredToolCallTurnStillSendsEmptyReasoningContent(t *testing.T) {
	restored := []Message{
		{Role: "user", Content: "read the file"},
		{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "call_1", Name: "read", Arguments: map[string]any{"path": "a.txt"}}},
			// No ReasoningContent: json:"-" means a reloaded session cannot
			// have one, whatever the model originally produced.
		},
		{Role: "tool", ToolCallID: "call_1", Content: "contents"},
	}
	body := captureDeepSeekRequest(t, DialectDeepSeek, ChatOptions{Messages: restored})

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("messages = %#v, want three", body["messages"])
	}
	assistant, ok := messages[1].(map[string]any)
	if !ok {
		t.Fatalf("messages[1] = %#v, want an object", messages[1])
	}
	value, present := assistant["reasoning_content"]
	if !present {
		t.Fatal("a restored tool-call turn dropped reasoning_content; DeepSeek answers this request with HTTP 400")
	}
	if value != "" {
		t.Fatalf("reasoning_content = %#v, want the empty string DeepSeek accepts with HTTP 200", value)
	}
}

// The host-side source of the emptiness, stated as a test so the two facts stay
// connected: Message.ReasoningContent is `json:"-"`, so persistence cannot carry
// reasoning across a restart even if a writer forgot to strip it.
func TestMessageReasoningContentIsNeverPersisted(t *testing.T) {
	field, ok := reflect.TypeOf(Message{}).FieldByName("ReasoningContent")
	if !ok {
		t.Fatal("Message has no ReasoningContent field")
	}
	if got := field.Tag.Get("json"); got != "-" {
		t.Fatalf("json tag = %q, want \"-\": private chain-of-thought must not cross a session, transcript, or checkpoint boundary", got)
	}

	encoded, err := json.Marshal(Message{Role: "assistant", ReasoningContent: "private chain of thought"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := decoded["reasoning_content"]; present {
		t.Fatalf("a persisted message carried reasoning: %s", encoded)
	}
}
