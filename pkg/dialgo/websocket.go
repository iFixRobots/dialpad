package dialgo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
)

// Ably realtime push via Dialpad's internal API.
// The web client uses POST /api/realtime/channelauth/ to get an Ably JWT,
// then connects to wss://realtime.push.dialpad.com with the Ably v5 protocol.
// Events arrive on the channel "UserProfile-{user_id}:main".
const (
	ablyPushHost = "wss://realtime.push.dialpad.com"

	// Ably tokens expire after 1 hour. Refresh well before that.
	ablyTokenLifetime = 50 * time.Minute
	reconnectBackoff  = 5 * time.Second
	maxReconnectWait  = 5 * time.Minute
)

// Ably protocol action codes (v5, JSON format).
const (
	ablyActionHeartbeat = 0
	ablyActionConnected = 4
	ablyActionError     = 9
	ablyActionAttach    = 10
	ablyActionAttached  = 11
	ablyActionDetach    = 12
	ablyActionMessage   = 15
	ablyActionPresence  = 16
)

// channelAuthResponse is the response from POST /api/realtime/channelauth/.
type channelAuthResponse struct {
	Capabilities []string `json:"capabilities"`
	Token        string   `json:"token"`
}

// ablyMessage is a message received on the Ably WebSocket.
type ablyMessage struct {
	Action          int                `json:"action"`
	Channel         string             `json:"channel,omitempty"`
	ConnectionID    string             `json:"connectionId,omitempty"`
	Messages        []ablyMessageEntry `json:"messages,omitempty"`
	Presence        json.RawMessage    `json:"presence,omitempty"`
	Error           *ablyError         `json:"error,omitempty"`
	ConnectionDetails json.RawMessage  `json:"connectionDetails,omitempty"`
	ChannelSerial   string             `json:"channelSerial,omitempty"`
	Flags           int                `json:"flags,omitempty"`
}

// ablyMessageEntry is a single message in an Ably MESSAGE frame.
type ablyMessageEntry struct {
	Name      string          `json:"name"`
	Data      json.RawMessage `json:"data"`
	ID        string          `json:"id,omitempty"`
	Timestamp int64           `json:"timestamp,omitempty"`
}

// ablyError is an error payload from Ably.
type ablyError struct {
	Code       int    `json:"code"`
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
}

// websocketConn wraps the Ably WebSocket connection for Dialpad push events.
type websocketConn struct {
	log    zerolog.Logger
	client *Client

	ctx    context.Context
	cancel context.CancelFunc

	conn    *websocket.Conn
	writeMu sync.Mutex
	closed  bool

	ablyToken   string
	clientID    string   // "{user_id}-{switch_id}"
	switchID    string   // random client suffix
	channels    []string // channels from capabilities
	mainChannel string   // "UserProfile-{user_id}:main"
}

// getAblyToken fetches a fresh Ably JWT from the Dialpad internal API.
// POST /api/realtime/channelauth/
func (c *Client) getAblyToken(ctx context.Context, switchID string) (*channelAuthResponse, error) {
	body := strings.NewReader(`{"extra_channels":[]}`)

	req, err := http.NewRequestWithContext(ctx, "POST", c.InternalAPIBaseURL+"/realtime/channelauth/", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("switch-id", switchID)
	req.Header.Set("switch-type", "harness")

	resp, err := c.doAPIRequest(req, http.StatusOK, "ably channel auth")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var authResp channelAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return nil, fmt.Errorf("decode channelauth response: %w", err)
	}

	c.Log.Info().
		Int("capabilities", len(authResp.Capabilities)).
		Msg("Obtained Ably push token")

	return &authResp, nil
}

// Connect establishes an Ably WebSocket connection to receive real-time events.
// The client must have a valid Bearer token (call SetBearerToken first).
//
// Flow:
//  1. POST /api/realtime/channelauth/ → get Ably JWT + capabilities
//  2. Connect WebSocket to wss://realtime.push.dialpad.com with the JWT
//  3. Subscribe to UserProfile-{user_id}:main channel
//  4. Handle hourly token refresh and reconnection
func (c *Client) Connect(ctx context.Context) error {
	if c.bearerToken == "" {
		return ErrNotLoggedIn
	}

	c.Log.Info().Msg("Connecting to Dialpad via Ably push")

	// Generate a random switch ID for this client instance
	switchID := generateSwitchID()

	// Step 1: Get Ably token
	authResp, err := c.getAblyToken(ctx, switchID)
	if err != nil {
		return fmt.Errorf("get ably token: %w", err)
	}

	// Determine the main channel from capabilities
	// The Ably channel uses the NUMERIC user ID, not the Datastore key
	c.mu.Lock()
	numericID := c.numericUserID
	c.mu.Unlock()
	if numericID == 0 {
		return fmt.Errorf("numeric user ID not set — call GetCurrentUser first")
	}
	mainChannel := fmt.Sprintf("UserProfile-%d:main", numericID)

	// Build the client ID: {numeric_user_id}-{switch_id}
	clientID := fmt.Sprintf("%d-%s", numericID, switchID)

	// Step 2: Connect WebSocket
	wsURL := fmt.Sprintf(
		"%s/?access_token=%s&clientId=%s&format=json&heartbeats=true&v=5&agent=dialpad-bridge/1.0",
		ablyPushHost, authResp.Token, clientID,
	)

	wsCtx, cancel := context.WithCancel(ctx)

	conn, _, err := websocket.Dial(wsCtx, wsURL, nil)
	if err != nil {
		cancel()
		return fmt.Errorf("dial ably websocket: %w", err)
	}
	conn.SetReadLimit(1 << 20) // 1 MiB

	ws := &websocketConn{
		log:         c.Log.With().Str("component", "ably-push").Logger(),
		client:      c,
		ctx:         wsCtx,
		cancel:      cancel,
		conn:        conn,
		ablyToken:   authResp.Token,
		clientID:    clientID,
		switchID:    switchID,
		channels:    authResp.Capabilities,
		mainChannel: mainChannel,
	}

	c.mu.Lock()
	c.ws = ws
	c.connected = true
	c.mu.Unlock()

	ws.log.Info().
		Str("client_id", clientID).
		Str("main_channel", mainChannel).
		Msg("Connected to Ably push WebSocket")

	go ws.readLoop()
	go ws.tokenRefreshLoop()

	c.dispatchEvent(&Connected{})
	return nil
}

// Disconnect cleanly closes the connection to Dialpad.
func (c *Client) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected {
		return
	}

	c.Log.Info().Msg("Disconnecting from Dialpad")

	if c.ws != nil {
		c.ws.close()
		c.ws = nil
	}

	c.connected = false
	c.dispatchEvent(&Disconnected{Reason: "manual disconnect"})
}

// IsConnected returns whether the client has an active push connection.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// readLoop reads messages from the Ably WebSocket and dispatches events.
func (ws *websocketConn) readLoop() {
	defer func() {
		ws.client.mu.Lock()
		wasConnected := ws.client.connected
		ws.client.connected = false
		ws.client.mu.Unlock()

		if wasConnected {
			ws.client.dispatchEvent(&Disconnected{Reason: "read loop ended"})
		}
	}()

	for {
		select {
		case <-ws.ctx.Done():
			return
		default:
		}

		_, data, err := ws.conn.Read(ws.ctx)
		if err != nil {
			if ws.closed {
				return
			}
			ws.log.Warn().Err(err).Msg("Ably WebSocket read error")
			ws.reconnect()
			return
		}

		ws.handleAblyMessage(data)
	}
}

// handleAblyMessage parses an Ably protocol frame and takes appropriate action.
func (ws *websocketConn) handleAblyMessage(data []byte) {
	var msg ablyMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		ws.log.Warn().Err(err).Str("raw", string(data)).Msg("Failed to parse Ably message")
		return
	}

	switch msg.Action {
	case ablyActionHeartbeat:
		// Server-side keepalive ping. Do NOT echo — Ably treats an unsolicited
		// client HEARTBEAT as a new ping it must respond to, and the resulting
		// echo loop saturates the connection at >2000 frames/sec, tripping
		// Ably's abuse protection (code 42924). WebSocket-level liveness is
		// handled by the coder/websocket library.

	case ablyActionConnected:
		ws.log.Info().Str("connection_id", msg.ConnectionID).Msg("Ably connected, subscribing to channels")
		// Subscribe to the main user channel
		ws.sendJSON(ablyMessage{
			Action:  ablyActionAttach,
			Channel: ws.mainChannel,
		})

	case ablyActionAttached:
		ws.log.Info().Str("channel", msg.Channel).Msg("Subscribed to Ably channel")

	case ablyActionMessage:
		// This is a real event — unwrap the Ably envelope and dispatch
		ws.handleEventMessages(msg.Channel, msg.Messages)

	case ablyActionPresence:
		// Ignore presence events
		ws.log.Debug().Str("channel", msg.Channel).Msg("Ably presence event (ignored)")

	case ablyActionError:
		if msg.Error != nil {
			ws.log.Error().
				Int("code", msg.Error.Code).
				Str("msg", msg.Error.Message).
				Msg("Ably error")
			// Token expired or invalid — trigger reconnect with fresh token
			if msg.Error.StatusCode == 401 || msg.Error.Code == 40142 {
				ws.log.Warn().Msg("Ably token expired, reconnecting with fresh token")
				ws.reconnect()
			}
		}

	default:
		ws.log.Debug().Int("action", msg.Action).Str("channel", msg.Channel).Msg("Unhandled Ably action")
	}
}

// handleEventMessages processes actual Dialpad event data from Ably MESSAGE frames.
// Each Ably message has a "name" (event type) and "data" (the Dialpad event payload).
func (ws *websocketConn) handleEventMessages(channel string, messages []ablyMessageEntry) {
	for _, entry := range messages {
		ws.log.Debug().
			Str("channel", channel).
			Str("event_name", entry.Name).
			Str("data_preview", truncate(string(entry.Data), 200)).
			Msg("Received Ably push event")

		// The event data may be a JSON string or a JSON object.
		// If it's a string, we need to un-quote it first.
		eventData := entry.Data
		if len(eventData) > 0 && eventData[0] == '"' {
			var unquoted string
			if err := json.Unmarshal(eventData, &unquoted); err == nil {
				eventData = []byte(unquoted)
			}
		}

		// Try to parse as a known event type
		if err := ws.handleRawEvent(entry.Name, eventData); err != nil {
			ws.log.Warn().Err(err).Str("event_name", entry.Name).Msg("Failed to handle push event")
		}
	}
}

// handleRawEvent dispatches a Dialpad event based on its Ably event name or payload fields.
func (ws *websocketConn) handleRawEvent(eventName string, data []byte) error {
	if len(data) == 0 {
		return nil
	}

	// First try to identify by event name
	switch {
	case strings.Contains(eventName, "sms"),
		strings.Contains(eventName, "message"):
		return ws.handleSMSEvent(data)

	case strings.Contains(eventName, "call"),
		strings.Contains(eventName, "ringing"),
		strings.Contains(eventName, "voicemail"):
		return ws.handleCallEvent(data)
	}

	// Fallback: identify by field presence in the JSON payload
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Not JSON — log and skip
		ws.log.Debug().Str("event_name", eventName).Str("raw", truncate(string(data), 300)).Msg("Non-JSON event data")
		return nil
	}

	// Check "type" field — Ably push events have {"type":"call",...} or {"type":"sms",...}
	if typeRaw, hasType := raw["type"]; hasType {
		var eventType string
		if err := json.Unmarshal(typeRaw, &eventType); err == nil {
			switch eventType {
			case "call":
				return ws.handleCallEvent(data)
			case "sms", "mms", "message":
				return ws.handleSMSEvent(data)
			case "text_message":
				// Internal API push event — the SMS data is nested inside
				// a "text_message" object with a different field structure.
				return ws.handleTextMessageEvent(raw)
			}
		}
	}

	// Legacy field-based detection
	if _, hasSMS := raw["from_number"]; hasSMS {
		return ws.handleSMSEvent(data)
	}
	if _, hasCall := raw["call_uuid"]; hasCall {
		return ws.handleCallEvent(data)
	}
	if _, hasCall := raw["call_id"]; hasCall {
		return ws.handleCallEvent(data)
	}
	// Delta/presence updates — dispatch for contact sync.
	// If the delta contains "on_call", also signal the connector to poll
	// /api/activecalls/me for the actual call data (caller number, name, etc.).
	if _, hasDelta := raw["delta"]; hasDelta {
		ws.log.Debug().
			Str("event_name", eventName).
			Str("data", truncate(string(data), 1000)).
			Msg("Received delta event from Ably push")

		// Check if this delta signals an incoming call (presence → on_call).
		// The delta itself does NOT contain the caller's info — only that the
		// user's line is active. The connector will poll /api/activecalls/me.
		if strings.Contains(string(data), "on_call") {
			ws.log.Info().Msg("Delta contains on_call presence — signaling active call check")
			ws.client.dispatchEvent(&ActiveCallCheckEvent{})
		}

		ws.client.dispatchEvent(&DeltaEvent{Raw: data})
		return nil
	}

	ws.log.Info().
		Str("event_name", eventName).
		Str("raw", truncate(string(data), 500)).
		Msg("Unrecognized event type from Ably push — log full payload for analysis")
	return nil
}

// SMSEvent represents an incoming SMS event from Dialpad push.
type SMSEvent struct {
	ID                    int64    `json:"id"`
	CreatedDate           int64    `json:"created_date"`
	Direction             string   `json:"direction"`
	Target                Target   `json:"target"`
	Contact               Contact  `json:"contact"`
	SenderID              string   `json:"sender_id"`
	FromNumber            string   `json:"from_number"`
	ToNumber              []string `json:"to_number"`
	MMS                   bool     `json:"mms"`
	Text                  string   `json:"text"`
	MessageStatus         string   `json:"message_status"`
	MessageDeliveryResult string   `json:"message_delivery_result"`

	// MMS media fields
	MediaURLs []string `json:"media_urls,omitempty"`
	MediaURL  string   `json:"media_url,omitempty"`

	// MyNumber is set by handleTextMessageEvent when converting from the
	// internal push format. It indicates which of the user's Dialpad lines
	// is involved in this conversation (the "to_phone" for inbound,
	// "from_phone" for outbound).
	MyNumber string `json:"-"`

	// ClientID is the value we sent in the request body's "client_id" field.
	// Dialpad preserves it in outbound echoes — used as the stable identifier
	// for matching the echo to the pending send (works for MMS without
	// caption, where Text is empty). The server-rewritten FeedKey is NOT a
	// reliable match key.
	ClientID string `json:"-"`

	// MMSDetails carries the inbound MMS's original filename, content_type,
	// and dimensions. Used by the connector to set proper FileName/Body on
	// the Matrix m.image / m.video / m.audio event so received attachments
	// don't render as a generic "attachment" with no extension.
	MMSDetails *MMSDetails `json:"-"`
}

// Target is the internal Dialpad user/department/call center.
type Target struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number"`
}

// Contact is the external party in a call or SMS.
type Contact struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number"`
}

func (ws *websocketConn) handleSMSEvent(data []byte) error {
	var evt SMSEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		ws.log.Warn().Err(err).Msg("Failed to parse SMS event")
		return err
	}

	ws.log.Info().
		Int64("id", evt.ID).
		Str("direction", evt.Direction).
		Str("from", evt.FromNumber).
		Str("text_preview", truncate(evt.Text, 50)).
		Msg("Received SMS event via Ably push")

	ws.client.dispatchEvent(&evt)
	return nil
}

// TextMessagePush is the internal API push event for text messages.
// Received via Ably push with {"type":"text_message","text_message":{...}}.
// The nested object uses feed-style field names (contact_key, feed_date, etc.)
// instead of the v2 webhook format (from_number, to_number, etc.).
type TextMessagePush struct {
	ContactKey  string   `json:"contact_key"`
	FeedTarget  string   `json:"feed_target"`
	FeedKey     string   `json:"feed_key"` // Server-assigned canonical key; NOT the one we sent
	FeedType    string   `json:"feed_type"` // "TextMessage"
	FeedTags    []string `json:"feed_tags"` // e.g. ["inbox", "inbound", "text_message", "unread", "mms", "image"]
	FeedDate    int64    `json:"feed_date"` // epoch millis
	Text        string   `json:"text"`
	Orientation string   `json:"orientation"` // "internal" = sent by user, "external" = received
	FromPhone   string   `json:"from_phone"`
	ToPhone     string   `json:"to_phone"`
	ID          int64    `json:"id"`

	// ClientID is the value we sent in the request body's "client_id" field.
	// Dialpad preserves it end-to-end on outbound echoes (orientation=internal),
	// making it the only stable identifier for echo→pending matching. Inbound
	// echoes (external) don't include it. Verified via live API probe.
	ClientID string `json:"client_id,omitempty"`

	// MMS fields — populated for image/video messages
	DeliveryMethod string      `json:"delivery_method,omitempty"` // "sms", "mms", "dp"
	MMSURL         string      `json:"mms_url,omitempty"`
	ThumbURL       string      `json:"thumb_url,omitempty"`
	MMSDetails     *MMSDetails `json:"mms_details,omitempty"`
}

// MMSDetails contains metadata about an MMS attachment.
type MMSDetails struct {
	ContentType string `json:"content_type"` // e.g. "image/jpeg"
	Ext         string `json:"ext"`          // e.g. "jpeg"
	Type        string `json:"type"`         // e.g. "image", "video"
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int64  `json:"bytes"`
	Filename    string `json:"filename"`
}

// handleTextMessageEvent handles the internal API push event for text messages.
// The event payload has the SMS data nested under a "text_message" key.
func (ws *websocketConn) handleTextMessageEvent(raw map[string]json.RawMessage) error {
	tmRaw, hasTM := raw["text_message"]
	if !hasTM {
		ws.log.Warn().Msg("text_message push event missing 'text_message' field")
		return nil
	}

	// Full raw payload — used to debug delivery_method, mms_details, client_id,
	// and any other server-side fields we don't yet parse. Grep
	// "TextMessagePush raw payload" to inspect what Dialpad sends back.
	ws.log.Debug().
		RawJSON("text_message_raw", tmRaw).
		Msg("TextMessagePush raw payload")

	var tm TextMessagePush
	if err := json.Unmarshal(tmRaw, &tm); err != nil {
		ws.log.Warn().Err(err).Msg("Failed to parse text_message push payload")
		return err
	}

	// Determine direction from feed_tags or orientation
	direction := "inbound"
	if tm.Orientation == "internal" {
		direction = "outbound"
	}
	isMMS := false
	for _, tag := range tm.FeedTags {
		if tag == "outbound" {
			direction = "outbound"
		}
		if tag == "mms" {
			isMMS = true
		}
	}
	// Also check delivery_method
	if tm.DeliveryMethod == "mms" {
		isMMS = true
	}

	// Collect MMS media URL
	var mediaURLs []string
	if tm.MMSURL != "" {
		mediaURLs = append(mediaURLs, tm.MMSURL)
	}

	// Map to SMSEvent for the existing connector pipeline.
	// Set MyNumber so the connector knows which of the user's Dialpad lines
	// is involved. For internal push events, from_phone is the sender and
	// to_phone is the recipient — direction tells us which is "ours".
	var myNumber string
	if direction == "outbound" {
		myNumber = tm.FromPhone // We sent it → our number is the sender
	} else {
		myNumber = tm.ToPhone // We received it → our number is the recipient
	}

	evt := &SMSEvent{
		ID:          tm.ID,
		CreatedDate: tm.FeedDate,
		Direction:   direction,
		Text:        tm.Text,
		FromNumber:  tm.FromPhone,
		ToNumber:    []string{tm.ToPhone},
		MMS:         isMMS,
		MediaURLs:   mediaURLs,
		MyNumber:    myNumber,
		ClientID:    tm.ClientID,
		MMSDetails:  tm.MMSDetails,
		Contact: Contact{
			ID: tm.ContactKey,
		},
	}

	ws.log.Info().
		Int64("id", evt.ID).
		Str("direction", evt.Direction).
		Str("from", evt.FromNumber).
		Str("contact_key", tm.ContactKey).
		Str("client_id", tm.ClientID).
		Bool("mms", isMMS).
		Int("media_count", len(mediaURLs)).
		Str("text_preview", truncate(evt.Text, 50)).
		Msg("Received text_message push event via Ably")

	ws.client.dispatchEvent(evt)
	return nil
}

func (ws *websocketConn) handleCallEvent(data []byte) error {
	// Full raw payload for debugging (DateConnected, missed flag, full
	// CallDetail). Same diagnostic strategy as text_message events.
	ws.log.Debug().
		RawJSON("call_raw", data).
		Msg("CallEvent raw payload")

	var evt CallEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		ws.log.Warn().Err(err).Msg("Failed to parse call event")
		return err
	}

	ws.log.Info().
		Int64("call_uuid", evt.CallUUID).
		Str("state", evt.State).
		Str("direction", evt.GetDirection()).
		Str("contact", evt.GetExternalNumber()).
		Str("contact_name", evt.ContactName).
		Msg("Received call event via Ably push")

	ws.client.dispatchEvent(&evt)
	return nil
}

// sendJSON sends a JSON message on the Ably WebSocket.
func (ws *websocketConn) sendJSON(msg interface{}) {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	data, err := json.Marshal(msg)
	if err != nil {
		ws.log.Warn().Err(err).Msg("Failed to marshal Ably message")
		return
	}

	if err := ws.conn.Write(ws.ctx, websocket.MessageText, data); err != nil {
		ws.log.Warn().Err(err).Msg("Failed to send Ably message")
	}
}

// reconnect attempts to reconnect with exponential backoff and fresh Ably tokens.
func (ws *websocketConn) reconnect() {
	if ws.closed {
		return
	}

	backoff := reconnectBackoff
	for attempt := 1; !ws.closed; attempt++ {
		ws.log.Info().Int("attempt", attempt).Dur("backoff", backoff).Msg("Attempting Ably reconnect")

		select {
		case <-ws.ctx.Done():
			return
		case <-time.After(backoff):
		}

		// Get a fresh Ably token
		authResp, err := ws.client.getAblyToken(ws.ctx, ws.switchID)
		if err != nil {
			ws.log.Warn().Err(err).Msg("Failed to get fresh Ably token for reconnect")
			backoff = min(backoff*2, maxReconnectWait)
			continue
		}

		ws.ablyToken = authResp.Token

		// Build new WebSocket URL with fresh token
		wsURL := fmt.Sprintf(
			"%s/?access_token=%s&clientId=%s&format=json&heartbeats=true&v=5&agent=dialpad-bridge/1.0",
			ablyPushHost, ws.ablyToken, ws.clientID,
		)

		conn, _, err := websocket.Dial(ws.ctx, wsURL, nil)
		if err != nil {
			ws.log.Warn().Err(err).Int("attempt", attempt).Msg("Reconnect dial failed")
			backoff = min(backoff*2, maxReconnectWait)
			continue
		}

		conn.SetReadLimit(1 << 20)

		// Close old connection
		if ws.conn != nil {
			ws.conn.CloseNow()
		}
		ws.conn = conn

		ws.log.Info().Int("attempt", attempt).Msg("Ably WebSocket reconnected")

		ws.client.mu.Lock()
		ws.client.connected = true
		ws.client.mu.Unlock()
		ws.client.dispatchEvent(&Connected{})

		// Continue the read loop inline
		ws.readLoopInner()
		return
	}
}

// readLoopInner is the inner read loop used after reconnection.
func (ws *websocketConn) readLoopInner() {
	for {
		select {
		case <-ws.ctx.Done():
			return
		default:
		}

		_, data, err := ws.conn.Read(ws.ctx)
		if err != nil {
			if ws.closed {
				return
			}
			ws.log.Warn().Err(err).Msg("Ably read error after reconnect")
			ws.reconnect()
			return
		}

		ws.handleAblyMessage(data)
	}
}

// tokenRefreshLoop refreshes the Ably token before it expires (1 hour TTL).
// On refresh, it disconnects and reconnects with the new token since Ably
// doesn't support mid-connection token rotation on raw WebSocket.
func (ws *websocketConn) tokenRefreshLoop() {
	ticker := time.NewTicker(ablyTokenLifetime)
	defer ticker.Stop()

	for {
		select {
		case <-ws.ctx.Done():
			return
		case <-ticker.C:
			ws.log.Info().Msg("Refreshing Ably token (approaching 1-hour expiry)")

			authResp, err := ws.client.getAblyToken(ws.ctx, ws.switchID)
			if err != nil {
				ws.log.Err(err).Msg("Failed to refresh Ably token")
				continue
			}

			ws.ablyToken = authResp.Token
			ws.log.Info().Msg("Ably token refreshed, reconnecting with new token")

			// Close current connection — readLoop will trigger reconnect
			if ws.conn != nil {
				ws.conn.Close(websocket.StatusNormalClosure, "token refresh")
			}
		}
	}
}

func (ws *websocketConn) close() {
	if ws.closed {
		return
	}
	ws.closed = true
	ws.cancel()
	if ws.conn != nil {
		ws.conn.Close(websocket.StatusNormalClosure, "client disconnect")
	}
}

// generateSwitchID creates a random client identifier suffix.
func generateSwitchID() string {
	// Use timestamp-based ID for simplicity and uniqueness
	return fmt.Sprintf("bridge%d", time.Now().UnixMilli()%1000000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
