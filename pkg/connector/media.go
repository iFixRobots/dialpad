package connector

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/util/exmime"
	"go.mau.fi/util/ffmpeg"
	"golang.org/x/image/draw"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

// maxMMSSize is the upstream MMS upload ceiling for Dialpad's internal
// /api/upload_file/ endpoint — empirically validated at 2 MiB. (Dialpad's
// UI shows a 500 KiB reliability recommendation, but it's not a hard cap
// and below 2 MiB the upload endpoint accepts.) Used as the recompression
// target for outbound media. NOT what we advertise to the Matrix client —
// see the maxIngest* constants below.
const maxMMSSize = 2 * 1024 * 1024

// maxIngest{Image,Video,Audio} are the caps advertised to the Matrix client
// via GetCapabilities. They're larger than maxMMSSize because the bridge
// recompresses oversized inputs down to fit the wire cap. Without this
// two-tier shape, Beeper Desktop rejects large attachments client-side
// before the bridge ever sees them, defeating the compression entirely.
//
// maxIngestVideo and maxIngestAudio only apply when ffmpeg is present on
// the host — see capabilities.go, which falls back to maxMMSSize for those
// types when ffmpeg is missing. maxIngestImage doesn't depend on ffmpeg
// (image recompression is std-lib).
const (
	maxIngestImage = 25 * 1024 * 1024  // 25 MiB — std-lib JPEG recompressor
	maxIngestVideo = 100 * 1024 * 1024 // 100 MiB — H.264 transcode via ffmpeg
	maxIngestAudio = 100 * 1024 * 1024 // 100 MiB — AAC transcode via ffmpeg
)

// imageNeedsRecompress reports whether the image bytes need to go through
// compressImageForMMS — either too many bytes for the wire cap, or pixel
// dimensions over the carrier-friendly limit. Reads only the file header
// (cheap), not the full pixel data.
func imageNeedsRecompress(data []byte, maxSize int) bool {
	if len(data) > maxSize {
		return true
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// Header unreadable. Don't force recompression; let the upload path
		// surface whatever Dialpad/carrier says about it.
		return false
	}
	return cfg.Width > maxMMSImageDimension || cfg.Height > maxMMSImageDimension
}

// maxMMSImageDimension caps the longest side of any outbound MMS image.
// Carriers reject MMS over ~1-2 megapixels for compatibility, so even a
// well-compressed 22 MP iPhone JPEG that fits under 2 MiB still gets
// silently dropped by the receiving carrier. 1600 px on the longest side
// keeps quality reasonable for "viewed on a phone in a text thread."
const maxMMSImageDimension = 1600

// compressImageForMMS recompresses an image (any std-decodable format) to
// fit under maxSize bytes AND under maxMMSImageDimension on the longest
// side. Tries progressively smaller scales × binary-searched JPEG quality.
// Returns the smallest acceptable encoding or an error if even
// minimum-quality at 30% scale exceeds maxSize.
func compressImageForMMS(data []byte, maxSize int) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	bounds := src.Bounds()
	origW, origH := bounds.Dx(), bounds.Dy()
	// If the input is much larger than the carrier-safe dimension, start the
	// scale loop at the size cap rather than 100%. Otherwise an efficient JPEG
	// could ship at full res and get dropped by the carrier on delivery.
	startScale := 1.0
	if maxDim := max(origW, origH); maxDim > maxMMSImageDimension {
		raw := float64(maxMMSImageDimension) / float64(maxDim)
		// Round down to nearest 0.1 step so the loop counter stays in sync.
		startScale = math.Floor(raw*10) / 10
		if startScale < 0.3 {
			startScale = 0.3
		}
	}
	for scale := startScale; scale >= 0.3; scale -= 0.1 {
		w := int(math.Round(float64(origW) * scale))
		h := int(math.Round(float64(origH) * scale))
		if w < 1 || h < 1 {
			continue
		}
		var resized image.Image
		if scale < 1.0 {
			dst := image.NewRGBA(image.Rect(0, 0, w, h))
			draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)
			resized = dst
		} else {
			resized = src
		}
		result, err := findOptimalJPEGQuality(resized, maxSize, 50, 85)
		if err != nil {
			continue
		}
		if result != nil {
			return result, nil
		}
	}
	return nil, fmt.Errorf("could not compress image below %d bytes at any quality/resolution", maxSize)
}

func findOptimalJPEGQuality(img image.Image, maxSize, minQ, maxQ int) ([]byte, error) {
	low, err := encodeJPEG(img, minQ)
	if err != nil {
		return nil, err
	}
	if len(low) > maxSize {
		return nil, nil
	}
	high, err := encodeJPEG(img, maxQ)
	if err != nil {
		return nil, err
	}
	if len(high) <= maxSize {
		return high, nil
	}
	best := low
	lo, hi := minQ, maxQ
	for lo <= hi {
		mid := (lo + hi) / 2
		result, err := encodeJPEG(img, mid)
		if err != nil {
			return best, nil
		}
		if len(result) <= maxSize {
			best = result
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best, nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mediaTranscoder wraps the small slice of the ffmpeg API the bridge actually
// uses, so unit tests can stub it out. Production uses ffmpegTranscoder;
// tests inject a fake that returns canned durations and output bytes.
type mediaTranscoder interface {
	probe(ctx context.Context, data []byte, inputMime string) (durationSec float64, err error)
	convert(ctx context.Context, data []byte, outputExt string, inputArgs, outputArgs []string, inputMime string) ([]byte, error)
}

// ffmpegTranscoder is the production implementation, delegating to
// go.mau.fi/util/ffmpeg. The package's binary path resolution is package-level
// state populated in its init(), so all instances share the same lookup.
type ffmpegTranscoder struct{}

// probe writes the input to a temp file and runs ffprobe to read the
// container duration. Bytes-only ffprobe isn't exposed by the util package,
// so we do the temp-file dance ourselves.
func (ffmpegTranscoder) probe(ctx context.Context, data []byte, inputMime string) (float64, error) {
	if !ffmpeg.ProbeSupported() {
		return 0, fmt.Errorf("ffprobe not available in PATH")
	}
	tempDir, err := os.MkdirTemp("", "dialpad_probe_*")
	if err != nil {
		return 0, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tempDir)
	ext := exmime.ExtensionFromMimetype(inputMime)
	if ext == "" {
		ext = ".bin"
	}
	inputPath := filepath.Join(tempDir, "input"+ext)
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		return 0, fmt.Errorf("write probe input: %w", err)
	}
	res, err := ffmpeg.Probe(ctx, inputPath)
	if err != nil {
		return 0, err
	}
	if res == nil || res.Format == nil {
		return 0, fmt.Errorf("ffprobe returned no format metadata")
	}
	return res.Format.Duration, nil
}

// convert writes the input bytes to a temp file and runs ffmpeg with distinct
// input/output paths. We can't use ffmpeg.ConvertBytes here because it builds
// both paths from the same basename — when input and output extensions match
// (video/mp4 → .mp4), the paths collide and ffmpeg silently no-ops.
func (ffmpegTranscoder) convert(ctx context.Context, data []byte, outputExt string, inputArgs, outputArgs []string, inputMime string) ([]byte, error) {
	tempDir, err := os.MkdirTemp("", "dialpad_convert_*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tempDir)
	inExt := exmime.ExtensionFromMimetype(inputMime)
	if inExt == "" {
		inExt = ".bin"
	}
	inPath := filepath.Join(tempDir, "input"+inExt)
	outPath := filepath.Join(tempDir, "output"+outputExt)
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("write convert input: %w", err)
	}
	if err := ffmpeg.ConvertPathWithDestination(ctx, inPath, outPath, inputArgs, outputArgs, false); err != nil {
		return nil, err
	}
	return os.ReadFile(outPath)
}

// targetAudioBitrate returns the AAC bitrate (kbps) needed to fit an audio
// stream of the given duration into maxBytes, with 8% container/encoder
// headroom. Returns an error if even 32 kbps (the floor for intelligible
// speech) would exceed the budget.
//
// Pure function: no I/O, no ffmpeg call. Easy to test.
func targetAudioBitrate(durationSec float64, maxBytes int) (int, error) {
	if durationSec <= 0 {
		return 0, fmt.Errorf("invalid duration: %f", durationSec)
	}
	const (
		headroom = 0.92
		minKbps  = 32
		maxKbps  = 128
	)
	rawKbps := int(float64(maxBytes) * 8 * headroom / durationSec / 1000)
	if rawKbps < minKbps {
		return 0, fmt.Errorf("audio too long: %.0fs would need <%d kbps to fit %d bytes", durationSec, minKbps, maxBytes)
	}
	if rawKbps > maxKbps {
		rawKbps = maxKbps
	}
	return rawKbps, nil
}

// targetVideoBitrate splits the byte budget between video and a small fixed
// audio reserve (64 kbps), returning the video bitrate (kbps) that fits.
// Errors if the remaining budget is too small for usable video (< 80 kbps).
//
// Headroom is aggressive (35%) because libx264 single-pass ABR is loose with
// the target — measured ~50% overshoot at headroom=0.88 in the integration
// test. With 0.65 headroom + tight maxrate/bufsize in the encode args, the
// real output reliably lands under the cap.
func targetVideoBitrate(durationSec float64, maxBytes int) (int, error) {
	if durationSec <= 0 {
		return 0, fmt.Errorf("invalid duration: %f", durationSec)
	}
	const (
		headroom         = 0.65
		audioReserveKbps = 64
		minVideoKbps     = 80
	)
	totalKbps := int(float64(maxBytes) * 8 * headroom / durationSec / 1000)
	videoKbps := totalKbps - audioReserveKbps
	if videoKbps < minVideoKbps {
		return 0, fmt.Errorf("video too long: %.0fs would leave <%d kbps for video after %d kbps audio reserve",
			durationSec, minVideoKbps, audioReserveKbps)
	}
	return videoKbps, nil
}

// compressAudioForMMS transcodes audio to mono AAC at a bitrate sized to fit
// under maxSize. Returns the transcoded bytes (M4A container) or an error if
// the input is too long to fit even at minimum bitrate.
func compressAudioForMMS(ctx context.Context, tr mediaTranscoder, data []byte, inputMime string, maxSize int) ([]byte, error) {
	duration, err := tr.probe(ctx, data, inputMime)
	if err != nil {
		return nil, fmt.Errorf("probe audio: %w", err)
	}
	kbps, err := targetAudioBitrate(duration, maxSize)
	if err != nil {
		return nil, err
	}
	out, err := tr.convert(ctx, data, ".m4a",
		nil,
		[]string{
			"-vn",
			"-c:a", "aac",
			"-b:a", fmt.Sprintf("%dk", kbps),
			"-ac", "1",
		},
		inputMime,
	)
	if err != nil {
		return nil, fmt.Errorf("transcode audio: %w", err)
	}
	return out, nil
}

// compressVideoForMMS transcodes video to H.264 baseline + AAC (MP4 container,
// faststart), capped at 640px wide and 24 fps for phone-friendly playback.
// Bitrate is computed from the input duration and the maxSize budget.
func compressVideoForMMS(ctx context.Context, tr mediaTranscoder, data []byte, inputMime string, maxSize int) ([]byte, error) {
	duration, err := tr.probe(ctx, data, inputMime)
	if err != nil {
		return nil, fmt.Errorf("probe video: %w", err)
	}
	vKbps, err := targetVideoBitrate(duration, maxSize)
	if err != nil {
		return nil, err
	}
	out, err := tr.convert(ctx, data, ".mp4",
		nil,
		[]string{
			"-c:v", "libx264",
			"-preset", "medium", // better compression efficiency than ultrafast
			"-profile:v", "baseline",
			"-level", "3.0",
			"-pix_fmt", "yuv420p",
			"-vf", "scale='min(640,iw)':-2,fps=24",
			// Strict near-CBR rate control: maxrate = bitrate, small 1-second
			// buffer. Without this, libx264 single-pass ABR can overshoot
			// the target bitrate by 30-50%.
			"-b:v", fmt.Sprintf("%dk", vKbps),
			"-maxrate", fmt.Sprintf("%dk", vKbps),
			"-bufsize", fmt.Sprintf("%dk", vKbps),
			"-c:a", "aac",
			"-b:a", "64k",
			"-ac", "1",
			"-movflags", "+faststart",
		},
		inputMime,
	)
	if err != nil {
		return nil, fmt.Errorf("transcode video: %w", err)
	}
	return out, nil
}

// downloadMatrixMedia downloads a media attachment from the Matrix homeserver,
// transparently decrypting if the room is E2EE.
//
// In E2EE rooms the media is uploaded as ciphertext and the URL + AES-CTR key
// live in content.File (event.EncryptedFileInfo); content.URL is empty.
// mautrix-go's DownloadMedia uses file.URL when file != nil and runs
// DecryptInPlace on the response. Passing nil here for an E2EE message
// returns the ciphertext bytes — uploading those to Dialpad produces a
// corrupt blob that the recipient can't render. Same pattern used by
// mautrix-twitter (pkg/connector/handlematrix.go:191).
func (da *DialpadAPI) downloadMatrixMedia(ctx context.Context, msg *bridgev2.MatrixMessage) ([]byte, string, error) {
	content := msg.Content
	if content == nil {
		return nil, "", fmt.Errorf("no message content")
	}
	if content.URL == "" && (content.File == nil || content.File.URL == "") {
		return nil, "", fmt.Errorf("no media URL in message")
	}

	data, err := da.connector.br.Bot.DownloadMedia(ctx, content.URL, content.File)
	if err != nil {
		return nil, "", fmt.Errorf("download from Matrix: %w", err)
	}

	// Use the MIME type from the event if available, otherwise sniff from content
	mimeType := ""
	if content.Info != nil && content.Info.MimeType != "" {
		mimeType = content.Info.MimeType
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}

	return data, mimeType, nil
}

// mediaHint carries optional filename/content-type metadata for inbound media
// so the resulting Matrix event has a proper FileName + extension instead of
// the generic "attachment" placeholder. Pass nil when no hint is available;
// uploadMediaToMatrix will fall back to magic-byte detection + a mime-derived
// default filename.
type mediaHint struct {
	Filename    string
	ContentType string
}

// swapMediaExtension replaces the extension on a filename (or appends one if
// there isn't already an extension). Used after transcoding so the upload
// carries the right extension for the new container format.
func swapMediaExtension(filename, newExt string) string {
	if filename == "" {
		return "attachment" + newExt
	}
	if dot := strings.LastIndexByte(filename, '.'); dot > 0 && dot > strings.LastIndexByte(filename, '/') {
		return filename[:dot] + newExt
	}
	return filename + newExt
}

// defaultMediaFilename returns a sensible filename for an inbound media
// attachment when the server didn't give us one. Used as a final fallback so
// Matrix clients always see SOMETHING with an extension.
func defaultMediaFilename(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/jpeg"):
		return "image.jpg"
	case strings.HasPrefix(mimeType, "image/png"):
		return "image.png"
	case strings.HasPrefix(mimeType, "image/gif"):
		return "image.gif"
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "video/mp4"):
		return "video.mp4"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case strings.HasPrefix(mimeType, "audio/mpeg"):
		return "audio.mp3"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	default:
		return "attachment"
	}
}

// uploadMediaToMatrix downloads media from a Dialpad URL and uploads it to Matrix.
// Returns a MessageEventContent ready to use in a ConvertedMessagePart. The
// hint argument (if non-nil) supplies the canonical filename and content type
// that Dialpad provided in MMSDetails; without it, the function still works
// but the resulting Matrix event uses a synthesized default filename.
func (da *DialpadAPI) uploadMediaToMatrix(ctx context.Context, intent bridgev2.MatrixAPI, mediaURL string, hint *mediaHint) (*event.MessageEventContent, error) {
	data, err := da.client.DownloadMedia(ctx, mediaURL)
	if err != nil {
		return nil, fmt.Errorf("download from Dialpad: %w", err)
	}

	// Prefer the hint's content type — Dialpad's MMSDetails.ContentType is
	// the authoritative value. Fall back to magic-byte sniffing only when
	// the hint is missing.
	mimeType := ""
	if hint != nil {
		mimeType = hint.ContentType
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}

	msgType := event.MsgFile
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		msgType = event.MsgImage
	case strings.HasPrefix(mimeType, "video/"):
		msgType = event.MsgVideo
	case strings.HasPrefix(mimeType, "audio/"):
		msgType = event.MsgAudio
	}

	filename := ""
	if hint != nil {
		filename = hint.Filename
	}
	if filename == "" {
		filename = defaultMediaFilename(mimeType)
	}

	uri, encFile, err := intent.UploadMedia(ctx, "", data, filename, mimeType)
	if err != nil {
		return nil, fmt.Errorf("upload to Matrix: %w", err)
	}

	return &event.MessageEventContent{
		MsgType:  msgType,
		Body:     filename,
		FileName: filename,
		URL:      uri,
		File:     encFile,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
		},
	}, nil
}
