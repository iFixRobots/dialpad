package dialgo

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError represents a structured error from a Dialpad API response.
// It preserves the HTTP status code and response body for diagnosis.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
	Endpoint   string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Endpoint, e.StatusCode, e.Body)
	}
	return fmt.Sprintf("%s: HTTP %d %s", e.Endpoint, e.StatusCode, e.Status)
}

// IsAuthError returns true if this error indicates expired or invalid credentials.
func (e *APIError) IsAuthError() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// Is10DLCError returns true if the response body indicates a 10DLC compliance failure.
func (e *APIError) Is10DLCError() bool {
	lower := strings.ToLower(e.Body)
	return strings.Contains(lower, "10dlc") ||
		strings.Contains(lower, "a2p") ||
		strings.Contains(lower, "campaign") ||
		strings.Contains(lower, "registration") && strings.Contains(lower, "required")
}

// IsGroupSMSDisabled matches Dialpad's 400 response for accounts without bulk SMS entitlement.
func (e *APIError) IsGroupSMSDisabled() bool {
	if e.StatusCode != http.StatusBadRequest {
		return false
	}
	lower := strings.ToLower(e.Body)
	return strings.Contains(lower, "target is not eligible for messaging") ||
		strings.Contains(lower, "not eligible for messaging") ||
		strings.Contains(lower, "bulk sms") && strings.Contains(lower, "disabled")
}

// parseErrorResponse reads the error body from a non-2xx HTTP response and returns
// a structured APIError. The response body is consumed and closed.
func parseErrorResponse(endpoint string, resp *http.Response) *APIError {
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Endpoint:   endpoint,
	}

	// Read up to 4KB of error body — enough for structured JSON errors
	// without risking unbounded memory from a misbehaving server.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err == nil && len(body) > 0 {
		apiErr.Body = strings.TrimSpace(string(body))
	}

	return apiErr
}

// AuthErrorHandler is called when an API request receives a 401/403 response,
// indicating that the Bearer token has expired or is invalid.
type AuthErrorHandler func(err *APIError)
