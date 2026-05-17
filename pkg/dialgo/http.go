package dialgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Generic JSON request helpers on *Client. Every method in this file goes
// through doAPIRequest, so the Authorization bearer header, the
// OnAuthError fire on 401/403, and the APIError body capture on non-200 all
// behave identically to bespoke endpoint methods.
//
// Method bodies become single-line wrappers, e.g.:
//
//	func (c *Client) GetContact(ctx context.Context, key string) (*ContactInfo, error) {
//	    return getJSON[*ContactInfo](ctx, c, "/contact/"+key, nil, "get contact")
//	}
//
// Endpoints with bespoke payload logging or non-JSON bodies (UploadFile,
// GetMessageHistory) keep their own code paths — see those methods for why.

// getJSON sends a GET to InternalAPIBaseURL+path and decodes the JSON body
// into T. params (when non-nil) is appended as the query string.
func getJSON[T any](ctx context.Context, c *Client, path string, params url.Values, opName string) (T, error) {
	return doJSON[T](ctx, c, http.MethodGet, path, params, nil, opName)
}

// postJSON sends a POST with body marshalled as JSON and decodes the response
// into T. Use json.RawMessage as T for endpoints whose response we ignore.
func postJSON[T any](ctx context.Context, c *Client, path string, params url.Values, body any, opName string) (T, error) {
	return doJSON[T](ctx, c, http.MethodPost, path, params, body, opName)
}

// patchJSON sends a PATCH with body marshalled as JSON and decodes the
// response into T.
func patchJSON[T any](ctx context.Context, c *Client, path string, params url.Values, body any, opName string) (T, error) {
	return doJSON[T](ctx, c, http.MethodPatch, path, params, body, opName)
}

// doJSON is the shared request/decode loop behind the three exported helpers.
func doJSON[T any](ctx context.Context, c *Client, method, path string, params url.Values, body any, opName string) (T, error) {
	var zero T
	if !c.IsLoggedIn() {
		return zero, ErrNotLoggedIn
	}

	reqURL := c.InternalAPIBaseURL + path
	if len(params) > 0 {
		reqURL += "?" + params.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		marshalled, err := json.Marshal(body)
		if err != nil {
			return zero, fmt.Errorf("marshal %s: %w", opName, err)
		}
		reqBody = bytes.NewReader(marshalled)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, reqBody)
	if err != nil {
		return zero, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.doAPIRequest(req, http.StatusOK, opName)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, fmt.Errorf("read %s body: %w", opName, err)
	}

	c.Log.Debug().
		Str("endpoint", opName).
		Str("method", method).
		Str("url", reqURL).
		Int("response_len", len(respBody)).
		Msg("API response")

	var out T
	if len(respBody) == 0 {
		// Some endpoints (e.g., PATCH /textmessage/) return an empty body on
		// success. Returning the zero value lets callers use json.RawMessage
		// as T for "I don't care about the response" cases.
		return out, nil
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		preview := string(respBody)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return zero, fmt.Errorf("decode %s (body=%s): %w", opName, preview, err)
	}
	return out, nil
}
