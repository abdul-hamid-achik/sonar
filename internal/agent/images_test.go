package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

type imageCaptureClient struct {
	calls    int
	messages []llm.Message
}

func (client *imageCaptureClient) ChatStream(_ context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	client.calls++
	client.messages = cloneMessagesWithImages(options.Messages)
	return emit(llm.StreamChunk{Text: "image inspected", Done: true, EvalCount: 1, PromptEvalCount: 1})
}

func (*imageCaptureClient) Ping() error   { return nil }
func (*imageCaptureClient) Model() string { return "vision-test" }
func (*imageCaptureClient) Embed(context.Context, string, []string) ([][]float32, error) {
	return nil, nil
}

func TestRestoredImageReferenceIsResolvedImmediatelyBeforeDispatch(t *testing.T) {
	data := []byte("restored image bytes")
	image, err := llm.NewReferencedImageData("capture.png", "image/png", 800, 600, data)
	if err != nil {
		t.Fatal(err)
	}
	safe := SanitizeMessagesForPersistence([]llm.Message{{
		Role: "user", Content: "inspect the restored image", Images: []llm.ImageData{image},
	}})
	encoded, err := json.Marshal(safe)
	if err != nil {
		t.Fatal(err)
	}
	var restored []llm.Message
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || len(restored[0].Images) != 1 || len(restored[0].Images[0].Data) != 0 {
		t.Fatalf("restored history = %#v", restored)
	}

	client := &imageCaptureClient{}
	agent := New(client, nil, 8192)
	agent.ReplaceMessages(restored)
	resolverCalls := 0
	agent.SetImageResolver(func(_ context.Context, reference llm.ImageData) ([]byte, error) {
		resolverCalls++
		if len(reference.Data) != 0 || reference.SHA256 != image.SHA256 || strings.ContainsAny(reference.Name, `/\`) {
			t.Fatalf("resolver received unsafe reference: %#v", reference)
		}
		return data, nil
	})

	if err := agent.Run(context.Background(), &mockOutput{}); err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 1 || client.calls != 1 {
		t.Fatalf("resolver calls = %d, provider calls = %d", resolverCalls, client.calls)
	}
	if len(client.messages) == 0 || len(client.messages[0].Images) != 1 || string(client.messages[0].Images[0].Data) != string(data) {
		t.Fatalf("provider messages = %#v", client.messages)
	}
	if live := agent.Messages()[0].Images[0]; len(live.Data) != 0 {
		t.Fatalf("resolver mutated live restored history: %#v", live)
	}
}

func TestImageResolverFailurePreventsProviderDispatch(t *testing.T) {
	image, err := llm.NewReferencedImageData("capture.png", "image/png", 10, 10, []byte("image bytes"))
	if err != nil {
		t.Fatal(err)
	}
	image.Data = nil
	client := &imageCaptureClient{}
	agent := New(client, nil, 8192)
	agent.ReplaceMessages([]llm.Message{{Role: "user", Images: []llm.ImageData{image}}})
	agent.SetImageResolver(func(context.Context, llm.ImageData) ([]byte, error) {
		return nil, errors.New("asset missing")
	})

	out := &mockOutput{}
	err = agent.Run(context.Background(), out)
	if err == nil || !errors.Is(err, llm.ErrInferenceNotStarted) || !strings.Contains(err.Error(), "asset missing") {
		t.Fatalf("resolver error = %v, want inference-not-started asset failure", err)
	}
	if client.calls != 0 {
		t.Fatalf("provider calls = %d, want zero", client.calls)
	}
	if strings.Contains(strings.Join(out.errors, "\n"), "conservatively charged") {
		t.Fatalf("pre-dispatch resolver failure charged an unknown provider outcome: %#v", out.errors)
	}
}

func TestAddUserMessageWithImagesDefensivelyCopies(t *testing.T) {
	data := []byte("original image bytes")
	image, err := llm.NewReferencedImageData("capture.png", "image/png", 10, 20, data)
	if err != nil {
		t.Fatal(err)
	}
	images := []llm.ImageData{image}
	agent := New(nil, nil, 8192)
	if err := agent.AddUserMessageWithImages("inspect", images); err != nil {
		t.Fatal(err)
	}

	images[0].Name = "mutated.png"
	images[0].Data[0] = 'X'
	first := agent.Messages()
	if first[0].Images[0].Name != "capture.png" || string(first[0].Images[0].Data) != string(data) {
		t.Fatalf("source mutation reached live history: %#v", first[0].Images[0])
	}
	first[0].Images[0].Data[0] = 'Y'
	if second := agent.Messages(); string(second[0].Images[0].Data) != string(data) {
		t.Fatalf("Messages result aliases live history: %#v", second[0].Images[0])
	}
}
