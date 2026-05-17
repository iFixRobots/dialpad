package connector

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// makeSyntheticPNG builds a w×h PNG with deterministic high-entropy pixel
// values (LCG output) so the PNG encoder can't deflate it to near zero bytes.
func makeSyntheticPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	state := uint32(2463534242)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			img.Set(x, y, color.RGBA{
				uint8(state),
				uint8(state >> 8),
				uint8(state >> 16),
				255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode synthetic png: %v", err)
	}
	return buf.Bytes()
}

func TestCompressImageForMMS_LeavesSmallImagesValid(t *testing.T) {
	data := makeSyntheticPNG(t, 32, 32)
	out, err := compressImageForMMS(data, 4*1024*1024)
	if err != nil {
		t.Fatalf("compress small image: %v", err)
	}
	if len(out) == 0 {
		t.Error("compressed output is empty")
	}
	if _, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("compressed output not a valid image: %v", err)
	}
}

func TestCompressImageForMMS_ShrinksOversized(t *testing.T) {
	data := makeSyntheticPNG(t, 600, 600)
	// Pick a cap below the input PNG size so we know compression must do work.
	if len(data) < 8*1024 {
		t.Fatalf("synthetic input too small (%d bytes) to exercise compression", len(data))
	}
	maxSize := len(data) / 4
	out, err := compressImageForMMS(data, maxSize)
	if err != nil {
		t.Fatalf("compress oversized image (in=%d, cap=%d): %v", len(data), maxSize, err)
	}
	if len(out) > maxSize {
		t.Errorf("compressed output %d bytes > cap %d", len(out), maxSize)
	}
	if _, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("compressed output not a valid image: %v", err)
	}
}

func TestCompressImageForMMS_RejectsImpossiblyTightBudget(t *testing.T) {
	data := makeSyntheticPNG(t, 4000, 4000)
	// 256 bytes is well below the minimum-quality / 30%-scale floor for any
	// real image, so compressImageForMMS should give up rather than return
	// a corrupted blob.
	if _, err := compressImageForMMS(data, 256); err == nil {
		t.Error("expected error when target size is unachievable")
	}
}

func TestCompressImageForMMS_RejectsInvalidInput(t *testing.T) {
	if _, err := compressImageForMMS([]byte("not an image"), 1024*1024); err == nil {
		t.Error("expected decode error on garbage input")
	}
}

// Locks the rule for received-MMS filename derivation. Inbound voice notes
// were arriving as "attachment" with no extension because uploadMediaToMatrix
// previously hardcoded Body="attachment" and never set FileName — this test
// guards the fallback used when MMSDetails is missing.
func TestDefaultMediaFilename(t *testing.T) {
	cases := []struct {
		mime string
		want string
	}{
		{"image/jpeg", "image.jpg"},
		{"image/png", "image.png"},
		{"image/gif", "image.gif"},
		{"image/webp", "image"},
		{"video/mp4", "video.mp4"},
		{"video/quicktime", "video"},
		{"audio/mpeg", "audio.mp3"},
		{"audio/ogg", "audio"},
		{"application/octet-stream", "attachment"},
		{"", "attachment"},
	}
	for _, tc := range cases {
		if got := defaultMediaFilename(tc.mime); got != tc.want {
			t.Errorf("defaultMediaFilename(%q) = %q; want %q", tc.mime, got, tc.want)
		}
	}
}

// Locks the carrier-dimension recompress rule. A small-byte but
// huge-pixel-dimension image (e.g. an efficient iPhone JPEG at 22 MP) used
// to ship full-res because the recompress gate only checked file size; the
// carrier then dropped the MMS silently. imageNeedsRecompress should now
// flag those inputs even when bytes fit, and the resulting compressed
// image should have its longest side at or below maxMMSImageDimension.
func TestImageNeedsRecompress(t *testing.T) {
	smallPNG := makeSyntheticPNG(t, 64, 64) // tiny, well under any sane cap
	largeBytesPNG := makeSyntheticPNG(t, 600, 600)
	hugeDimsPNG := makeSyntheticPNG(t, 2400, 1800) // dims > 1600 cap

	cases := []struct {
		name string
		data []byte
		max  int
		want bool
	}{
		{"under both caps — no recompress", smallPNG, 8 * 1024 * 1024, false},
		{"over byte cap — recompress", largeBytesPNG, 256, true},
		{"under byte cap but over dimension cap — recompress", hugeDimsPNG, 100 * 1024 * 1024, true},
		{"garbage input — header unreadable, don't force recompress", []byte("not an image"), 1024, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageNeedsRecompress(tc.data, tc.max); got != tc.want {
				t.Errorf("imageNeedsRecompress = %v; want %v (input %d bytes, cap %d)",
					got, tc.want, len(tc.data), tc.max)
			}
		})
	}
}

// End-to-end: an image with carrier-rejecting dimensions (2400x1800) but a
// file size that fits the wire cap must come back from compressImageForMMS
// scaled down to ≤ maxMMSImageDimension on the longest side.
func TestCompressImageForMMS_ScalesDownLargeDimensions(t *testing.T) {
	data := makeSyntheticPNG(t, 2400, 1800)
	out, err := compressImageForMMS(data, 100*1024*1024) // huge budget so size doesn't drive it
	if err != nil {
		t.Fatalf("compress large-dimension image: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode config of output: %v", err)
	}
	if cfg.Width > maxMMSImageDimension || cfg.Height > maxMMSImageDimension {
		t.Errorf("output dims %dx%d exceed carrier cap %d", cfg.Width, cfg.Height, maxMMSImageDimension)
	}
}
