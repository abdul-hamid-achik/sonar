package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewImageDataValidatesNormalizesAndCopies(t *testing.T) {
	original := []byte{0x89, 'P', 'N', 'G'}
	image, err := NewImageData(" IMAGE/PNG; version=1 ", original)
	if err != nil {
		t.Fatal(err)
	}
	if image.MediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", image.MediaType)
	}
	original[0] = 0
	if image.Data[0] != 0x89 {
		t.Fatal("image data aliases caller-owned bytes")
	}
}

func TestImageDataRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		data      []byte
		want      string
	}{
		{name: "empty type", data: []byte{1}, want: "invalid image media type"},
		{name: "not image", mediaType: "text/plain", data: []byte{1}, want: "is not an image"},
		{name: "missing subtype", mediaType: "image/", data: []byte{1}, want: "invalid image media type"},
		{name: "empty data", mediaType: "image/png", want: "image payload is empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewImageData(test.mediaType, test.data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewImageData error = %v, want fragment %q", err, test.want)
			}
		})
	}
}

func TestMessageImagePayloadIsNeverJSONSerialized(t *testing.T) {
	secret := "RAW_IMAGE_PAYLOAD_MUST_NOT_PERSIST"
	image, err := NewReferencedImageData("../../screen shot.png", "image/png", 1200, 800, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	message := Message{
		Role:    "user",
		Content: "inspect this",
		Images:  []ImageData{image},
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "../") || strings.Contains(string(encoded), `"data"`) {
		t.Fatalf("raw or path-bearing image data leaked to JSON: %s", encoded)
	}
	for _, want := range []string{`"images"`, `"sha256"`, `"name":"screen shot.png"`, `"mime_type":"image/png"`, `"size_bytes":`, `"width":1200`, `"height":800`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("message JSON = %s, missing %s", encoded, want)
		}
	}

	var restored Message
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Images) != 1 || len(restored.Images[0].Data) != 0 {
		t.Fatalf("restored image = %#v, want metadata without bytes", restored.Images)
	}
	if err := restored.Images[0].ValidateReference(); err != nil {
		t.Fatalf("restored reference is invalid: %v", err)
	}
}

func TestReferencedImageDataVerifiesHydratedContent(t *testing.T) {
	data := []byte("trusted image bytes")
	image, err := NewReferencedImageData(`C:\private\capture.png`, "image/png", 640, 480, data)
	if err != nil {
		t.Fatal(err)
	}
	if image.Name != "capture.png" || image.Size != int64(len(data)) || len(image.SHA256) != 64 {
		t.Fatalf("reference metadata = %#v", image)
	}
	reference := image
	reference.Data = nil
	hydrated, err := reference.WithData(data)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	if string(hydrated.Data) != "trusted image bytes" {
		t.Fatal("hydrated image aliases resolver-owned bytes")
	}
	if _, err := reference.WithData([]byte("wrong image bytes")); err == nil {
		t.Fatal("accepted bytes that do not match durable image reference")
	}
}

func TestSanitizeImageNameStripsBidirectionalControls(t *testing.T) {
	if got := SanitizeImageName("../../screen\u202egnp.png"); got != "screengnp.png" {
		t.Fatalf("SanitizeImageName() = %q, want bidi-safe basename", got)
	}

	image, err := NewReferencedImageData("screen.png", "image/png", 4, 3, []byte("trusted image bytes"))
	if err != nil {
		t.Fatal(err)
	}
	image.Name = "screen\u202egnp.png"
	if err := image.ValidateReference(); err == nil {
		t.Fatal("ValidateReference accepted bidirectional control in image name")
	}
}
