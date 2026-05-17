package types

// Attachment represents a media attachment on a message.
type Attachment struct {
	ID       string `json:"id"`
	MIMEType string `json:"mime_type"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}
