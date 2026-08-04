package imageasset

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestAdmitBytesStoresPrivateContentAddressedImage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "assets")
	store, err := NewStore(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	data := encodePNG(t, 12, 9)
	ref, err := store.AdmitBytes(context.Background(), "/private/screenshots/\x00checkout.png", data)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	wantDigest := hex.EncodeToString(digest[:])
	if ref.Digest != wantDigest || ref.MIMEType != "image/png" || ref.Width != 12 || ref.Height != 9 || ref.SizeBytes != int64(len(data)) {
		t.Fatalf("unexpected ref: %#v", ref)
	}
	if ref.Name != "checkout.png" {
		t.Fatalf("name = %q, want sanitized basename", ref.Name)
	}
	if ref.Handle() != "img-"+wantDigest[:12] {
		t.Fatalf("handle = %q", ref.Handle())
	}
	encodedRef, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"/private/screenshots", root, string(data)} {
		if strings.Contains(string(encodedRef), secret) {
			t.Fatalf("persisted reference leaked path or raw bytes: %s", encodedRef)
		}
	}

	objectPath := filepath.Join(root, "objects", wantDigest[:2], wantDigest)
	info, err := os.Stat(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("object permissions = %o, want 600", info.Mode().Perm())
	}
	for _, directory := range []string{root, filepath.Join(root, "objects"), filepath.Dir(objectPath)} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s permissions = %o, want 700", directory, info.Mode().Perm())
		}
	}

	loaded, err := store.Load(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, data) {
		t.Fatal("loaded bytes differ from admitted image")
	}

	second, err := store.AdmitBytes(context.Background(), "duplicate.png", data)
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest != ref.Digest || second.Name != "duplicate.png" {
		t.Fatalf("duplicate admission = %#v", second)
	}
}

func TestAdmitFileFollowsExplicitSourceAndDoesNotPersistPath(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "actual.png")
	if err := os.WriteFile(target, encodePNG(t, 3, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(directory, "selected.png")
	if err := os.Symlink(target, selected); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store, err := NewStore(filepath.Join(directory, "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.AdmitFile(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != "selected.png" {
		t.Fatalf("name = %q", ref.Name)
	}
	if strings.Contains(ref.Name, directory) {
		t.Fatalf("reference leaked source path: %#v", ref)
	}
}

func TestAdmitFileCheckedRejectsBeforePublishing(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "private-source.png")
	if err := os.WriteFile(source, encodePNG(t, 4, 3), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(directory, "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	rejected := errors.New("conversation budget rejected image")
	var inspected Ref
	_, err = store.AdmitFileChecked(context.Background(), source, func(ref Ref) error {
		inspected = ref
		if strings.Contains(ref.Name, directory) {
			t.Fatalf("pre-publish reference leaked source path: %#v", ref)
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatalf("checked admission error = %v, want %v", err, rejected)
	}
	if inspected.Digest == "" {
		t.Fatal("admission check did not receive the validated reference")
	}
	if _, err := os.Stat(store.objectPath(inspected.Digest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected checked admission published an object: %v", err)
	}
}

func TestAdmissionStripsBidirectionalControlsFromDisplayName(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.AdmitBytes(context.Background(), "screen\u202egnp.png", encodePNG(t, 3, 2))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != "screengnp.png" {
		t.Fatalf("name = %q, want bidi-safe basename", ref.Name)
	}
	if strings.IndexFunc(ref.Name, func(r rune) bool { return unicode.In(r, unicode.Bidi_Control) }) >= 0 {
		t.Fatalf("name retained bidirectional control: %q", ref.Name)
	}
	if err := ref.Validate(); err != nil {
		t.Fatalf("sanitized reference rejected: %v", err)
	}
}

func TestAdmissionRejectsOversizeDimensionsPixelsAndNonImages(t *testing.T) {
	data := encodePNG(t, 8, 6)
	tests := []struct {
		name   string
		limits Limits
		data   []byte
		is     error
	}{
		{name: "bytes", limits: Limits{MaxBytes: int64(len(data) - 1), MaxWidth: 20, MaxHeight: 20, MaxPixels: 400}, data: data, is: safeio.ErrTooLarge},
		{name: "width", limits: Limits{MaxBytes: 1 << 20, MaxWidth: 7, MaxHeight: 20, MaxPixels: 400}, data: data, is: ErrInvalidDimensions},
		{name: "height", limits: Limits{MaxBytes: 1 << 20, MaxWidth: 20, MaxHeight: 5, MaxPixels: 400}, data: data, is: ErrInvalidDimensions},
		{name: "pixels", limits: Limits{MaxBytes: 1 << 20, MaxWidth: 20, MaxHeight: 20, MaxPixels: 47}, data: data, is: ErrInvalidDimensions},
		{name: "not-image", limits: DefaultLimits(), data: []byte("not an image")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(filepath.Join(t.TempDir(), "store"), test.limits)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.AdmitBytes(context.Background(), "input.png", test.data)
			if err == nil {
				t.Fatal("expected admission error")
			}
			if test.is != nil && !errors.Is(err, test.is) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, test.is)
			}
		})
	}
}

func TestAdmissionRecognizesImageContentNotExtension(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 5, 4)), nil); err != nil {
		t.Fatal(err)
	}
	ref, err := store.AdmitBytes(context.Background(), "looks-like.txt", jpegData.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if ref.MIMEType != "image/jpeg" || ref.Name != "looks-like.txt" {
		t.Fatalf("unexpected JPEG ref: %#v", ref)
	}
}

func TestAdmissionRejectsTruncatedImageAfterValidHeader(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// A PNG signature plus complete IHDR is enough for DecodeConfig, but not a
	// complete image. Admission must validate more than dimensions and MIME.
	truncated := encodePNG(t, 5, 4)[:33]
	if _, _, err := image.DecodeConfig(bytes.NewReader(truncated)); err != nil {
		t.Fatalf("fixture must have a valid image header: %v", err)
	}
	if _, err := store.AdmitBytes(context.Background(), "truncated.png", truncated); err == nil || !strings.Contains(err.Error(), "decode image content") {
		t.Fatalf("truncated image error = %v", err)
	}
}

func TestAdmissionDecodesEveryGIFFrame(t *testing.T) {
	animation := testGIF(t, 8, 6, 2)
	var truncated []byte
	for cut := len(animation) - 1; cut > 16; cut-- {
		candidate := animation[:cut]
		if _, _, err := image.DecodeConfig(bytes.NewReader(candidate)); err != nil {
			continue
		}
		if _, _, err := image.Decode(bytes.NewReader(candidate)); err != nil {
			continue
		}
		if _, err := gif.DecodeAll(bytes.NewReader(candidate)); err != nil {
			truncated = append([]byte(nil), candidate...)
			break
		}
	}
	if len(truncated) == 0 {
		t.Fatal("could not construct a GIF with a valid first frame and truncated later frame")
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitBytes(context.Background(), "animated.gif", truncated); err == nil || !strings.Contains(err.Error(), "decode image content") {
		t.Fatalf("truncated animated GIF error = %v", err)
	}
}

func TestAdmissionBoundsCumulativeGIFFramePixels(t *testing.T) {
	data := testGIF(t, 8, 6, 2)
	limits := Limits{MaxBytes: 1 << 20, MaxWidth: 20, MaxHeight: 20, MaxPixels: 80}
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitBytes(context.Background(), "animated.gif", data); !errors.Is(err, ErrInvalidDimensions) {
		t.Fatalf("cumulative GIF frame error = %v", err)
	}
}

func TestGIFFrameBudgetIsCheckedBeforePixelDecode(t *testing.T) {
	// Build a structurally framed GIF with two full-canvas image descriptors
	// and intentionally invalid LZW data. The aggregate preflight must reject
	// it before gif.DecodeAll attempts to allocate/decode the frames.
	original := testGIF(t, 8, 6, 1)
	headerBytes := 13
	if original[10]&0x80 != 0 {
		headerBytes += gifColorTableBytes(original[10])
	}
	data := append([]byte(nil), original[:headerBytes]...)
	frame := []byte{0x2c, 0, 0, 0, 0, 8, 0, 6, 0, 0, 2, 1, 0, 0}
	data = append(data, frame...)
	data = append(data, frame...)
	data = append(data, 0x3b)
	limits := Limits{MaxBytes: 1 << 20, MaxWidth: 20, MaxHeight: 20, MaxPixels: 80}
	if err := preflightGIFFrames(data, 8, 6, limits); !errors.Is(err, ErrInvalidDimensions) {
		t.Fatalf("pre-decode GIF frame budget error = %v", err)
	}
}

func testGIF(t *testing.T, width, height, frames int) []byte {
	t.Helper()
	animation := &gif.GIF{Config: image.Config{ColorModel: color.Palette{color.Black, color.White}, Width: width, Height: height}}
	for index := 0; index < frames; index++ {
		frame := image.NewPaletted(image.Rect(0, 0, width, height), color.Palette{color.Black, color.White})
		for pixel := range frame.Pix {
			frame.Pix[pixel] = uint8((pixel + index) % 2)
		}
		animation.Image = append(animation.Image, frame)
		animation.Delay = append(animation.Delay, 1)
	}
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, animation); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestDisplayNameStripsForeignPlatformPath(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.AdmitBytes(context.Background(), `C:\Users\private\screen.png`, encodePNG(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name != "screen.png" {
		t.Fatalf("name = %q, want cross-platform basename", ref.Name)
	}
	fallback, err := store.AdmitBytes(context.Background(), "/", encodePNG(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if fallback.Name != "image.png" || fallback.Validate() != nil {
		t.Fatalf("unsafe basename fallback = %#v (validation: %v)", fallback, fallback.Validate())
	}
}

func TestLoadRejectsTamperingAndMetadataMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	store, err := NewStore(root, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.AdmitBytes(context.Background(), "a.png", encodePNG(t, 4, 3))
	if err != nil {
		t.Fatal(err)
	}

	wrongMetadata := ref
	wrongMetadata.Width++
	if _, err := store.Load(context.Background(), wrongMetadata); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("metadata mismatch error = %v", err)
	}

	objectPath := filepath.Join(root, "objects", ref.Digest[:2], ref.Digest)
	if err := os.WriteFile(objectPath, encodePNG(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), ref); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestStoreRejectsSymlinkRootAndObject(t *testing.T) {
	directory := t.TempDir()
	realRoot := filepath.Join(directory, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(directory, "linked")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NewStore(linkedRoot, DefaultLimits()); !errors.Is(err, safeio.ErrSymlink) {
		t.Fatalf("symlink root error = %v", err)
	}

	store, err := NewStore(filepath.Join(directory, "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	data := encodePNG(t, 3, 3)
	digest := sha256.Sum256(data)
	digestText := hex.EncodeToString(digest[:])
	bucket := filepath.Join(directory, "store", "objects", digestText[:2])
	if err := os.Mkdir(bucket, 0o700); err != nil {
		t.Fatal(err)
	}
	object := filepath.Join(bucket, digestText)
	if err := os.Symlink(filepath.Join(directory, "elsewhere"), object); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdmitBytes(context.Background(), "a.png", data); !errors.Is(err, safeio.ErrSymlink) {
		t.Fatalf("symlink object error = %v", err)
	}
}

func TestAdmissionHonorsCanceledContext(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.AdmitBytes(ctx, "a.png", encodePNG(t, 1, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRefValidateRejectsForgedPersistedMetadata(t *testing.T) {
	data := encodePNG(t, 4, 3)
	digest := sha256.Sum256(data)
	valid := Ref{
		Digest:    hex.EncodeToString(digest[:]),
		MIMEType:  "image/png",
		Name:      "screen shot.png",
		SizeBytes: int64(len(data)),
		Width:     4,
		Height:    3,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid reference rejected: %v", err)
	}

	overlongName := strings.Repeat("a", maxDisplayNameRunes+1)
	tests := []struct {
		name   string
		mutate func(*Ref)
	}{
		{name: "digest traversal", mutate: func(ref *Ref) { ref.Digest = strings.Repeat("a", 62) + ".." }},
		{name: "uppercase digest", mutate: func(ref *Ref) { ref.Digest = strings.ToUpper(ref.Digest) }},
		{name: "unsupported MIME", mutate: func(ref *Ref) { ref.MIMEType = "image/webp" }},
		{name: "parameterized MIME", mutate: func(ref *Ref) { ref.MIMEType = "image/png; charset=binary" }},
		{name: "empty name", mutate: func(ref *Ref) { ref.Name = "" }},
		{name: "parent name", mutate: func(ref *Ref) { ref.Name = ".." }},
		{name: "slash path", mutate: func(ref *Ref) { ref.Name = "private/screen.png" }},
		{name: "backslash path", mutate: func(ref *Ref) { ref.Name = `private\screen.png` }},
		{name: "control name", mutate: func(ref *Ref) { ref.Name = "screen\n.png" }},
		{name: "bidi control name", mutate: func(ref *Ref) { ref.Name = "screen\u202egnp.png" }},
		{name: "noncanonical whitespace", mutate: func(ref *Ref) { ref.Name = "screen  shot.png" }},
		{name: "overlong name", mutate: func(ref *Ref) { ref.Name = overlongName }},
		{name: "zero size", mutate: func(ref *Ref) { ref.SizeBytes = 0 }},
		{name: "negative size", mutate: func(ref *Ref) { ref.SizeBytes = -1 }},
		{name: "oversize", mutate: func(ref *Ref) { ref.SizeBytes = defaultMaxBytes + 1 }},
		{name: "zero width", mutate: func(ref *Ref) { ref.Width = 0 }},
		{name: "negative height", mutate: func(ref *Ref) { ref.Height = -1 }},
		{name: "overwide", mutate: func(ref *Ref) { ref.Width = defaultMaxWidth + 1 }},
		{name: "overtall", mutate: func(ref *Ref) { ref.Height = defaultMaxHeight + 1 }},
		{name: "too many pixels", mutate: func(ref *Ref) { ref.Width, ref.Height = 5000, 5000 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forged := valid
			test.mutate(&forged)
			if err := forged.Validate(); !errors.Is(err, ErrInvalidReference) {
				t.Fatalf("Validate error = %v, want ErrInvalidReference", err)
			}
		})
	}
}

func TestLoadRejectsForgedReferenceBeforeObjectLookup(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "store"), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	forged := Ref{
		Digest:    strings.Repeat("a", sha256.Size*2),
		MIMEType:  "text/plain",
		Name:      "screen.png",
		SizeBytes: 1,
		Width:     1,
		Height:    1,
	}
	if _, err := store.Load(context.Background(), forged); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("Load error = %v, want forged reference rejection", err)
	}
}

func TestStoreLimitsMayOnlyTightenDefaults(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPixels++
	if _, err := NewStore(filepath.Join(t.TempDir(), "store"), limits); err == nil {
		t.Fatal("expected larger-than-default limits to be rejected")
	}
}

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
