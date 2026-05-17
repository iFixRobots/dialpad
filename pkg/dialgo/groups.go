package dialgo

import (
	"context"
	"fmt"
	"net/url"
)

type ContactGroupRequest struct {
	Type             string   `json:"type"`
	ContactKeys      []string `json:"contact_keys"`
	TargetKey        string   `json:"target_key"`
	SelectedCallerID string   `json:"selected_caller_id"`
}

type ContactGroupResponse struct {
	ContactKey      string `json:"contact_key"`
	DisplayName     string `json:"display_name"`
	CanSMS          bool   `json:"can_sms"`
	SMSErrorDetails string `json:"sms_error_details"`
}

func (c *Client) CreateContactGroup(ctx context.Context, req *ContactGroupRequest) (*ContactGroupResponse, error) {
	if req == nil || len(req.ContactKeys) < 2 || req.TargetKey == "" || req.SelectedCallerID == "" {
		return nil, fmt.Errorf("create contact group: invalid request (need ≥2 contact_keys, target_key, selected_caller_id)")
	}
	req.Type = "contact_group"
	return postJSON[*ContactGroupResponse](ctx, c, "/contact/", url.Values{"is_affinity": {"1"}}, req, "create contact group")
}

type PhoneLookupResponse struct {
	ContactKey   string `json:"contact_key"`
	DisplayName  string `json:"display_name"`
	PrimaryPhone string `json:"primary_phone"`
}

func (c *Client) LookupContactByPhone(ctx context.Context, e164, officeKey string) (*PhoneLookupResponse, error) {
	if e164 == "" {
		return nil, fmt.Errorf("phone is required")
	}
	var params url.Values
	if officeKey != "" {
		params = url.Values{"target_key": {officeKey}}
	}
	return getJSON[*PhoneLookupResponse](ctx, c, "/phone/"+url.PathEscape(e164), params, "lookup contact by phone")
}
