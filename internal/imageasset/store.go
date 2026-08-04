// Package imageasset validates and privately stores image inputs before they
// cross into UI or provider-specific message code.
//
// Stored objects are addressed by their complete SHA-256 digest. References
// deliberately retain only bounded display metadata and never the source path.
package imageasset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const (
	defaultMaxBytes     int64  = 20 << 20
	defaultMaxWidth            = 16_384
	defaultMaxHeight           = 16_384
	defaultMaxPixels    uint64 = 24_000_000
	maxDisplayNameRunes        = 120
)

var (
	ErrUnsupportedFormat = errors.New("unsupported image format")
	ErrInvalidDimensions = errors.New("invalid image dimensions")
	ErrIntegrity         = errors.New("image object integrity check failed")
	ErrInvalidReference  = errors.New("invalid image object reference")
)

// Limits bounds image admission before any provider sees the image. Pixel
// count is checked separately from each dimension to reject decompression-bomb
// shaped inputs with otherwise plausible headers.
type Limits struct {
	MaxBytes  int64
	MaxWidth  int
	MaxHeight int
	MaxPixels uint64
}

// DefaultLimits returns conservative limits suitable for interactive image
// attachments. Callers may provide stricter limits for a specific provider.
func DefaultLimits() Limits {
	return Limits{
		MaxBytes:  defaultMaxBytes,
		MaxWidth:  defaultMaxWidth,
		MaxHeight: defaultMaxHeight,
		MaxPixels: defaultMaxPixels,
	}
}

func (l Limits) validate() error {
	if l.MaxBytes <= 0 || l.MaxWidth <= 0 || l.MaxHeight <= 0 || l.MaxPixels == 0 {
		return fmt.Errorf("image admission limits must all be positive")
	}
	if l.MaxBytes > defaultMaxBytes || l.MaxWidth > defaultMaxWidth || l.MaxHeight > defaultMaxHeight || l.MaxPixels > defaultMaxPixels {
		return fmt.Errorf("image admission limits may be stricter than defaults but not larger")
	}
	return nil
}

// Ref is safe to persist with a session. Digest is the complete lowercase
// SHA-256 digest, while Handle is a compact presentation identifier derived
// from it. Name contains only a sanitized basename, never the source path.
type Ref struct {
	Digest    string `json:"sha256"`
	MIMEType  string `json:"mime_type"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// Handle returns a short display-only identifier. Durable lookup and integrity
// checks must always use the complete Digest.
func (r Ref) Handle() string {
	if validateDigest(r.Digest) != nil {
		return "img-invalid"
	}
	return "img-" + r.Digest[:12]
}

// Validate verifies that a persisted reference is canonical and within the
// global image-admission bounds. It does not access the object store or trust
// the reference's metadata as evidence about stored bytes; Load re-derives and
// compares that metadata after this structural check.
func (r Ref) Validate() error {
	if err := r.validate(DefaultLimits()); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReference, err)
	}
	return nil
}

func (r Ref) validate(limits Limits) error {
	if err := limits.validate(); err != nil {
		return err
	}
	if err := validateDigest(r.Digest); err != nil {
		return err
	}
	if !validReferenceMIME(r.MIMEType) {
		return fmt.Errorf("unsupported MIME type %q", r.MIMEType)
	}
	if !validDisplayName(r.Name) {
		return fmt.Errorf("unsafe or non-canonical display name %q", r.Name)
	}
	if r.SizeBytes <= 0 || r.SizeBytes > limits.MaxBytes {
		return fmt.Errorf("size %d is outside 1..%d bytes", r.SizeBytes, limits.MaxBytes)
	}
	if r.Width <= 0 || r.Width > limits.MaxWidth || r.Height <= 0 || r.Height > limits.MaxHeight {
		return fmt.Errorf("dimensions %dx%d are outside 1..%dx1..%d", r.Width, r.Height, limits.MaxWidth, limits.MaxHeight)
	}
	pixels := uint64(r.Width) * uint64(r.Height)
	if pixels > limits.MaxPixels {
		return fmt.Errorf("pixel count %d exceeds %d", pixels, limits.MaxPixels)
	}
	return nil
}

// Store owns a private content-addressed image object directory.
type Store struct {
	root         string
	limits       Limits
	sourceReader *safeio.Reader
	objectReader *safeio.Reader
}

// NewStore opens or creates a private image object store at root.
func NewStore(root string, limits Limits) (*Store, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return nil, fmt.Errorf("resolve image object store: %w", err)
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("image object store root is empty")
	}
	if err := ensurePrivateDirectory(absolute); err != nil {
		return nil, fmt.Errorf("prepare image object store: %w", err)
	}
	// Pin the store to its canonical location. A caller may legitimately place
	// it below a symlinked system directory (for example, macOS /var), but later
	// object lookups must not re-traverse a mutable alias.
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve image object store symlinks: %w", err)
	}
	absolute = filepath.Clean(resolved)
	objects := filepath.Join(absolute, "objects")
	if err := ensurePrivateDirectory(objects); err != nil {
		return nil, fmt.Errorf("prepare image objects directory: %w", err)
	}
	return &Store{
		root:         absolute,
		limits:       limits,
		sourceReader: safeio.NewReader(),
		objectReader: safeio.NewReader(),
	}, nil
}

// AdmitFile validates an explicitly selected regular file, copies it into the
// private content-addressed store, and returns a path-free reference. Explicit
// file selection follows a source symlink; the opened descriptor is still
// validated as a regular file by safeio.
func (s *Store) AdmitFile(ctx context.Context, path string) (Ref, error) {
	return s.AdmitFileChecked(ctx, path, nil)
}

// AdmitFileChecked validates an explicitly selected image and invokes check
// with its path-free reference before publishing bytes to the private object
// store. A rejected check leaves no new object behind. The check must be
// deterministic and must not retain the reference beyond the call.
func (s *Store) AdmitFileChecked(ctx context.Context, path string, check func(Ref) error) (Ref, error) {
	if err := s.ready(); err != nil {
		return Ref{}, err
	}
	if err := contextErr(ctx); err != nil {
		return Ref{}, err
	}
	data, err := s.sourceReader.ReadRegularFile(path, s.limits.MaxBytes, safeio.StartupReadTimeout)
	if err != nil {
		return Ref{}, fmt.Errorf("read image input: %w", err)
	}
	return s.AdmitBytesChecked(ctx, filepath.Base(path), data, check)
}

// AdmitBytes validates an in-memory image (for example, a future native
// clipboard adapter), stores it privately, and returns a durable reference.
func (s *Store) AdmitBytes(ctx context.Context, displayName string, data []byte) (Ref, error) {
	return s.AdmitBytesChecked(ctx, displayName, data, nil)
}

// AdmitBytesChecked validates in-memory image bytes and invokes check with the
// path-free reference before publication. A rejected check leaves no new
// object behind.
func (s *Store) AdmitBytesChecked(ctx context.Context, displayName string, data []byte, check func(Ref) error) (Ref, error) {
	if err := s.ready(); err != nil {
		return Ref{}, err
	}
	if err := contextErr(ctx); err != nil {
		return Ref{}, err
	}
	ref, err := inspect(displayName, data, s.limits)
	if err != nil {
		return Ref{}, err
	}
	if check != nil {
		if err := check(ref); err != nil {
			return Ref{}, err
		}
	}
	if err := s.publish(ctx, ref.Digest, data); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// Load reads an admitted object without following symlinks and verifies its
// complete digest and image metadata before returning bytes to a trusted
// provider adapter.
func (s *Store) Load(ctx context.Context, ref Ref) ([]byte, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	// Stores may opt into stricter provider-specific limits. Apply those before
	// deriving a path or reading any bytes.
	if err := ref.validate(s.limits); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReference, err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	path := s.objectPath(ref.Digest)
	data, err := s.objectReader.ReadPrivateRegularFileNoFollow(path, s.limits.MaxBytes, safeio.StartupReadTimeout)
	if err != nil {
		return nil, fmt.Errorf("read image object %s: %w", ref.Handle(), err)
	}
	actual, err := inspect(ref.Name, data, s.limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrIntegrity, ref.Handle(), err)
	}
	if actual.Digest != ref.Digest ||
		(ref.MIMEType != "" && actual.MIMEType != ref.MIMEType) ||
		(ref.SizeBytes != 0 && actual.SizeBytes != ref.SizeBytes) ||
		(ref.Width != 0 && actual.Width != ref.Width) ||
		(ref.Height != 0 && actual.Height != ref.Height) {
		return nil, fmt.Errorf("%w: metadata mismatch for %s", ErrIntegrity, ref.Handle())
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Store) ready() error {
	if s == nil || s.sourceReader == nil || s.objectReader == nil || s.root == "" {
		return fmt.Errorf("image object store is not initialized")
	}
	return s.limits.validate()
}

func (s *Store) publish(ctx context.Context, digest string, data []byte) error {
	if err := validateDigest(digest); err != nil {
		return err
	}
	bucket := filepath.Join(s.root, "objects", digest[:2])
	if err := ensurePrivateDirectory(bucket); err != nil {
		return fmt.Errorf("prepare image object bucket: %w", err)
	}
	destination := s.objectPath(digest)
	if _, err := os.Lstat(destination); err == nil {
		return s.verifyExisting(ctx, digest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect image object destination: %w", err)
	}

	temporary, err := os.CreateTemp(bucket, ".incoming-*")
	if err != nil {
		return fmt.Errorf("create temporary image object: %w", err)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary image object: %w", err)
	}
	if err := writeAll(ctx, temporary, data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary image object: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary image object: %w", err)
	}

	// Linking a complete temporary file publishes without overwriting an object
	// won by another process. Both paths are in the same private directory.
	if err := os.Link(temporaryPath, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publish image object: %w", err)
		}
		if err := s.verifyExisting(ctx, digest); err != nil {
			return err
		}
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("remove temporary image object: %w", err)
	}
	removeTemporary = false
	return nil
}

func (s *Store) verifyExisting(ctx context.Context, digest string) error {
	data, err := s.objectReader.ReadPrivateRegularFileNoFollow(s.objectPath(digest), s.limits.MaxBytes, safeio.StartupReadTimeout)
	if err != nil {
		return fmt.Errorf("verify existing image object: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != digest {
		return fmt.Errorf("%w: stored object digest mismatch", ErrIntegrity)
	}
	return nil
}

func (s *Store) objectPath(digest string) string {
	return filepath.Join(s.root, "objects", digest[:2], digest)
}

func inspect(displayName string, data []byte, limits Limits) (Ref, error) {
	if err := limits.validate(); err != nil {
		return Ref{}, err
	}
	if int64(len(data)) > limits.MaxBytes {
		return Ref{}, fmt.Errorf("%w: image is %d bytes (limit %d)", safeio.ErrTooLarge, len(data), limits.MaxBytes)
	}
	if len(data) == 0 {
		return Ref{}, fmt.Errorf("decode image header: empty input")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Ref{}, fmt.Errorf("decode image header: %w", err)
	}
	mimeType, ok := supportedMIME(format)
	if !ok {
		return Ref{}, fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > limits.MaxWidth || config.Height > limits.MaxHeight {
		return Ref{}, fmt.Errorf("%w: %dx%d (limits %dx%d)", ErrInvalidDimensions, config.Width, config.Height, limits.MaxWidth, limits.MaxHeight)
	}
	pixels := uint64(config.Width) * uint64(config.Height)
	if pixels > limits.MaxPixels {
		return Ref{}, fmt.Errorf("%w: %d pixels (limit %d)", ErrInvalidDimensions, pixels, limits.MaxPixels)
	}
	if format == "gif" {
		if err := preflightGIFFrames(data, config.Width, config.Height, limits); err != nil {
			return Ref{}, fmt.Errorf("decode image content: %w", err)
		}
		animation, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return Ref{}, fmt.Errorf("decode image content: %w", err)
		}
		if len(animation.Image) == 0 || animation.Config.Width != config.Width || animation.Config.Height != config.Height {
			return Ref{}, fmt.Errorf("decode image content: header/content mismatch")
		}
		var decodedPixels uint64
		canvas := image.Rect(0, 0, config.Width, config.Height)
		for _, frame := range animation.Image {
			bounds := frame.Bounds()
			if !bounds.In(canvas) || bounds.Empty() {
				return Ref{}, fmt.Errorf("decode image content: animation frame is outside the canvas")
			}
			framePixels := uint64(bounds.Dx()) * uint64(bounds.Dy())
			if framePixels > limits.MaxPixels-decodedPixels {
				return Ref{}, fmt.Errorf("%w: animated image exceeds %d decoded frame pixels", ErrInvalidDimensions, limits.MaxPixels)
			}
			decodedPixels += framePixels
		}
	} else {
		decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return Ref{}, fmt.Errorf("decode image content: %w", err)
		}
		decodedBounds := decoded.Bounds()
		if decodedFormat != format || decodedBounds.Dx() != config.Width || decodedBounds.Dy() != config.Height {
			return Ref{}, fmt.Errorf("decode image content: header/content mismatch")
		}
	}
	digest := sha256.Sum256(data)
	ref := Ref{
		Digest:    hex.EncodeToString(digest[:]),
		MIMEType:  mimeType,
		Name:      sanitizeDisplayName(displayName, format),
		SizeBytes: int64(len(data)),
		Width:     config.Width,
		Height:    config.Height,
	}
	if err := ref.validate(limits); err != nil {
		return Ref{}, fmt.Errorf("construct image reference: %w", err)
	}
	return ref, nil
}

// preflightGIFFrames walks GIF block framing before gif.DecodeAll can allocate
// one pixel buffer per frame. It validates every image rectangle and applies
// the aggregate decoded-pixel budget using only the bounded compressed input.
func preflightGIFFrames(data []byte, canvasWidth, canvasHeight int, limits Limits) error {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return fmt.Errorf("invalid GIF header")
	}
	if int(binary.LittleEndian.Uint16(data[6:8])) != canvasWidth || int(binary.LittleEndian.Uint16(data[8:10])) != canvasHeight {
		return fmt.Errorf("header/content mismatch")
	}
	position := 13
	if data[10]&0x80 != 0 {
		position += gifColorTableBytes(data[10])
		if position > len(data) {
			return io.ErrUnexpectedEOF
		}
	}
	var decodedPixels uint64
	frames := 0
	for {
		if position >= len(data) {
			return io.ErrUnexpectedEOF
		}
		marker := data[position]
		position++
		switch marker {
		case 0x3b: // trailer
			if frames == 0 {
				return fmt.Errorf("animation has no image frame")
			}
			return nil
		case 0x21: // extension
			if position >= len(data) {
				return io.ErrUnexpectedEOF
			}
			position++ // extension label
			next, err := skipGIFSubBlocks(data, position)
			if err != nil {
				return err
			}
			position = next
		case 0x2c: // image descriptor
			if len(data)-position < 9 {
				return io.ErrUnexpectedEOF
			}
			left := int(binary.LittleEndian.Uint16(data[position : position+2]))
			top := int(binary.LittleEndian.Uint16(data[position+2 : position+4]))
			width := int(binary.LittleEndian.Uint16(data[position+4 : position+6]))
			height := int(binary.LittleEndian.Uint16(data[position+6 : position+8]))
			packed := data[position+8]
			position += 9
			if width <= 0 || height <= 0 || width > limits.MaxWidth || height > limits.MaxHeight || left < 0 || top < 0 || left+width > canvasWidth || top+height > canvasHeight {
				return fmt.Errorf("animation frame is outside the canvas")
			}
			framePixels := uint64(width) * uint64(height)
			if framePixels > limits.MaxPixels-decodedPixels {
				return fmt.Errorf("%w: animated image exceeds %d decoded frame pixels", ErrInvalidDimensions, limits.MaxPixels)
			}
			decodedPixels += framePixels
			frames++
			if packed&0x80 != 0 {
				position += gifColorTableBytes(packed)
				if position > len(data) {
					return io.ErrUnexpectedEOF
				}
			}
			if position >= len(data) {
				return io.ErrUnexpectedEOF
			}
			position++ // LZW minimum code size
			next, err := skipGIFSubBlocks(data, position)
			if err != nil {
				return err
			}
			position = next
		default:
			return fmt.Errorf("unexpected GIF block marker 0x%02x", marker)
		}
	}
}

func gifColorTableBytes(packed byte) int {
	return 3 * (1 << ((packed & 0x07) + 1))
}

func skipGIFSubBlocks(data []byte, position int) (int, error) {
	for {
		if position >= len(data) {
			return 0, io.ErrUnexpectedEOF
		}
		size := int(data[position])
		position++
		if size == 0 {
			return position, nil
		}
		if size > len(data)-position {
			return 0, io.ErrUnexpectedEOF
		}
		position += size
	}
}

func supportedMIME(format string) (string, bool) {
	switch strings.ToLower(format) {
	case "png":
		return "image/png", true
	case "jpeg":
		return "image/jpeg", true
	case "gif":
		return "image/gif", true
	default:
		return "", false
	}
}

func validReferenceMIME(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/gif":
		return true
	default:
		return false
	}
}

func validDisplayName(name string) bool {
	if name == "" || name == "." || name == ".." || name != strings.TrimSpace(name) || !utf8.ValidString(name) {
		return false
	}
	if len([]rune(name)) > maxDisplayNameRunes || strings.ContainsAny(name, `/\`) {
		return false
	}
	if strings.Join(strings.Fields(name), " ") != name {
		return false
	}
	for _, r := range name {
		if unsafeDisplayNameRune(r) {
			return false
		}
	}
	return true
}

func unsafeDisplayNameRune(r rune) bool {
	return r == utf8.RuneError || unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control)
}

func sanitizeDisplayName(name, format string) string {
	// Treat both common path separators as separators regardless of the host OS
	// so a persisted display name can never retain a foreign-platform path.
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if unsafeDisplayNameRune(r) {
			return -1
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		name = "image." + format
	}
	runes := []rune(name)
	if len(runes) > maxDisplayNameRunes {
		name = string(runes[:maxDisplayNameRunes])
	}
	return name
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", safeio.ErrSymlink, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s (%s)", safeio.ErrNotRegular, path, info.Mode().Type())
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return nil
}

func validateDigest(value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("invalid image object SHA-256 digest")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("invalid image object SHA-256 digest")
	}
	return nil
}

func writeAll(ctx context.Context, writer io.Writer, data []byte) error {
	const chunkSize = 64 << 10
	for offset := 0; offset < len(data); {
		if err := contextErr(ctx); err != nil {
			return err
		}
		end := min(offset+chunkSize, len(data))
		written, err := writer.Write(data[offset:end])
		if err != nil {
			return fmt.Errorf("write temporary image object: %w", err)
		}
		if written == 0 {
			return fmt.Errorf("write temporary image object: %w", io.ErrShortWrite)
		}
		offset += written
	}
	return nil
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("image operation context is nil")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
