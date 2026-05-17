package dialgo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/beeper/dialpad-bridge/pkg/dialgo/types"
)

// DownloadMedia downloads a media attachment by its URL with authentication.
func (c *Client) DownloadMedia(ctx context.Context, mediaURL string) ([]byte, error) {
	reader, err := c.DownloadMediaReader(ctx, mediaURL)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

// DownloadMediaReader returns a reader for streaming media downloads.
func (c *Client) DownloadMediaReader(ctx context.Context, mediaURL string) (io.ReadCloser, error) {
	if !c.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}

	req, err := http.NewRequestWithContext(ctx, "GET", mediaURL, nil)
	if err != nil {
		return nil, err
	}


	resp, err := c.doAPIRequest(req, http.StatusOK, "download media")
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}



// GetUser fetches a user by ID. Use "me" to get the current user.
// GET /api/v2/users/{id}. Rate limit: 1200/min.
func (c *Client) GetUser(ctx context.Context, userID string) (*types.User, error) {
	if !c.IsLoggedIn() {
		return nil, ErrNotLoggedIn
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.APIBaseURL+"/users/"+userID, nil)
	if err != nil {
		return nil, err
	}


	resp, err := c.doAPIRequest(req, http.StatusOK, "get user")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var user types.User
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("decode user: %w", err)
	}

	return &user, nil
}
