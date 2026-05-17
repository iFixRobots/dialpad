package dialgo

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/rs/zerolog"
)

const (
	// BaseURL is the Dialpad API base URL (public API v2).
	// Kept for any future public API usage.
	BaseURL = "https://dialpad.com/api/v2"
	// SandboxBaseURL is the sandbox API base URL.
	SandboxBaseURL = "https://sandbox.dialpad.com/api/v2"
	// InternalBaseURL is the internal web client API base URL.
	// Used for authentication, events, message history, and user info.
	InternalBaseURL = "https://dialpad.com/api"
)

// Client is the main Dialpad protocol client.
// It uses Bearer token auth for all API calls, matching the web client's
// authentication model (Google OAuth → Bearer token in Authorization header).
// Real-time events are received via Ably push (wss://realtime.push.dialpad.com).
type Client struct {
	Log  zerolog.Logger
	HTTP *http.Client

	// APIBaseURL allows overriding for sandbox. Defaults to BaseURL.
	APIBaseURL string

	// InternalAPIBaseURL is used for internal API calls. Defaults to InternalBaseURL.
	InternalAPIBaseURL string

	// SMSRateLimiter enforces per-minute SMS rate limits.
	SMSRateLimiter *SMSRateLimiter

	// OnAuthError is called when any API request receives a 401/403,
	// indicating that the Bearer token has expired.
	OnAuthError AuthErrorHandler

	// bearerToken is the API token used in Authorization headers
	bearerToken string

	// Ably push WebSocket state
	ws *websocketConn

	eventHandlers   []EventHandler
	eventHandlersMu sync.RWMutex

	connected      bool
	userID         string
	numericUserID  int64  // Numeric user ID for Ably channels
	mu             sync.Mutex
}

// NewClient creates a new Dialpad client.
func NewClient(sessionStore interface{}, log zerolog.Logger) *Client {
	jar, _ := newCookieJar(nil)
	return &Client{
		Log:                log,
		HTTP:               &http.Client{Jar: jar},
		APIBaseURL:         BaseURL,
		InternalAPIBaseURL: InternalBaseURL,
	}
}

// LoadCookies pre-populates the cookie jar from a domain-scoped map
// (see UserLoginMetadata.Cookies). Replaces any existing jar contents.
func (c *Client) LoadCookies(stored map[string]string) error {
	jar, err := newCookieJar(stored)
	if err != nil {
		return err
	}
	c.HTTP.Jar = jar
	return nil
}

// ExportCookies returns the current cookie jar state for persistence.
func (c *Client) ExportCookies() map[string]string {
	return exportCookies(c.HTTP.Jar)
}

// SetBearerToken sets the API token used for all HTTP requests.
// This token is obtained after Google OAuth login and is sent in the
// Authorization: Bearer header on every request.
func (c *Client) SetBearerToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bearerToken = token
}



// GetCurrentUser fetches the current user from the internal API and caches
// the user ID on the client (required for Ably channel subscription).
// GET /api/user/me
func (c *Client) GetCurrentUser(ctx context.Context) (*InternalUser, error) {
	user, err := getJSON[*InternalUser](ctx, c, "/user/me", nil, "get current user")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.userID = user.ID
	if uid, err := user.UserID.Int64(); err == nil {
		c.numericUserID = uid
	}
	c.mu.Unlock()
	return user, nil
}

// IsLoggedIn returns whether the client has a valid Bearer token.
func (c *Client) IsLoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bearerToken != ""
}

// GetUserID returns the logged-in user's Dialpad ID.
func (c *Client) GetUserID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID
}


// doAPIRequest executes an HTTP request and checks the response for auth errors.
// Injects the Authorization: Bearer header on every request.
// If the response is not the expected status code, it returns a structured APIError
// with the response body preserved. On 401/403, it also fires the OnAuthError callback.
func (c *Client) doAPIRequest(req *http.Request, expectedStatus int, endpoint string) (*http.Response, error) {
	// Inject Bearer token auth header
	c.mu.Lock()
	token := c.bearerToken
	c.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}

	if resp.StatusCode != expectedStatus {
		apiErr := parseErrorResponse(endpoint, resp)
		resp.Body.Close()

		if apiErr.IsAuthError() && c.OnAuthError != nil {
			c.OnAuthError(apiErr)
		}

		return nil, apiErr
	}

	return resp, nil
}
