package dialgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// FeedMessage represents an entry from the internal /api/feed/ endpoint.
// The feed returns a mix of SMS messages and Call entries in the same array.
// Reverse-engineered from the Dialpad web client HAR capture.
type FeedMessage struct {
	ID             int64  `json:"id"`
	Key            string `json:"key"`       // App Engine Datastore entity key
	ContactKey     string `json:"contact_key"`
	Date           int64  `json:"date"`           // Unix timestamp in milliseconds
	Orientation    string `json:"orientation"`     // "internal" = sent by user, "external" = received
	DeliveryMethod string `json:"delivery_method"` // "dp" = internal, "sms", "mms"
	Text           string `json:"text"`
	FromPhone      string `json:"from_phone"`
	ToPhone        string `json:"to_phone"`

	// MMS fields — populated when delivery_method is "mms"
	// HAR evidence: field is "mms_url" (not "media_url") with "mms_details" metadata
	MMSURL     string      `json:"mms_url,omitempty"`
	ThumbURL   string      `json:"thumb_url,omitempty"`
	MMSDetails *MMSDetails `json:"mms_details,omitempty"`

	// Call fields — populated when feed_type is "Call"
	FeedType          string         `json:"feed_type"`           // "Call" for call entries, empty/other for SMS
	FeedDate          int64          `json:"feed_date"`           // timestamp for feed ordering
	FeedTags          []string       `json:"feed_tags"`           // e.g. ["voicemail", "missed", "inbox"]
	Category          string         `json:"category"`            // "incoming", "missed", "outgoing", "cancelled", "voicemail"
	State             string         `json:"state"`               // "hangup" for completed calls
	Direction         string         `json:"direction"`           // "inbound", "outbound"
	ExternalEndpoint  string         `json:"external_endpoint"`   // caller/callee E.164 number
	Duration          int            `json:"duration"`            // total call duration in seconds
	DurationConnected int            `json:"duration_connected"` // connected duration in seconds
	DateStarted       int64          `json:"date_started"`        // epoch millis
	DateConnected     int64          `json:"date_connected"`      // epoch millis
	DateEnded         int64          `json:"date_ended"`          // epoch millis
	Recording         *CallRecording `json:"recording,omitempty"` // Voicemail recording (present when category=="voicemail")
	RecordingID       string         `json:"recording_id,omitempty"`
}

// FeedContact represents a conversation entry from the internal /api/contact/ endpoint.
type FeedContact struct {
	ContactID        int64            `json:"contact_id"`
	ContactKey       string           `json:"contact_key"`
	DisplayName      string           `json:"display_name"`
	DialString       string           `json:"dial_string"`
	PrimaryPhone     string           `json:"primary_phone"`
	Phones           []string         `json:"phones"`
	LastMessage      *FeedLastMessage `json:"last_message,omitempty"`
	DateDescription  int64            `json:"date_description"`
	Unread           int              `json:"unread"`
	Type             string           `json:"type,omitempty"`
	SelectedCallerID string           `json:"selected_caller_id,omitempty"`
	// TargetKey scopes this conversation's feed. May be the user's
	// UserProfile key (personal-line chats) OR the Office key (office-line
	// chats) — varies per contact. Use this when querying /api/feed/.
	TargetKey string `json:"target_key,omitempty"`
}

// IsGroup reports whether this entry is a multi-recipient contact_group.
func (fc *FeedContact) IsGroup() bool {
	return fc.Type == "contact_group"
}

// FeedLastMessage is the last message preview in a conversation listing.
type FeedLastMessage struct {
	Date      int64  `json:"date"` // Unix timestamp in milliseconds
	Text      string `json:"text"`
	FromPhone string `json:"from_phone"`
}

// GetMessageHistory fetches message history for a conversation via the internal API.
// GET /api/feed/?contact_key=...&target_key=...&limit=N
//
// This endpoint is NOT in the public API — it's reverse-engineered from the web client.
// The target_key is the logged-in user's entity key. The contact_key identifies the conversation.
func (c *Client) GetMessageHistory(ctx context.Context, contactKey, targetKey string, limit int) ([]FeedMessage, error) {
	if !c.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}

	if limit <= 0 {
		limit = 25
	}

	params := url.Values{
		"contact_key":        {contactKey},
		"target_key":         {targetKey},
		"limit":              {strconv.Itoa(limit)},
		"projection":         {"-contact"},
		"support_link_media": {"true"},
	}

	reqURL := c.InternalAPIBaseURL + "/feed/?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, err
	}


	resp, err := c.doAPIRequest(req, http.StatusOK, "get message history")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read the full response body for debugging
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read message history response: %w", err)
	}

	c.Log.Debug().
		Str("url", reqURL).
		Int("response_len", len(bodyBytes)).
		Str("response_preview", string(bodyBytes[:min(len(bodyBytes), 2000)])).
		Msg("Feed API raw response")

	var messages []FeedMessage
	if err := json.Unmarshal(bodyBytes, &messages); err != nil {
		return nil, fmt.Errorf("decode message history (body=%s): %w", string(bodyBytes[:min(len(bodyBytes), 200)]), err)
	}

	return messages, nil
}

// GetConversations fetches the conversation list via the internal API.
// GET /api/contact/?filter=messages&include_last_message=1&target_key=...&limit=N
//
// filter=messages matches what the Dialpad web client uses for the inbox
// list (verified via HAR). filter=all returns only self-reference entries
// when called against the Office target, blocking office-line chats from
// being synced. Endpoint is NOT in the public API.
func (c *Client) GetConversations(ctx context.Context, targetKey string, limit int) ([]FeedContact, error) {
	if limit <= 0 {
		limit = 25
	}
	return getJSON[[]FeedContact](ctx, c, "/contact/", url.Values{
		"filter":               {"messages"},
		"include_last_message": {"1"},
		"target_key":           {targetKey},
		"limit":                {strconv.Itoa(limit)},
	}, "get conversations")
}

// InternalUser represents the user info returned by the internal /api/user/me endpoint.
type InternalUser struct {
	Key         string      `json:"key"`          // UserProfile entity key — used as sender_key on sends and target_key on /api/contact/
	OfficeKey   string      `json:"office_key"`   // Office entity key — used as target_key/feed_target on /api/feed/ and sends
	ID          string      `json:"id"`           // User identifier (Datastore key string)
	UserID      json.Number `json:"user_id"`      // Numeric user ID (used in Ably channels: UserProfile-{user_id}:main)
	DisplayName string      `json:"display_name"`
	FirstName   string      `json:"first_name"`
	LastName    string      `json:"last_name"`
	Email       string      `json:"email,omitempty"`
	CallerID    string      `json:"caller_id,omitempty"`  // E.164 phone number (e.g., "+14155550100")
	Phones      []string    `json:"phones,omitempty"`     // List of phone numbers
}



// OfficeInfo represents an Office entity from GET /api/group/{office_key}.
// Reverse-engineered from the Dialpad web client; used to discover the office's
// DIDs so we can include them in the user's "own lines" set.
type OfficeInfo struct {
	GroupID     int64    `json:"group_id"`
	DisplayName string   `json:"display_name"`
	CallerID    string   `json:"caller_id"`
	DialString  string   `json:"dial_string"`
	DIDs        []string `json:"dids"`
}

// GetOffice fetches the office entity's metadata (notably its DIDs) via the
// internal API. GET /api/group/{office_key}.
func (c *Client) GetOffice(ctx context.Context, officeKey string) (*OfficeInfo, error) {
	return getJSON[*OfficeInfo](ctx, c, "/group/"+officeKey, nil, "get office")
}

// ContactInfo represents the contact details returned by GET /api/contact/{key}.
// Reverse-engineered from the Dialpad web client's contact update flow.
type ContactInfo struct {
	ContactID    int64    `json:"contact_id"`
	ContactKey   string   `json:"contact_key"`
	DisplayName  string   `json:"display_name"`
	FirstName    string   `json:"first_name"`
	LastName     string   `json:"last_name,omitempty"`
	CompanyName  string   `json:"company_name,omitempty"`
	DialString   string   `json:"dial_string"`    // E.164 phone (e.g., "+14155550102")
	Phones       []string `json:"phones"`
	Emails       []string `json:"emails"`
	IsExternal   bool     `json:"is_external"`
	ImageURL     string   `json:"image_url,omitempty"`
}

// GetContact fetches contact details by contact_key via the internal API.
// GET /api/contact/{contact_key}. The contact_key comes from Ably push events
// (call.contact_key field).
func (c *Client) GetContact(ctx context.Context, contactKey string) (*ContactInfo, error) {
	return getJSON[*ContactInfo](ctx, c, "/contact/"+contactKey, nil, "get contact")
}

// ActiveCall represents an active call from GET /api/activecalls/me.
// Reverse-engineered from the Dialpad web client HAR capture.
// This is what the web client polls when a delta event signals "on_call".
type ActiveCall struct {
	ID                int64              `json:"id"`
	State             string             `json:"state"`              // "ringing", "connected", "hangup"
	Direction         string             `json:"direction"`          // "inbound", "outbound"
	ExternalEndpoint  string             `json:"external_endpoint"`  // Caller's E.164 number
	InternalEndpoint  string             `json:"internal_endpoint"`  // Dialpad user's number
	ContactKey        string             `json:"contact_key"`
	Contact           *ActiveCallContact `json:"contact"`
	DateFirstRang     int64              `json:"date_first_rang"`    // epoch millis
	DateStarted       int64              `json:"date_started"`       // epoch millis
	DateConnected     int64              `json:"date_connected"`     // epoch millis (null if not answered)
	DateEnded         int64              `json:"date_ended"`         // epoch millis (null if active)
	Duration          int                `json:"duration"`
	DurationConnected int                `json:"duration_connected"`
	EntryPointDID     string             `json:"entry_point_did"`    // Dialpad line number
}

// ActiveCallContact is the embedded contact info in an active call response.
type ActiveCallContact struct {
	DisplayName  string `json:"display_name"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	DialString   string `json:"dial_string"`    // E.164
	ContactKey   string `json:"key"`            // Datastore entity key
	ImageURL     string `json:"image_url"`
	IsExternal   bool   `json:"is_external"`
	PrimaryPhone string `json:"primary_phone"`
}

// GetActiveCalls fetches currently active calls for the logged-in user.
// GET /api/activecalls/me. The Dialpad web client polls this endpoint when
// it detects a delta presence change to "on_call".
func (c *Client) GetActiveCalls(ctx context.Context) ([]ActiveCall, error) {
	return getJSON[[]ActiveCall](ctx, c, "/activecalls/me", nil, "get active calls")
}

// CallRecording represents a voicemail recording attached to a call.
// When a caller leaves a voicemail, the feed entry has category="voicemail"
// and includes this recording object with the MP3 URL and transcription.
//
// Example from the feed API:
//
//	"recording": {
//	  "date": 1776264661416,
//	  "date_listened": null,
//	  "duration": 5,
//	  "id": "a822eccd-...",
//	  "recording_url": "https://dialpad.com/blob/voicemail/a822eccd-.../....mp3?t=1&region=us",
//	  "transcription_text": "Hello, this is a message..."
//	}
type CallRecording struct {
	Date              int64  `json:"date"`
	DateListened      *int64 `json:"date_listened"`
	Duration          int    `json:"duration"`          // Duration of voicemail in seconds
	ID                string `json:"id"`
	RecordingURL      string `json:"recording_url"`     // MP3 download URL (authenticated)
	TranscriptionText string `json:"transcription_text"` // Auto-transcription of the voicemail
}

// GetCallByID fetches the full call detail by call ID.
// GET /api/call/{call_id}. For voicemail calls the response includes the
// recording object with an MP3 URL on Dialpad's blob storage.
func (c *Client) GetCallByID(ctx context.Context, callID int64) (*FeedMessage, error) {
	return getJSON[*FeedMessage](ctx, c, fmt.Sprintf("/call/%d", callID), nil, "get call by ID")
}

// internalMsgCounter is a process-wide counter for generating unique feed_key
// values used in the SendInternalMessage API. The Dialpad web client uses a
// client-scoped counter (sent0, sent1, ...) to assign local IDs before the
// server assigns a permanent ID.
var internalMsgCounter atomic.Int64

// NewFeedKey returns the next per-process feed_key in the "sent{N}" sequence
// that the web client uses for outbound text messages. Exposed so callers can
// pre-generate the key, register a pending echo against it, then pass it back
// into SendInternalMessage — guarantees the send-side and echo-side use the
// same identifier (the push event carries it back as TextMessagePush.FeedKey).
func (c *Client) NewFeedKey() string {
	return fmt.Sprintf("sent%d", internalMsgCounter.Add(1)-1)
}

// UploadFileResponse is the response from POST /api/upload_file/.
// Reverse-engineered from the Dialpad web client MMS upload flow.
type UploadFileResponse struct {
	ContentType string `json:"content_type"` // e.g. "image/png"
	Filename    string `json:"filename"`     // original filename
	GCSFilename string `json:"gcs_filename"` // GCS storage path (e.g. "/uber-voice_mms_us-central1/2026-05-02/uuid")
	UUID        string `json:"uuid"`         // unique file ID
}

// sanitizeMMSFilename removes characters Dialpad's upload endpoint rejects in
// multipart filenames: spaces, control characters, and anything outside a
// conservative ASCII set. The Dialpad web client uses sanitized names (HAR
// shows no spaces / special chars), and the upload endpoint has been
// observed to 400 with body "null" when a filename contains spaces.
func sanitizeMMSFilename(name string) string {
	if name == "" {
		return "upload"
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "upload"
	}
	return out
}

// UploadFile uploads a file to Dialpad's internal storage for MMS.
// POST /api/upload_file/ — multipart/form-data with fields:
//   - file: binary file data (filename sanitized — see sanitizeMMSFilename),
//     with the part's Content-Type set to the real MIME type
//   - file_type: "MMS"
//
// The per-part Content-Type matters: Dialpad rejects the request with HTTP
// 400 (body "null") if the file part declares application/octet-stream, which
// is what multipart.CreateFormFile hard-codes. We build the part header
// manually to send the actual image MIME type instead.
//
// Returns the upload metadata needed for the subsequent PATCH /api/textmessage/ call.
func (c *Client) UploadFile(ctx context.Context, data []byte, filename, contentType string) (*UploadFileResponse, error) {
	if !c.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}

	safeName := sanitizeMMSFilename(filename)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, safeName))
	partHeader.Set("Content-Type", contentType)
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("write file data: %w", err)
	}

	if err := writer.WriteField("file_type", "MMS"); err != nil {
		return nil, fmt.Errorf("write file_type: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.InternalAPIBaseURL+"/upload_file/", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// Browser-flavored headers. The default Go-http-client/1.1 UA appears to
	// trip Dialpad's bot heuristics on /upload_file/ (HTTP 400 "null"). Origin
	// and Referer match what the web client sends.
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Origin", "https://dialpad.com")
	req.Header.Set("Referer", "https://dialpad.com/app/")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	c.Log.Debug().
		Str("filename_original", filename).
		Str("filename_sanitized", safeName).
		Str("content_type", contentType).
		Int("body_bytes", body.Len()).
		Msg("Uploading MMS file to Dialpad")

	resp, err := c.doAPIRequest(req, http.StatusOK, "upload MMS file")
	if err != nil {
		// doAPIRequest already captures the body in APIError; on this endpoint
		// the body is often the literal "null" string, so also surface response
		// headers (Date, Content-Length, X-Request-Id) to aid diagnosis.
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			c.Log.Warn().
				Str("filename", safeName).
				Int("status", apiErr.StatusCode).
				Str("body", apiErr.Body).
				Msg("Dialpad rejected MMS upload")
		}
		return nil, err
	}
	defer resp.Body.Close()

	var result UploadFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode upload response: %w", err)
	}

	return &result, nil
}

// InternalMessageRequest contains the fields for sending a message via the
// internal textmessage API (PATCH /api/textmessage/{feed_key}).
//
// Reverse-engineered from the Dialpad web client HAR capture.
type InternalMessageRequest struct {
	SenderKey  string // UserProfile entity key (the user sending)
	TargetKey  string // Office entity key (the line this message belongs to) — used for both target_key and feed_target
	ContactKey string // The recipient contact's entity key
	TargetDID  string // E.164 phone number of the line the message is being sent FROM
	Text       string // Message body

	// FeedKey overrides the auto-generated per-process feed_key. When set,
	// SendInternalMessage uses it verbatim instead of calling NewFeedKey().
	// Callers should pre-generate via Client.NewFeedKey() and pass it back
	// here so the local pending registration and the eventual push echo
	// share the same identifier.
	FeedKey string

	// MMS fields — set these when sending an image. Populated from UploadFile response.
	MMS *MMSAttachment
}

// MMSAttachment contains the upload metadata for an outbound MMS.
type MMSAttachment struct {
	ContentType string // MIME type (e.g. "image/png")
	Filename    string // Original filename
	GCSFilename string // GCS storage path from UploadFile response
	UUID        string // Unique file ID from UploadFile response
	Bytes       int    // File size in bytes
}

// InternalMessageResponse is the response from PATCH /api/textmessage/{feed_key}.
type InternalMessageResponse struct {
	Contact json.RawMessage `json:"contact"` // Contact details (preserved but unused)
	// The response doesn't contain a simple message ID — the server assigns
	// the real ID asynchronously and delivers it via Ably push.
}

// SendInternalMessage sends a text message via the internal textmessage API.
// PATCH /api/textmessage/{feed_key}
//
// This endpoint is used by the Dialpad web client to message internal contacts
// (e.g., Dialbot). The feed_key is a client-assigned identifier (e.g., "sent0",
// "sent1") that serves as a temporary local ID until the server assigns the real ID.
//
// Returns the feed_key used as the local transaction ID.
func (c *Client) SendInternalMessage(ctx context.Context, req *InternalMessageRequest) (string, error) {
	feedKey := req.FeedKey
	if feedKey == "" {
		feedKey = c.NewFeedKey()
	}

	body := map[string]interface{}{
		"feed_type":   "TextMessage",
		"orientation": "internal",
		"sender_key":  req.SenderKey,
		"feed_date":   time.Now().UnixMilli(),
		"client_id":   feedKey, // unique per-session
		"target_key":  req.TargetKey,
		"contact_key": req.ContactKey,
		"target_did":  req.TargetDID,
		"isChannel":   false,
		"feed_target": req.TargetKey,
		"feed_key":    feedKey,
		"text":        req.Text,
		"action":      "new",
		// Fields the web client sends but are optional:
		"ignored_rich_media_urls": []string{},
		"rich_media_tiptap_json":  nil,
		"rich_text_markdown":      nil,
	}

	// MMS fields — injected when sending an image attachment.
	// Reverse-engineered from HAR: the web client adds mmsType, fileType,
	// mms_url (boolean true), mms_details, and mms (upload metadata) to the
	// same PATCH /api/textmessage/ payload used for text SMS.
	if req.MMS != nil {
		ext := filepath.Ext(req.MMS.Filename)
		if ext != "" {
			ext = ext[1:] // strip leading dot
		}
		mmsType := "image"
		switch {
		case strings.HasPrefix(req.MMS.ContentType, "video/"):
			mmsType = "video"
		case strings.HasPrefix(req.MMS.ContentType, "audio/"):
			mmsType = "audio"
		}

		body["mmsType"] = mmsType
		body["fileType"] = req.MMS.ContentType
		body["mms_url"] = true
		body["mms_details"] = map[string]interface{}{
			"content_type": req.MMS.ContentType,
			"bytes":        req.MMS.Bytes,
			"type":         mmsType,
			"ext":          ext,
		}
		body["mms"] = map[string]interface{}{
			"file":         map[string]interface{}{},
			"id":           "upload_0",
			"path":         fmt.Sprintf("%s%d", req.MMS.Filename, req.MMS.Bytes),
			"content_type": req.MMS.ContentType,
			"filename":     req.MMS.Filename,
			"gcs_filename": req.MMS.GCSFilename,
			"uuid":         req.MMS.UUID,
			"fileType":     "MMS",
		}
	}

	// Response body is discarded — the server returns the assigned message ID
	// via the Ably push echo, not in the PATCH response. json.RawMessage as T
	// lets the helper succeed without forcing us to model an unused struct.
	if _, err := patchJSON[json.RawMessage](ctx, c, "/textmessage/"+feedKey, nil, body, "send internal message"); err != nil {
		return "", err
	}
	return feedKey, nil
}

// IsInternalContact checks whether a phone number belongs to an internal/system
// contact (like Dialbot) that requires the internal textmessage API instead of
// the public SMS API.
func IsInternalContact(phoneNumber string) bool {
	// Dialbot — Dialpad's system notification service
	return strings.TrimSpace(phoneNumber) == "+14159389005"
}
