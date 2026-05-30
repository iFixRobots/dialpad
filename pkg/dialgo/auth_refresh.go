package dialgo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ErrRefreshInteractionRequired = fmt.Errorf("silent OAuth rejected by Google; interactive re-auth required")
	ErrRefreshNoCookies           = fmt.Errorf("no Google session cookies stored; cannot refresh silently")
)

const fallbackBearerTTL = 30 * 24 * time.Hour

type RefreshResult struct {
	BearerToken string
	ExpiresAt   time.Time
	Email       string
}

func (c *Client) RefreshBearer(ctx context.Context, loginHint string) (*RefreshResult, error) {
	if c.HTTP.Jar == nil {
		return nil, ErrRefreshNoCookies
	}
	if !hasCookie(c.HTTP.Jar, "RHSID00") && !hasCookie(c.HTTP.Jar, "SAPISID") {
		return nil, ErrRefreshNoCookies
	}

	googleAuthURL, err := c.fetchOAuthInitiation(ctx)
	if err != nil {
		return nil, fmt.Errorf("step 1 (initiate): %w", err)
	}
	c.Log.Debug().Str("google_url", redactQuery(googleAuthURL)).Msg("OAuth refresh: got initiation URL")

	silentURL, err := promptNone(googleAuthURL, loginHint)
	if err != nil {
		return nil, fmt.Errorf("step 2 (prompt rewrite): %w", err)
	}

	callbackURL, err := c.silentGoogleConsent(ctx, silentURL)
	if err != nil {
		return nil, fmt.Errorf("step 3 (Google silent consent): %w", err)
	}
	c.Log.Debug().Str("callback_url", redactQuery(callbackURL)).Msg("OAuth refresh: Google approved")

	chainResult, err := c.followDialpadCallback(ctx, callbackURL)
	if err != nil {
		return nil, fmt.Errorf("step 4 (Dialpad callback): %w", err)
	}
	c.Log.Debug().
		Str("final_url", chainResult.finalURL).
		Int("status", chainResult.status).
		Int("body_len", chainResult.bodyLen).
		Msg("OAuth refresh: callback chain complete")

	result, err := c.extractBearer(ctx, chainResult)
	if err != nil {
		return nil, fmt.Errorf("step 5 (bearer extraction): %w", err)
	}
	if result.Email == "" {
		result.Email = loginHint
	}

	c.SetBearerToken(result.BearerToken)
	return result, nil
}

func (c *Client) fetchOAuthInitiation(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://dialpad.com/auth/google/request?action=login", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.doNonRedirect(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("expected 302, got %d: %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if loc == "" || !strings.Contains(loc, "accounts.google.com") {
		return "", fmt.Errorf("unexpected redirect target: %q", loc)
	}
	return loc, nil
}

func promptNone(googleURL string, loginHint string) (string, error) {
	u, err := url.Parse(googleURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("prompt", "none")
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) silentGoogleConsent(ctx context.Context, silentURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", silentURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.doNonRedirect(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusFound {
		loc := resp.Header.Get("Location")
		if loc == "" {
			return "", fmt.Errorf("Google returned 302 with no Location")
		}
		if strings.Contains(loc, "error=") {
			return "", fmt.Errorf("%w: %s", ErrRefreshInteractionRequired, redactQuery(loc))
		}
		if !strings.Contains(loc, "dialpad.com") {
			return "", fmt.Errorf("unexpected redirect target: %s", redactQuery(loc))
		}
		return loc, nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if strings.Contains(string(body), "interaction_required") ||
		strings.Contains(string(body), "login_required") ||
		strings.Contains(string(body), "consent_required") {
		return "", ErrRefreshInteractionRequired
	}
	return "", fmt.Errorf("Google returned %d (not a redirect): %s", resp.StatusCode, snippet(string(body)))
}

type chainResult struct {
	finalURL    string
	status      int
	bodyLen     int
	body        string
	idToken     string
	finalHeader http.Header
}

func (c *Client) followDialpadCallback(ctx context.Context, callbackURL string) (*chainResult, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", callbackURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))

	res := &chainResult{
		finalURL:    resp.Request.URL.String(),
		status:      resp.StatusCode,
		bodyLen:     len(body),
		body:        string(body),
		finalHeader: resp.Header.Clone(),
	}

	if u, err := url.Parse(res.finalURL); err == nil {
		if id := u.Query().Get("id_token"); id != "" {
			res.idToken = id
		} else if frag := u.Fragment; frag != "" {
			if vals, err := url.ParseQuery(frag); err == nil {
				if id := vals.Get("id_token"); id != "" {
					res.idToken = id
				}
			}
		}
	}
	return res, nil
}

func (c *Client) extractBearer(ctx context.Context, chain *chainResult) (*RefreshResult, error) {
	if token := sessionKeyRegex.FindStringSubmatch(chain.body); len(token) == 2 {
		expiry := time.Now().Add(fallbackBearerTTL)
		return &RefreshResult{BearerToken: token[1], ExpiresAt: expiry}, nil
	}

	if chain.idToken != "" {
		bearer, expiry, email, err := c.mintBearerFromIDToken(ctx, chain.idToken)
		if err == nil {
			return &RefreshResult{BearerToken: bearer, ExpiresAt: expiry, Email: email}, nil
		}
		c.Log.Debug().Err(err).Msg("mintBearerFromIDToken failed; trying fallback paths")
	}

	if user, err := c.GetCurrentUser(ctx); err == nil {
		return nil, fmt.Errorf("cookies still authenticate /api/user/me (user=%s) but no new Bearer surfaced anywhere (final_url=%s)", user.ID, chain.finalURL)
	}

	return nil, fmt.Errorf("could not extract Bearer (final_url=%s, status=%d, body_len=%d, id_token_present=%v)", chain.finalURL, chain.status, chain.bodyLen, chain.idToken != "")
}

func (c *Client) mintBearerFromIDToken(ctx context.Context, idToken string) (string, time.Time, string, error) {
	candidates := []struct {
		path   string
		method string
	}{
		{"/api/userlogin/", "POST"},
		{"/api/user_login/", "POST"},
		{"/api/login/", "POST"},
		{"/api/session/", "POST"},
		{"/api/auth/login", "POST"},
	}
	body := map[string]any{
		"session_key":    idToken,
		"remote_service": "google",
		"type":           "harness",
	}
	bodyJSON, _ := json.Marshal(body)

	var lastErr error
	for _, cand := range candidates {
		req, err := http.NewRequestWithContext(ctx, cand.method,
			"https://dialpad.com"+cand.path, strings.NewReader(string(bodyJSON)))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("%s %s → %d: %s", cand.method, cand.path, resp.StatusCode, snippet(string(bodyBytes)))
			continue
		}
		var parsed struct {
			Auth struct {
				AccessToken string `json:"access_token"`
				ExpiresIn   int64  `json:"expires_in"`
			} `json:"auth"`
			AccessToken string `json:"access_token"`
			ExpiresIn   int64  `json:"expires_in"`
			Email       string `json:"email"`
		}
		if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
			lastErr = fmt.Errorf("%s %s → 200 but unparseable: %s", cand.method, cand.path, snippet(string(bodyBytes)))
			continue
		}
		token := parsed.Auth.AccessToken
		if token == "" {
			token = parsed.AccessToken
		}
		exp := parsed.Auth.ExpiresIn
		if exp == 0 {
			exp = parsed.ExpiresIn
		}
		if token == "" {
			lastErr = fmt.Errorf("%s %s → 200 but no token field in response", cand.method, cand.path)
			continue
		}
		c.Log.Info().Str("endpoint", cand.path).Msg("Bearer minted via SPA login endpoint")
		expiry := time.UnixMilli(exp)
		if exp > 0 && exp < 1e12 {
			expiry = time.Now().Add(time.Duration(exp) * time.Second)
		}
		return token, expiry, parsed.Email, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate endpoint succeeded")
	}
	return "", time.Time{}, "", lastErr
}

func (c *Client) doNonRedirect(req *http.Request) (*http.Response, error) {
	client := &http.Client{
		Jar: c.HTTP.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client.Do(req)
}

func (c *Client) ShouldRefresh(expiresAt time.Time, withinWindow time.Duration) bool {
	if expiresAt.IsZero() {
		return false
	}
	return time.Until(expiresAt) <= withinWindow
}

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

var sessionKeyRegex = regexp.MustCompile(`document\.session_key\s*=\s*["']([^"']{8,})["']`)

func redactQuery(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for k := range q {
		if strings.Contains(strings.ToLower(k), "token") ||
			strings.Contains(strings.ToLower(k), "code") ||
			k == "state" {
			q.Set(k, "<redacted>")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func snippet(s string) string {
	const limit = 240
	s = strings.TrimSpace(s)
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}
