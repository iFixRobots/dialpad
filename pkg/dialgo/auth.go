package dialgo

import (
	"context"
	"errors"
)

var (
	ErrNotLoggedIn  = errors.New("not logged in")
	ErrTokenExpired = errors.New("token expired")
)

// Logout clears the session and disconnects.
func (c *Client) Logout(ctx context.Context) error {
	c.Disconnect()

	c.mu.Lock()
	c.bearerToken = ""
	c.userID = ""
	c.mu.Unlock()

	return nil
}
