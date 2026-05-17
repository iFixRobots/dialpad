package connector

import "testing"

// Locks the caption-detection rule. The bug this guards against: a large
// voice note got transcoded, the filename was renamed from .mp3 to .m4a,
// the old "Body == filename" check then thought Body was a caption and
// the recipient saw "Large Voice Note.mp3" as a literal text caption next
// to their audio attachment.
func TestDeriveMMSCaption(t *testing.T) {
	cases := []struct {
		name             string
		body             string
		originalFilename string
		finalFilename    string
		want             string
	}{
		{
			name:             "empty body",
			body:             "",
			originalFilename: "vacation.jpg",
			finalFilename:    "vacation.jpg",
			want:             "",
		},
		{
			name:             "body == filename, no compression rename — no caption",
			body:             "vacation.jpg",
			originalFilename: "vacation.jpg",
			finalFilename:    "vacation.jpg",
			want:             "",
		},
		{
			name:             "body matches original after audio transcode renames .mp3 → .m4a",
			body:             "Large Voice Note.mp3",
			originalFilename: "Large Voice Note.mp3",
			finalFilename:    "Large Voice Note.m4a",
			want:             "",
		},
		{
			name:             "body matches post-rename filename (image PNG → JPG)",
			body:             "image.jpg",
			originalFilename: "screenshot.png",
			finalFilename:    "image.jpg",
			want:             "",
		},
		{
			name:             "genuine caption — user typed something different",
			body:             "Check out this view!",
			originalFilename: "IMG_1234.jpg",
			finalFilename:    "IMG_1234.jpg",
			want:             "Check out this view!",
		},
		{
			name:             "genuine caption with transcoded audio",
			body:             "Listen to this clip",
			originalFilename: "song.mp3",
			finalFilename:    "song.m4a",
			want:             "Listen to this clip",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveMMSCaption(tc.body, tc.originalFilename, tc.finalFilename); got != tc.want {
				t.Errorf("deriveMMSCaption(%q, %q, %q) = %q; want %q",
					tc.body, tc.originalFilename, tc.finalFilename, got, tc.want)
			}
		})
	}
}
