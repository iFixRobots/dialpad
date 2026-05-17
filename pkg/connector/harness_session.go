package connector

import (
	"encoding/json"
	"fmt"
	"strings"
)

type harnessSession struct {
	Type     string `json:"type"`
	ClientID string `json:"client_id"`
	TabID    string `json:"tab_id"`
	Auth     struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		// ExpiresIn is an absolute Unix-millisecond timestamp, not a duration.
		ExpiresIn int64  `json:"expires_in"`
		Target    string `json:"target"`
		UserID    int64  `json:"user_id"`
	} `json:"auth"`
	User struct {
		Key                 string   `json:"key"`
		ID                  string   `json:"id"`
		DisplayName         string   `json:"display_name"`
		FirstName           string   `json:"first_name"`
		LastName            string   `json:"last_name"`
		PrimaryEmail        string   `json:"primary_email"`
		CorrespondenceEmail string   `json:"correspondence_email"`
		CallerID            string   `json:"caller_id"`
		PrimaryPhone        string   `json:"primary_phone"`
		Phones              []string `json:"phones"`
		OfficeKey           string   `json:"office_key"`
		OfficeID            string   `json:"office_id"`
	} `json:"user"`
	Office struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"office"`
}

func parseHarnessSession(raw string) (*harnessSession, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.HasPrefix(raw, "{") {
		return nil, nil
	}
	var hs harnessSession
	if err := json.Unmarshal([]byte(raw), &hs); err != nil {
		return nil, fmt.Errorf("parse harness:session JSON: %w", err)
	}
	if hs.Auth.AccessToken == "" {
		return nil, fmt.Errorf("harness:session JSON had no auth.access_token")
	}
	return &hs, nil
}

func gatherCookies(submitted map[string]string) map[string]string {
	out := map[string]string{}
	if v := submitted["RHSID00"]; v != "" {
		out["RHSID00@.dialpad.com"] = v
	}
	if v := submitted["DP-ROUTING-BUCKET"]; v != "" {
		out["DP-ROUTING-BUCKET@dialpad.com"] = v
	}
	for _, name := range googleCookieNames {
		v := submitted["google_"+name]
		if v == "" {
			continue
		}
		domain := ".google.com"
		if strings.HasPrefix(name, "__Secure-") {
			domain = "accounts.google.com"
		}
		out[name+"@"+domain] = v
	}
	return out
}
