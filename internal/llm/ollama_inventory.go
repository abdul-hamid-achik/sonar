package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OllamaModelLocation describes where Ollama executes a model. LocationLocal
// means /api/tags proved local weights; LocationCloud is an Ollama cloud stub.
// Unknown is deliberately retained instead of guessing from a model name.
type OllamaModelLocation string

const (
	OllamaModelLocationUnknown OllamaModelLocation = "unknown"
	OllamaModelLocationLocal   OllamaModelLocation = "local"
	OllamaModelLocationCloud   OllamaModelLocation = "cloud"
	OllamaModelLocationRemote  OllamaModelLocation = "remote-host"
)

type ollamaModelDetails struct {
	ParentModel       string   `json:"parent_model,omitempty"`
	Format            string   `json:"format,omitempty"`
	Family            string   `json:"family,omitempty"`
	Families          []string `json:"families,omitempty"`
	ParameterSize     string   `json:"parameter_size,omitempty"`
	QuantizationLevel string   `json:"quantization_level,omitempty"`
	ContextLength     int64    `json:"context_length,omitempty"`
}

// OllamaModel is one inventory entry returned by the configured Ollama host.
// It preserves cloud stubs and custom models; policy filtering belongs above
// the wire layer.
type OllamaModel struct {
	Name          string
	Digest        string
	ModifiedAt    time.Time
	SizeBytes     int64
	RemoteModel   string
	RemoteHost    string
	Location      OllamaModelLocation
	Format        string
	Family        string
	Families      []string
	ParameterSize string
	Quantization  string
	ContextLength int64
	Capabilities  []string
}

// OllamaModelInfo contains the authoritative metadata returned by /api/show.
type OllamaModelInfo struct {
	Model         OllamaModel
	Capabilities  []string
	ModelInfo     map[string]any
	Parameters    string
	Template      string
	System        string
	License       string
	NativeContext int64
}

type ollamaShowResponse struct {
	ModifiedAt   time.Time          `json:"modified_at,omitempty"`
	Details      ollamaModelDetails `json:"details,omitempty"`
	Capabilities []string           `json:"capabilities,omitempty"`
	ModelInfo    map[string]any     `json:"model_info,omitempty"`
	Parameters   string             `json:"parameters,omitempty"`
	Template     string             `json:"template,omitempty"`
	System       string             `json:"system,omitempty"`
	License      string             `json:"license,omitempty"`
}

// OllamaRunningModel is transient residency data from /api/ps.
type OllamaRunningModel struct {
	Model         OllamaModel
	ExpiresAt     time.Time
	SizeVRAM      int64
	ContextLength int64
}

type ollamaPSResponse struct {
	Models []struct {
		Name          string             `json:"name"`
		Model         string             `json:"model"`
		Size          int64              `json:"size"`
		Digest        string             `json:"digest"`
		Details       ollamaModelDetails `json:"details"`
		ExpiresAt     time.Time          `json:"expires_at"`
		SizeVRAM      int64              `json:"size_vram"`
		ContextLength int64              `json:"context_length"`
	} `json:"models"`
}

type OllamaPullProgress struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
}

// ListModels returns every model reported by Ollama. Unlike
// ListLocalModelInventory, it intentionally does not apply privacy, memory, or
// static-catalog policy.
func (o *OllamaClient) ListModels(ctx context.Context) ([]OllamaModel, error) {
	wire, err := o.listModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Ollama models: %w", err)
	}
	models := make([]OllamaModel, 0, len(wire))
	for _, item := range wire {
		name := strings.TrimSpace(item.Model)
		if name == "" {
			name = strings.TrimSpace(item.Name)
		}
		if name == "" {
			continue
		}
		model := modelFromWire(name, item.Digest, item.Size, item.ModifiedAt, item.RemoteModel, item.RemoteHost, item.Details, item.Capabilities)
		model.Location = o.modelLocation(model.Location)
		models = append(models, model)
	}
	return models, nil
}

func (o *OllamaClient) modelLocation(wire OllamaModelLocation) OllamaModelLocation {
	if wire == OllamaModelLocationCloud || wire == OllamaModelLocationRemote || isLocalOllamaHost(o.base.Hostname()) {
		return wire
	}
	if strings.EqualFold(o.base.Hostname(), "ollama.com") || strings.EqualFold(o.base.Hostname(), "www.ollama.com") {
		return OllamaModelLocationCloud
	}
	return OllamaModelLocationRemote
}

func modelFromWire(name, digest string, size int64, modified time.Time, remoteModel, remoteHost string, details ollamaModelDetails, capabilities []string) OllamaModel {
	location := OllamaModelLocationUnknown
	switch {
	case strings.TrimSpace(remoteHost) != "" && !isOllamaCloudRemoteHost(remoteHost):
		location = OllamaModelLocationRemote
	case strings.TrimSpace(remoteModel) != "" || strings.TrimSpace(remoteHost) != "":
		location = OllamaModelLocationCloud
	case size > 0:
		location = OllamaModelLocationLocal
	}
	return OllamaModel{Name: name, Digest: digest, ModifiedAt: modified, SizeBytes: size,
		RemoteModel: remoteModel, RemoteHost: remoteHost, Location: location,
		Format: details.Format, Family: details.Family, Families: append([]string(nil), details.Families...),
		ParameterSize: details.ParameterSize, Quantization: details.QuantizationLevel,
		ContextLength: details.ContextLength,
		Capabilities:  append([]string(nil), capabilities...)}
}

func isOllamaCloudRemoteHost(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "ollama.com" || host == "www.ollama.com"
}

// ShowModel obtains capability and model-native metadata without verbose token
// tables, keeping the response within the shared metadata bound.
func (o *OllamaClient) ShowModel(ctx context.Context, model string) (OllamaModelInfo, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return OllamaModelInfo{}, errors.New("model name is required")
	}
	var response ollamaShowResponse
	if err := o.doJSON(ctx, http.MethodPost, "/api/show", map[string]any{"model": model, "verbose": false}, &response); err != nil {
		return OllamaModelInfo{}, fmt.Errorf("show Ollama model %q: %w", model, err)
	}
	info := OllamaModelInfo{
		Model:        modelFromWire(model, "", 0, response.ModifiedAt, "", "", response.Details, response.Capabilities),
		Capabilities: append([]string(nil), response.Capabilities...), ModelInfo: cloneJSONMap(response.ModelInfo),
		Parameters: response.Parameters, Template: response.Template, System: response.System, License: response.License,
	}
	info.Model.Location = o.modelLocation(info.Model.Location)
	info.NativeContext = nativeContextLength(response.ModelInfo, response.Details.Family)
	return info, nil
}

func cloneJSONMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func nativeContextLength(info map[string]any, family string) int64 {
	preferred := strings.TrimSpace(family) + ".context_length"
	if value, ok := numericInt64(info[preferred]); ok {
		return value
	}
	var largest int64
	for key, value := range info {
		if strings.HasSuffix(key, ".context_length") {
			if result, ok := numericInt64(value); ok && result > largest {
				largest = result
			}
		}
	}
	return largest
}

func numericInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		if number <= 0 || number > math.MaxInt64 || number != math.Trunc(number) {
			return 0, false
		}
		return int64(number), true
	case json.Number:
		result, err := number.Int64()
		return result, err == nil && result > 0
	default:
		return 0, false
	}
}

func (o *OllamaClient) ListRunningModels(ctx context.Context) ([]OllamaRunningModel, error) {
	var response ollamaPSResponse
	if err := o.doJSON(ctx, http.MethodGet, "/api/ps", nil, &response); err != nil {
		return nil, fmt.Errorf("list running Ollama models: %w", err)
	}
	models := make([]OllamaRunningModel, 0, len(response.Models))
	for _, item := range response.Models {
		name := item.Model
		if strings.TrimSpace(name) == "" {
			name = item.Name
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		model := modelFromWire(name, item.Digest, item.Size, time.Time{}, "", "", item.Details, nil)
		model.Location = o.modelLocation(model.Location)
		models = append(models, OllamaRunningModel{Model: model, ExpiresAt: item.ExpiresAt, SizeVRAM: item.SizeVRAM, ContextLength: item.ContextLength})
	}
	return models, nil
}

func (o *OllamaClient) Version(ctx context.Context) (string, error) {
	var response struct {
		Version string `json:"version"`
	}
	if err := o.doJSON(ctx, http.MethodGet, "/api/version", nil, &response); err != nil {
		return "", fmt.Errorf("get Ollama version: %w", err)
	}
	if strings.TrimSpace(response.Version) == "" {
		return "", errors.New("ollama version response is empty")
	}
	return response.Version, nil
}

// PullModel streams bounded Ollama pull progress. Cancellation propagates
// through the request context and callback errors stop the stream immediately.
func (o *OllamaClient) PullModel(ctx context.Context, model string, fn func(OllamaPullProgress) error) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model name is required")
	}
	if fn == nil {
		return errors.New("pull progress callback is nil")
	}
	sawSuccess := false
	err := o.streamJSON(ctx, "/api/pull", map[string]any{"model": model, "stream": true}, func(record []byte) error {
		var progress OllamaPullProgress
		if err := json.Unmarshal(record, &progress); err != nil {
			return fmt.Errorf("decode Ollama pull progress: %w", err)
		}
		if strings.EqualFold(strings.TrimSpace(progress.Status), "success") {
			sawSuccess = true
		}
		return fn(progress)
	})
	if err != nil {
		return fmt.Errorf("pull Ollama model %q: %w", model, err)
	}
	if !sawSuccess {
		return fmt.Errorf("pull Ollama model %q ended before success", model)
	}
	return nil
}
