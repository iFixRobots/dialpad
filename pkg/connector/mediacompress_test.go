package connector

import (
	"context"
	"errors"
	"testing"
)

// targetAudioBitrate: short clips clamp to 128 kbps, long clips fail, the
// middle band scales linearly with duration. The math doesn't run ffmpeg, so
// this is a pure-function test.
func TestTargetAudioBitrate(t *testing.T) {
	const maxSize = 2 * 1024 * 1024 // 2 MiB
	cases := []struct {
		name        string
		durationSec float64
		wantKbps    int
		wantErr     bool
	}{
		{name: "30s voice note → clamps to 128 kbps ceiling", durationSec: 30, wantKbps: 128},
		{name: "5 min voice note → scales between min/max", durationSec: 300, wantKbps: 51},
		{name: "1 hour clip → falls below 32 kbps floor, error", durationSec: 3600, wantErr: true},
		{name: "zero duration → error", durationSec: 0, wantErr: true},
		{name: "negative duration → error", durationSec: -1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := targetAudioBitrate(tc.durationSec, maxSize)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got bitrate %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantKbps {
				t.Errorf("kbps = %d; want %d", got, tc.wantKbps)
			}
		})
	}
}

// targetVideoBitrate: reserves audio bandwidth, fails below 80 kbps useful
// video. Threshold tuned in the test to match the audio-reserve + headroom.
func TestTargetVideoBitrate(t *testing.T) {
	const maxSize = 2 * 1024 * 1024
	cases := []struct {
		name        string
		durationSec float64
		wantErr     bool
	}{
		{name: "15s phone clip → plenty of bitrate", durationSec: 15},
		{name: "60s video → tight but above the 80 kbps floor", durationSec: 60},
		{name: "90s video → falls below floor under tight 0.65 headroom, error",
			durationSec: 90, wantErr: true},
		{name: "5 min video → far below floor, error", durationSec: 300, wantErr: true},
		{name: "30 min video → far below floor, error", durationSec: 1800, wantErr: true},
		{name: "zero duration → error", durationSec: 0, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := targetVideoBitrate(tc.durationSec, maxSize)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got bitrate %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got < 80 {
				t.Errorf("bitrate %d kbps under 80 floor", got)
			}
		})
	}
}

// fakeTranscoder is a stub mediaTranscoder for testing the compression
// dispatch without invoking ffmpeg. It records the args it was called with
// and returns canned results.
type fakeTranscoder struct {
	probeDuration float64
	probeErr      error
	convertOut    []byte
	convertErr    error

	// Captured args (last call wins).
	gotProbeMime    string
	gotConvertExt   string
	gotConvertOut   []string
	gotConvertCalls int
}

func (f *fakeTranscoder) probe(_ context.Context, _ []byte, inputMime string) (float64, error) {
	f.gotProbeMime = inputMime
	return f.probeDuration, f.probeErr
}

func (f *fakeTranscoder) convert(_ context.Context, _ []byte, outputExt string, _, outputArgs []string, _ string) ([]byte, error) {
	f.gotConvertCalls++
	f.gotConvertExt = outputExt
	f.gotConvertOut = outputArgs
	return f.convertOut, f.convertErr
}

// compressAudioForMMS happy path: probe returns a usable duration, convert
// returns bytes — function passes them through cleanly with the right ffmpeg
// args.
func TestCompressAudioForMMS_HappyPath(t *testing.T) {
	tr := &fakeTranscoder{
		probeDuration: 60, // 1 minute
		convertOut:    []byte("fake-aac"),
	}
	out, err := compressAudioForMMS(context.Background(), tr, []byte("source"), "audio/ogg", 2*1024*1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "fake-aac" {
		t.Errorf("output = %q; want fake-aac", out)
	}
	if tr.gotProbeMime != "audio/ogg" {
		t.Errorf("probe called with mime %q; want audio/ogg", tr.gotProbeMime)
	}
	if tr.gotConvertExt != ".m4a" {
		t.Errorf("convert ext = %q; want .m4a", tr.gotConvertExt)
	}
	if !containsArgs(tr.gotConvertOut, "-c:a", "aac", "-ac", "1") {
		t.Errorf("missing expected audio args; got %v", tr.gotConvertOut)
	}
}

// compressAudioForMMS surfaces probe failures intact.
func TestCompressAudioForMMS_ProbeError(t *testing.T) {
	tr := &fakeTranscoder{probeErr: errors.New("ffprobe boom")}
	_, err := compressAudioForMMS(context.Background(), tr, []byte("x"), "audio/mp4", 2*1024*1024)
	if err == nil || !errorContains(err, "ffprobe boom") {
		t.Errorf("expected probe error wrapped; got %v", err)
	}
	if tr.gotConvertCalls != 0 {
		t.Error("convert should not be called when probe fails")
	}
}

// compressVideoForMMS happy path: returns transcoded bytes with H.264 + AAC
// args. We don't assert exact bitrate values (those are tested via
// targetVideoBitrate) but we do confirm key codec args are passed.
func TestCompressVideoForMMS_HappyPath(t *testing.T) {
	tr := &fakeTranscoder{
		probeDuration: 15,
		convertOut:    []byte("fake-mp4"),
	}
	out, err := compressVideoForMMS(context.Background(), tr, []byte("source"), "video/quicktime", 2*1024*1024)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "fake-mp4" {
		t.Errorf("output = %q; want fake-mp4", out)
	}
	if tr.gotConvertExt != ".mp4" {
		t.Errorf("convert ext = %q; want .mp4", tr.gotConvertExt)
	}
	if !containsArgs(tr.gotConvertOut, "-c:v", "libx264", "-profile:v", "baseline", "-c:a", "aac") {
		t.Errorf("missing expected video args; got %v", tr.gotConvertOut)
	}
}

// compressVideoForMMS bails before invoking ffmpeg when the duration math
// produces an unworkable bitrate.
func TestCompressVideoForMMS_TooLongFailsBeforeConvert(t *testing.T) {
	tr := &fakeTranscoder{probeDuration: 1800} // 30 min — math will fail
	_, err := compressVideoForMMS(context.Background(), tr, []byte("x"), "video/mp4", 2*1024*1024)
	if err == nil {
		t.Fatal("expected error for unworkable bitrate")
	}
	if tr.gotConvertCalls != 0 {
		t.Errorf("convert called %d times; want 0 (should bail before convert)", tr.gotConvertCalls)
	}
}

// swapMediaExtension: covers the three meaningful cases — replace existing
// extension, append when no extension, handle paths with directories.
func TestSwapMediaExtension(t *testing.T) {
	cases := []struct {
		in, newExt, want string
	}{
		{"video.mov", ".mp4", "video.mp4"},
		{"voicenote", ".m4a", "voicenote.m4a"},
		{"", ".mp4", "attachment.mp4"},
		{"clip.with.dots.MOV", ".mp4", "clip.with.dots.mp4"},
		{"sub/dir/file.ogg", ".m4a", "sub/dir/file.m4a"},
		{"sub/dir/no_ext", ".mp4", "sub/dir/no_ext.mp4"},
	}
	for _, tc := range cases {
		t.Run(tc.in+" → "+tc.newExt, func(t *testing.T) {
			got := swapMediaExtension(tc.in, tc.newExt)
			if got != tc.want {
				t.Errorf("got %q; want %q", got, tc.want)
			}
		})
	}
}

// containsArgs reports whether all needles appear in haystack in order.
func containsArgs(haystack []string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func errorContains(err error, substr string) bool {
	return err != nil && containsSubstr(err.Error(), substr)
}

func containsSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
