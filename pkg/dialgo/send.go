package dialgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// SMSRequest is the request body for POST /api/v2/sms.
type SMSRequest struct {
	ToNumbers        []string `json:"to_numbers,omitempty"`
	Text             string   `json:"text,omitempty"`
	UserID           int64    `json:"user_id,omitempty"`
	FromNumber       string   `json:"from_number,omitempty"`
	ChannelHashtag   string   `json:"channel_hashtag,omitempty"`
	Media            string   `json:"media,omitempty"`
	InferCountryCode bool     `json:"infer_country_code,omitempty"`
	SenderGroupID    int64    `json:"sender_group_id,omitempty"`
	SenderGroupType  string   `json:"sender_group_type,omitempty"`
}

// SMSResponse is the response from POST /api/v2/sms.
type SMSResponse struct {
	ID json.Number `json:"id"`
}

// SendSMS posts to the public /api/v2/sms endpoint (unused — kept for parity).
func (c *Client) SendSMS(ctx context.Context, req *SMSRequest) (*SMSResponse, error) {
	if !c.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}

	// Respect SMS rate limit before sending
	if c.SMSRateLimiter != nil {
		if err := c.SMSRateLimiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limit wait: %w", err)
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal SMS request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.APIBaseURL+"/sms", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")


	resp, err := c.doAPIRequest(httpReq, http.StatusOK, "send SMS")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var smsResp SMSResponse
	if err := json.NewDecoder(resp.Body).Decode(&smsResp); err != nil {
		return nil, fmt.Errorf("decode SMS response: %w", err)
	}

	return &smsResp, nil
}


