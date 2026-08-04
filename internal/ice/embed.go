package ice

import (
	"context"
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

const (
	defaultEmbedModel = "nomic-embed-text"
	maxBatchSize      = 32
)

// Embedder wraps an LLM client to produce vector embeddings.
type Embedder struct {
	client llm.Client
	model  string
}

// NewEmbedder creates an Embedder. If model is empty, defaults to nomic-embed-text.
func NewEmbedder(client llm.Client, model string) *Embedder {
	if model == "" {
		model = defaultEmbedModel
	}
	return &Embedder{client: client, model: model}
}

// Embed produces an embedding for a single text.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	return vecs[0], nil
}

// EmbedBatch produces embeddings for multiple texts, batching in groups of maxBatchSize.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	var all [][]float32
	for i := 0; i < len(texts); i += maxBatchSize {
		// Respect cancellation between batches so a shutdown or aborted turn
		// doesn't keep embedding work it no longer needs.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := i + maxBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		vecs, err := e.client.Embed(ctx, e.model, batch)
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", i, end, err)
		}
		all = append(all, vecs...)
	}
	return all, nil
}
