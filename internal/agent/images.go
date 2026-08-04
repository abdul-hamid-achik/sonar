package agent

import (
	"context"
	"fmt"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
)

// ImageResolver resolves one validated path-free durable reference into raw
// bytes. Implementations should use SHA256 as the storage identity and honor
// context cancellation.
type ImageResolver func(context.Context, llm.ImageData) ([]byte, error)

func cloneValidatedImages(images []llm.ImageData) ([]llm.ImageData, error) {
	if len(images) == 0 {
		return nil, nil
	}
	cloned := cloneImages(images)
	for index, image := range cloned {
		var err error
		if len(image.Data) == 0 {
			err = image.ValidateReference()
		} else {
			err = image.Validate()
		}
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", index, err)
		}
	}
	return cloned, nil
}

func cloneImages(images []llm.ImageData) []llm.ImageData {
	if len(images) == 0 {
		return nil
	}
	cloned := make([]llm.ImageData, len(images))
	for index, image := range images {
		cloned[index] = image
		cloned[index].Data = append([]byte(nil), image.Data...)
	}
	return cloned
}

func cloneMessagesWithImages(messages []llm.Message) []llm.Message {
	// Preserve Agent.Messages' historical non-nil empty-slice contract. Some
	// rollback checkpoints use DeepEqual on a zero-length prefix.
	cloned := make([]llm.Message, len(messages))
	for index, message := range messages {
		cloned[index] = message
		cloned[index].Images = cloneImages(message.Images)
	}
	return cloned
}

// resolveProviderImages returns an independent request snapshot. Missing bytes
// are resolved outside Agent locks and verified against the full content
// address before the provider can observe the request.
func (a *Agent) resolveProviderImages(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	a.mu.RLock()
	resolver := a.imageResolver
	a.mu.RUnlock()

	resolved := cloneMessagesWithImages(messages)
	for messageIndex := range resolved {
		for imageIndex := range resolved[messageIndex].Images {
			image := resolved[messageIndex].Images[imageIndex]
			if len(image.Data) > 0 {
				if err := image.Validate(); err != nil {
					return nil, imageResolutionError(messageIndex, imageIndex, err)
				}
				continue
			}
			if err := image.ValidateReference(); err != nil {
				return nil, imageResolutionError(messageIndex, imageIndex, err)
			}
			if resolver == nil {
				return nil, imageResolutionError(messageIndex, imageIndex, fmt.Errorf("image resolver is not configured"))
			}
			if err := ctx.Err(); err != nil {
				return nil, imageResolutionError(messageIndex, imageIndex, err)
			}
			reference := image
			reference.Data = nil
			data, err := resolver(ctx, reference)
			if err != nil {
				return nil, imageResolutionError(messageIndex, imageIndex, err)
			}
			if err := ctx.Err(); err != nil {
				return nil, imageResolutionError(messageIndex, imageIndex, err)
			}
			hydrated, err := image.WithData(data)
			if err != nil {
				return nil, imageResolutionError(messageIndex, imageIndex, err)
			}
			resolved[messageIndex].Images[imageIndex] = hydrated
		}
	}
	return resolved, nil
}

// chatStreamWithResolvedImages is the single provider-dispatch boundary for
// agent-owned requests. Keeping resolution here makes both ordinary inference
// and compaction fail before dispatch when a durable image cannot be hydrated.
func (a *Agent) chatStreamWithResolvedImages(ctx context.Context, options llm.ChatOptions, emit func(llm.StreamChunk) error) error {
	messages, err := a.resolveProviderImages(ctx, options.Messages)
	if err != nil {
		return err
	}
	options.Messages = messages
	return a.llmClient.ChatStream(ctx, options, emit)
}

func imageResolutionError(messageIndex, imageIndex int, err error) error {
	return fmt.Errorf(
		"%w: resolve image %d for message %d: %w",
		llm.ErrInferenceNotStarted,
		imageIndex,
		messageIndex,
		err,
	)
}
