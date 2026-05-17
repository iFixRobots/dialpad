package connector

import (
	"context"

	"go.mau.fi/util/ffmpeg"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

// supportedIfFFmpeg returns PartialSupport when the host has ffmpeg
// installed (so the bridge can transcode oversized inputs to fit the
// 2 MiB MMS cap) and Rejected otherwise. Used for MIME types where the
// raw passthrough produces unreliable results — voice-note audio/ogg
// being the canonical example.
func supportedIfFFmpeg() event.CapabilitySupportLevel {
	if ffmpeg.Supported() {
		return event.CapLevelPartialSupport
	}
	return event.CapLevelRejected
}

// videoIngestCap and audioIngestCap return the MaxSize the bridge advertises
// for the corresponding media type — large when the host has ffmpeg (so the
// bridge can transcode), tight when it doesn't (so the client refuses
// uploads we couldn't compress anyway). Returns int64 because that's the
// type event.FileFeatures.MaxSize expects.
func videoIngestCap() int64 {
	if ffmpeg.Supported() {
		return int64(maxIngestVideo)
	}
	return int64(maxMMSSize)
}

func audioIngestCap() int64 {
	if ffmpeg.Supported() {
		return int64(maxIngestAudio)
	}
	return int64(maxMMSSize)
}

// capID stamps "+ffmpeg" onto the capability identifier when ffmpeg is
// available on the host, so Beeper Desktop invalidates its cached set
// if the bridge moves to/from an ffmpeg-equipped machine. Bump the date
// component when the cap shape changes so existing clients re-fetch.
func capID() string {
	base := "dialpad.capabilities.2026_05_16_b"
	if ffmpeg.Supported() {
		return base + "+ffmpeg"
	}
	return base
}

func (da *DialpadAPI) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	videoMax := videoIngestCap()
	audioMax := audioIngestCap()
	imageMax := int64(maxIngestImage)
	return &event.RoomFeatures{
		ID:            capID(),
		MaxTextLength: 1600, // SMS concatenated limit
		Edit:          event.CapLevelRejected,
		Delete:        event.CapLevelRejected,
		Reaction:      event.CapLevelRejected,
		Reply:         event.CapLevelRejected,
		Thread:        event.CapLevelRejected,
		File: event.FileFeatureMap{
			event.MsgImage: &event.FileFeatures{
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"image/*": event.CapLevelPartialSupport,
				},
				MaxSize: imageMax,
			},
			event.MsgVideo: &event.FileFeatures{
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"video/*": event.CapLevelPartialSupport,
				},
				MaxSize: videoMax,
			},
			event.MsgAudio: &event.FileFeatures{
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"audio/*": event.CapLevelPartialSupport,
				},
				MaxSize: audioMax,
			},
			// MsgFile is allowed only for MIME types Dialpad's MMS upload
			// endpoint actually accepts (verified: PDF and other arbitrary
			// types are hard-rejected with HTTP 400 body "null"). Image/
			// video/audio share the same upload path used by MsgImage etc.,
			// so users dragging in a .gif as an attachment will route here.
			//
			// MaxSize stays at maxMMSSize for the file path because we don't
			// know which compressor (if any) will apply — sendMMS dispatches
			// on MIME prefix at runtime.
			event.MsgFile: &event.FileFeatures{
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"image/*": event.CapLevelPartialSupport,
					"video/*": event.CapLevelPartialSupport,
					"audio/*": event.CapLevelPartialSupport,
					"*/*":     event.CapLevelRejected,
				},
				MaxSize: imageMax, // pessimistic but >= every per-type cap
			},
			// Stickers — Beeper's GIF picker sends as m.sticker, not m.image.
			// Same wire path as images on Dialpad's side.
			event.CapMsgSticker: &event.FileFeatures{
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"image/*": event.CapLevelPartialSupport,
				},
				MaxSize: imageMax,
			},
			// Voice notes — Beeper records as audio/ogg + Opus, which Dialpad
			// doesn't render natively. Requires ffmpeg to transcode to AAC.
			event.CapMsgVoice: &event.FileFeatures{
				MimeTypes: map[string]event.CapabilitySupportLevel{
					"audio/mp4":  event.CapLevelPartialSupport, // accepted as-is
					"audio/mpeg": event.CapLevelPartialSupport,
					"audio/ogg":  supportedIfFFmpeg(), // needs transcode
				},
				MaxSize: audioMax,
			},
		},
	}
}
