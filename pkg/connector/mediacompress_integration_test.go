package connector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.mau.fi/util/ffmpeg"
)

// These tests invoke real ffmpeg/ffprobe. They skip cleanly when those
// binaries aren't on $PATH, so `go test ./...` works for contributors
// without an ffmpeg install. When ffmpeg IS present, they verify the end-
// to-end path: synthesised oversized input → real compressor → output is
// both under the size cap AND a valid container ffprobe can re-read.

func skipIfNoFFmpeg(t *testing.T) {
	t.Helper()
	if !ffmpeg.Supported() || !ffmpeg.ProbeSupported() {
		t.Skip("ffmpeg/ffprobe not in PATH — skipping integration test")
	}
}

// genFixture shells out to ffmpeg with the given output args to produce a
// synthetic media file in a temp path. Returns the file's bytes.
func genFixture(t *testing.T, ext string, args ...string) []byte {
	t.Helper()
	dir, err := os.MkdirTemp("", "dialpad_fixture_*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "fixture"+ext)
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	full = append(full, path)
	cmd := exec.Command("ffmpeg", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture gen failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// probeDuration runs ffprobe against bytes via a temp file and returns the
// container duration. Used to verify compressed output is still a valid
// playable file (and the right length).
func probeDuration(t *testing.T, data []byte, ext string) float64 {
	t.Helper()
	dir, err := os.MkdirTemp("", "dialpad_probe_*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "probe"+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write probe input: %v", err)
	}
	res, err := ffmpeg.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe failed (output likely not a valid container): %v", err)
	}
	if res == nil || res.Format == nil {
		t.Fatal("probe returned no format")
	}
	return res.Format.Duration
}

// Real ffmpeg end-to-end: take an oversized 720p video, compress it, assert
// the result fits under maxMMSSize AND is still playable + roughly the same
// duration.
func TestCompressVideoForMMS_RealFFmpeg(t *testing.T) {
	skipIfNoFFmpeg(t)

	// 15s of 720p test pattern @ 30fps with high-bitrate H.264 → ~3-6 MB,
	// well over the 2 MiB cap.
	input := genFixture(t, ".mp4",
		"-f", "lavfi", "-i", "testsrc=duration=15:size=1280x720:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=15",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "4000k",
		"-c:a", "aac", "-b:a", "128k",
		"-pix_fmt", "yuv420p",
		"-shortest",
	)
	if len(input) <= maxMMSSize {
		t.Fatalf("test fixture too small (%d bytes) — needs to exceed %d to exercise compression",
			len(input), maxMMSSize)
	}

	out, err := compressVideoForMMS(context.Background(), ffmpegTranscoder{}, input, "video/mp4", maxMMSSize)
	if err != nil {
		t.Fatalf("compressVideoForMMS failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("compressor returned empty output")
	}
	if len(out) > maxMMSSize {
		t.Errorf("compressed output %d bytes > maxMMSSize %d — bitrate math or args wrong",
			len(out), maxMMSSize)
	}
	t.Logf("video: %d bytes → %d bytes (%.0f%% reduction)",
		len(input), len(out), 100*(1-float64(len(out))/float64(len(input))))

	// Verify it's still a valid playable container of roughly the right length.
	duration := probeDuration(t, out, ".mp4")
	if duration < 14 || duration > 16 {
		t.Errorf("output duration %.2fs; want ~15s (compression should preserve length)", duration)
	}
}

// Real ffmpeg end-to-end: take an oversized PCM-style audio fixture,
// compress to AAC, assert it fits under maxMMSSize and is playable.
func TestCompressAudioForMMS_RealFFmpeg(t *testing.T) {
	skipIfNoFFmpeg(t)

	// 60s of stereo 48kHz PCM in WAV → ~11.5 MB, far over the 2 MiB cap.
	input := genFixture(t, ".wav",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=60",
		"-ac", "2", "-ar", "48000",
		"-c:a", "pcm_s16le",
	)
	if len(input) <= maxMMSSize {
		t.Fatalf("audio fixture too small (%d bytes) — needs to exceed %d",
			len(input), maxMMSSize)
	}

	out, err := compressAudioForMMS(context.Background(), ffmpegTranscoder{}, input, "audio/wav", maxMMSSize)
	if err != nil {
		t.Fatalf("compressAudioForMMS failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("compressor returned empty output")
	}
	if len(out) > maxMMSSize {
		t.Errorf("compressed output %d bytes > maxMMSSize %d", len(out), maxMMSSize)
	}
	t.Logf("audio: %d bytes → %d bytes (%.0f%% reduction)",
		len(input), len(out), 100*(1-float64(len(out))/float64(len(input))))

	duration := probeDuration(t, out, ".m4a")
	if duration < 58 || duration > 62 {
		t.Errorf("output duration %.2fs; want ~60s", duration)
	}
}

// Real-ffmpeg verification that ffmpegTranscoder.probe correctly extracts
// duration from a known-length fixture. If this passes, the mock-driven
// dispatch tests above mean the real production path produces the right
// bitrate targets too.
func TestFFmpegTranscoder_ProbeRoundTrip(t *testing.T) {
	skipIfNoFFmpeg(t)

	input := genFixture(t, ".m4a",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=7",
		"-c:a", "aac", "-b:a", "96k",
	)
	tr := ffmpegTranscoder{}
	duration, err := tr.probe(context.Background(), input, "audio/mp4")
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if duration < 6.5 || duration > 7.5 {
		t.Errorf("probed duration %.2fs; want ~7s", duration)
	}
}

// Voice-note shape: short OGG/Opus input, transcode to AAC, verify the
// output is playable and renamed appropriately. This is the case Beeper
// voice notes hit in production.
func TestCompressAudioForMMS_VoiceNoteShape(t *testing.T) {
	skipIfNoFFmpeg(t)

	// 8s OGG/Opus voice note (~50 KB) — already well under 2 MiB. But the
	// transcode path still runs end-to-end if Beeper records ogg and we
	// route via the audio/ogg case. Use a larger duration to force the
	// compression branch when len(data) exceeds the cap.
	input := genFixture(t, ".ogg",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=8",
		"-c:a", "libopus", "-b:a", "64k",
	)
	tr := ffmpegTranscoder{}

	// Direct call — exercises the real ffmpeg convert path even though this
	// input is under-cap (handlematrix.go's gate is len(data)>maxMMSSize;
	// the helpers themselves don't gate on size, only on duration).
	out, err := compressAudioForMMS(context.Background(), tr, input, "audio/ogg", maxMMSSize)
	if err != nil {
		t.Fatalf("voice-note compress failed: %v", err)
	}
	dur := probeDuration(t, out, ".m4a")
	if dur < 7.5 || dur > 8.5 {
		t.Errorf("voice-note output duration %.2fs; want ~8s", dur)
	}
	if len(out) > maxMMSSize {
		t.Errorf("voice-note output %d bytes > maxMMSSize", len(out))
	}
}
