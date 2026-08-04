package ui

import "github.com/abdul-hamid-achik/sonar/internal/command"

// commandArtifactInfos projects only normalized, completed artifact receipts
// into slash-command state. The first occurrence of a host-derived URI wins,
// preserving deterministic transcript order without exposing tool payloads.
func commandArtifactInfos(entries []ToolEntry) ([]command.ArtifactInfo, bool) {
	capacity := len(entries)
	if capacity > command.MaxContextArtifacts {
		capacity = command.MaxContextArtifacts
	}
	artifacts := make([]command.ArtifactInfo, 0, capacity)
	seen := make(map[string]struct{}, capacity)
	for _, entry := range entries {
		if entry.Status != ToolStatusDone {
			continue
		}
		projection := entry.Projection.Normalize()
		if projection.Artifact == nil {
			continue
		}
		artifact := projection.Artifact
		if _, duplicate := seen[artifact.URI]; duplicate {
			continue
		}
		seen[artifact.URI] = struct{}{}
		if len(artifacts) == command.MaxContextArtifacts {
			return artifacts, true
		}
		artifacts = append(artifacts, command.ArtifactInfo{
			URI:            artifact.URI,
			FileCount:      artifact.FileCount,
			TotalBytes:     artifact.TotalSize,
			CreatedAt:      artifact.CreatedAt,
			ContentSHA256:  artifact.ContentSHA256,
			SecretsWarning: artifact.SecretsWarning,
			IndexingFailed: artifact.IndexingFailed,
		})
	}
	if len(artifacts) == 0 {
		return nil, false
	}
	return artifacts, false
}
