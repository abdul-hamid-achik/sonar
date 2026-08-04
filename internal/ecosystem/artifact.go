package ecosystem

import (
	"fmt"
	"strings"
	"time"
)

const (
	maxProjectionArtifactIDBytes = 128
	fileCheapManifestSchema      = "1.0"
	hitspecCaptureSchema         = "hitspec.capture.v1"
	// artifactRefSchemaURI pins file.cheap's portable cross-tool reference
	// contract (contracts/artifact-ref/v1/schema.json). Monitor incident
	// investigations and fcheap_artifact_ref both emit this envelope.
	artifactRefSchemaURI = "urn:filecheap.dev:artifact-ref:v1"
	// maxPortableArtifactRefIDBytes matches the contract's 160-char artifact_id
	// ceiling for hosted references; local stash IDs stay under the stricter
	// stash bound.
	maxPortableArtifactRefIDBytes = 160
	maxPortableArtifactKindBytes  = 128
)

// ArtifactDigestKind identifies one exact, bounded artifact contract. Artifact
// digests contain durable identity and integrity metadata only; paths, file
// trees, tags, custom fields, scan findings, and provider prose stay inside the
// short-lived parser boundary.
type ArtifactDigestKind string

const (
	ArtifactDigestFileCheapStash ArtifactDigestKind = "filecheap_stash"
	ArtifactDigestHitspecCapture ArtifactDigestKind = "hitspec_capture"
	// ArtifactDigestPortableRef is a validated ArtifactRefV1 envelope: a
	// credential-free pointer to an artifact another tool owns. It proves
	// identity and provenance, never content integrity — the contract
	// deliberately carries no hash.
	ArtifactDigestPortableRef ArtifactDigestKind = "artifact_ref"
)

// Portable reference providers accepted from the ArtifactRefV1 contract.
const (
	artifactRefProviderLocal = "fcheap-local"
	artifactRefProviderCloud = "fcheap-cloud"
	artifactRefProviderLink  = "link"
)

// ArtifactDigest is the persistable projection of one durable artifact. URI is
// host-derived during Normalize and is never accepted from an MCP response or a
// restored session. SchemaVersion identifies the exact parser contract: it is
// file.cheap's manifest version for direct saves and a host-owned projection
// version for Hitspec capture receipts. ContentSHA256 is retained only for the
// direct file.cheap manifest contract, which guarantees its algorithm.
type ArtifactDigest struct {
	Kind           ArtifactDigestKind `json:"kind"`
	ID             string             `json:"id"`
	URI            string             `json:"uri"`
	SchemaVersion  string             `json:"schema_version"`
	ContentSHA256  string             `json:"content_sha256"`
	FileCount      int64              `json:"file_count"`
	TotalSize      int64              `json:"total_size"`
	CreatedAt      string             `json:"created_at"`
	SecretsWarning bool               `json:"secrets_warning,omitempty"`
	IndexingFailed bool               `json:"indexing_failed,omitempty"`
	// Provider and RefKind exist only on portable ArtifactRefV1 digests: the
	// reference provider and the producer-declared artifact kind (for example
	// monitor.incident). They are bounded contract enums, never free prose.
	Provider string `json:"provider,omitempty"`
	RefKind  string `json:"ref_kind,omitempty"`
}

func normalizeArtifactDigest(digest ArtifactDigest) ArtifactDigest {
	if !validProjectionMetric(digest.FileCount) || !validProjectionMetric(digest.TotalSize) {
		return ArtifactDigest{}
	}
	switch digest.Kind {
	case ArtifactDigestFileCheapStash:
		if !validFileCheapStashID(digest.ID) || digest.SchemaVersion != fileCheapManifestSchema ||
			!validLowerSHA256(digest.ContentSHA256) || digest.CreatedAt == "" ||
			digest.Provider != "" || digest.RefKind != "" {
			return ArtifactDigest{}
		}
	case ArtifactDigestHitspecCapture:
		if !validFileCheapStashID(digest.ID) || digest.SchemaVersion != hitspecCaptureSchema ||
			digest.ContentSHA256 != "" || digest.FileCount < 1 ||
			digest.Provider != "" || digest.RefKind != "" {
			return ArtifactDigest{}
		}
	case ArtifactDigestPortableRef:
		if digest.SchemaVersion != artifactRefSchemaURI || digest.ContentSHA256 != "" ||
			digest.CreatedAt != "" || !validPortableArtifactKind(digest.RefKind) {
			return ArtifactDigest{}
		}
		switch digest.Provider {
		case artifactRefProviderLocal:
			if !validFileCheapStashID(digest.ID) {
				return ArtifactDigest{}
			}
		case artifactRefProviderCloud:
			if !validPortableArtifactID(digest.ID) {
				return ArtifactDigest{}
			}
		case artifactRefProviderLink:
			// A link reference is a metadata-only pointer without an artifact
			// identity the host could later resolve.
			if digest.ID != "" {
				return ArtifactDigest{}
			}
		default:
			return ArtifactDigest{}
		}
	default:
		return ArtifactDigest{}
	}
	if digest.CreatedAt != "" {
		createdAt, err := time.Parse(time.RFC3339, digest.CreatedAt)
		if err != nil {
			return ArtifactDigest{}
		}
		digest.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	}
	// URI stays host-derived: only a local stash identity yields a resolvable
	// URI. Cloud and link URIs from the wire are transport metadata and are
	// deliberately not persisted.
	if digest.Kind == ArtifactDigestPortableRef && digest.Provider != artifactRefProviderLocal {
		digest.URI = ""
	} else {
		digest.URI = fileCheapStashURI(digest.ID)
	}
	return digest
}

func (d ArtifactDigest) summaryText() string {
	d = normalizeArtifactDigest(d)
	if d.Kind == "" {
		return ""
	}
	if d.Kind == ArtifactDigestPortableRef {
		parts := []string{"artifact ref", d.RefKind}
		if d.URI != "" {
			parts = append(parts, d.URI)
		} else {
			parts = append(parts, d.Provider)
			if d.ID != "" {
				parts = append(parts, d.ID)
			}
		}
		return strings.Join(parts, " · ")
	}
	parts := []string{
		d.URI,
		metricLabel(d.FileCount, "file", "files"),
		fmt.Sprintf("%d bytes", d.TotalSize),
	}
	if d.SecretsWarning {
		parts = append(parts, "potential secrets need review")
	}
	if d.IndexingFailed {
		parts = append(parts, "saved; indexing incomplete")
	}
	return strings.Join(parts, " · ")
}

// validPortableArtifactID accepts the ArtifactRefV1 artifact_id shape for
// hosted references: the stash charset with the contract's larger ceiling.
func validPortableArtifactID(id string) bool {
	if id == "" || len(id) > maxPortableArtifactRefIDBytes || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// validPortableArtifactKind enforces the contract's kind grammar:
// ^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$ within the 128-byte ceiling.
func validPortableArtifactKind(kind string) bool {
	if kind == "" || len(kind) > maxPortableArtifactKindBytes {
		return false
	}
	segmentStart := true
	for index, r := range kind {
		switch {
		case r >= 'a' && r <= 'z':
			segmentStart = false
		case r >= '0' && r <= '9':
			if index == 0 {
				return false
			}
			segmentStart = false
		case r == '.' || r == '_' || r == '-':
			if segmentStart {
				return false
			}
			segmentStart = true
		default:
			return false
		}
	}
	return !segmentStart
}

func fileCheapStashURI(id string) string {
	return "fcheap://stash/" + id
}

// validFileCheapStashID accepts a bounded portable URI path element. The
// current file.cheap generator emits lowercase ASCII IDs, while preserving
// case here keeps the value opaque and avoids inventing a different identity.
func validFileCheapStashID(id string) bool {
	if id == "" || len(id) > maxProjectionArtifactIDBytes || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	reserved := strings.ToLower(id)
	if reserved == "fcheap.db" || strings.HasPrefix(reserved, "fcheap.db-") ||
		reserved == "fcheap.veclite" || strings.HasPrefix(reserved, "fcheap.veclite.") {
		return false
	}
	return true
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validProjectionMetric(value int64) bool {
	return value >= 0 && value <= maxProjectionMetric
}
