package connector

import (
	"testing"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// Locks the user-visible text rendered when a feed entry is an MMS but
// Dialpad either omitted the download URL or served a URL that 404s. The
// regression this guards against: the previous behavior silently dropped
// the message from the room timeline, leaving the user (correctly) unsure
// whether the message had even been sent.
func TestPlaceholderForMissingMMS(t *testing.T) {
	cases := []struct {
		name string
		fm   *dialgo.FeedMessage
		want string
	}{
		{
			name: "no MMSDetails — generic placeholder",
			fm:   &dialgo.FeedMessage{},
			want: "📎 Attachment unavailable (Dialpad did not serve a download URL)",
		},
		{
			name: "filename + type",
			fm: &dialgo.FeedMessage{
				MMSDetails: &dialgo.MMSDetails{
					Filename: "CleanShot.png",
					Type:     "image",
				},
			},
			want: "📎 CleanShot.png — image attachment unavailable (Dialpad did not serve a download URL)",
		},
		{
			name: "filename only",
			fm: &dialgo.FeedMessage{
				MMSDetails: &dialgo.MMSDetails{Filename: "voicenote.mp3"},
			},
			want: "📎 voicenote.mp3 — attachment unavailable",
		},
		{
			name: "type only — falls back to content_type when 'type' field is empty",
			fm: &dialgo.FeedMessage{
				MMSDetails: &dialgo.MMSDetails{ContentType: "audio/mpeg"},
			},
			want: "📎 audio/mpeg attachment unavailable (Dialpad did not serve a download URL)",
		},
		{
			name: "type field present takes precedence over content_type",
			fm: &dialgo.FeedMessage{
				MMSDetails: &dialgo.MMSDetails{Type: "video", ContentType: "video/mp4"},
			},
			want: "📎 video attachment unavailable (Dialpad did not serve a download URL)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := placeholderForMissingMMS(tc.fm); got != tc.want {
				t.Errorf("placeholderForMissingMMS\n got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}
