package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

const (
	LoginFlowIDCookies  = "cookies"
	LoginStepIDCookies  = "fi.mau.dialpad.cookies"
	LoginStepIDComplete = "fi.mau.dialpad.complete"
)

type DialpadLogin struct {
	connector *DialpadConnector
	user      *bridgev2.User
	log       zerolog.Logger
}

var _ bridgev2.LoginProcessCookies = (*DialpadLogin)(nil)

var googleCookieNames = []string{
	"SID", "HSID", "SSID", "APISID", "SAPISID",
	"__Secure-1PSID", "__Secure-3PSID",
	"__Secure-1PSIDTS", "__Secure-3PSIDTS",
	"__Secure-1PAPISID", "__Secure-3PAPISID",
	"OSID", "ACCOUNT_CHOOSER",
}

// The Beeper/x.y.z product token in front of Chrome/x.y.z.w opts out of
// Google's embedded-browser block ("This browser or app may not be secure").
const loginUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) " +
	"Beeper/3.110.1 Chrome/126.0.0.0 Safari/537.36"

var cookieLoginStep = &bridgev2.LoginStep{
	Type:   bridgev2.LoginStepTypeCookies,
	StepID: LoginStepIDCookies,
	Instructions: "Sign in to Dialpad with Google in the embedded browser. The bridge captures both your Dialpad session token and Google session cookies " +
		"so it can silently refresh the token every ~30 days without bothering you. Make sure to actually click 'Sign in with Google' rather than just pasting a token.",
	CookiesParams: &bridgev2.LoginCookiesParams{
		// Skip Dialpad's login chooser (Google / Microsoft / email+password)
		// and go straight to the Google OAuth flow. The bridge only supports
		// the Google path — silent refresh in particular requires the Google
		// session cookies — so landing on a chooser invites users to click
		// the wrong button. This URL 302-redirects to accounts.google.com.
		URL:       "https://dialpad.com/auth/google/request?action=login",
		UserAgent: loginUserAgent,
		Fields: append([]bridgev2.LoginCookieField{
			{
				ID:       "authorization_token",
				Required: true,
				Sources: []bridgev2.LoginCookieFieldSource{{
					Type:         bridgev2.LoginCookieTypeRequestHeader,
					Name:         "Authorization",
					CookieDomain: "dialpad.com",
				}},
			},
			{
				ID:       "harness_session",
				Required: false,
				Sources: []bridgev2.LoginCookieFieldSource{{
					Type:         bridgev2.LoginCookieTypeLocalStorage,
					Name:         "harness:session",
					CookieDomain: "dialpad.com",
				}},
			},
			{
				ID:       "RHSID00",
				Required: false,
				Sources: []bridgev2.LoginCookieFieldSource{{
					Type:         bridgev2.LoginCookieTypeCookie,
					Name:         "RHSID00",
					CookieDomain: ".dialpad.com",
				}},
			},
			{
				ID:       "DP-ROUTING-BUCKET",
				Required: false,
				Sources: []bridgev2.LoginCookieFieldSource{{
					Type:         bridgev2.LoginCookieTypeCookie,
					Name:         "DP-ROUTING-BUCKET",
					CookieDomain: "dialpad.com",
				}},
			},
		}, googleCookieFields()...),
	},
}

func googleCookieFields() []bridgev2.LoginCookieField {
	out := make([]bridgev2.LoginCookieField, 0, len(googleCookieNames))
	for _, name := range googleCookieNames {
		out = append(out, bridgev2.LoginCookieField{
			ID:       "google_" + name,
			Required: false,
			Sources: []bridgev2.LoginCookieFieldSource{
				{Type: bridgev2.LoginCookieTypeCookie, Name: name, CookieDomain: ".google.com"},
				{Type: bridgev2.LoginCookieTypeCookie, Name: name, CookieDomain: "accounts.google.com"},
			},
		})
	}
	return out
}

func (dl *DialpadLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return cookieLoginStep, nil
}

func (dl *DialpadLogin) Cancel() {
	dl.log.Info().Msg("Login cancelled")
}

func (dl *DialpadLogin) SubmitCookies(ctx context.Context, cookies map[string]string) (*bridgev2.LoginStep, error) {
	hs, err := parseHarnessSession(cookies["harness_session"])
	if err != nil {
		dl.log.Warn().Err(err).Msg("Could not parse harness:session; falling back to Authorization header")
	}

	token := ""
	if hs != nil {
		token = hs.Auth.AccessToken
	}
	if token == "" {
		token = cookies["authorization_token"]
		if token == "" {
			token = cookies["Authorization"]
		}
		token = strings.TrimSpace(token)
		token = strings.TrimPrefix(token, "Bearer ")
	}
	if token == "" {
		return nil, fmt.Errorf("could not extract a Bearer token — make sure you signed in to Dialpad with Google in the embedded browser")
	}

	return dl.completeLoginWithToken(ctx, token, hs, gatherCookies(cookies))
}

// completeLoginWithToken validates the token against Dialpad, persists the
// UserLogin row, and kicks off Connect.
func (dl *DialpadLogin) completeLoginWithToken(
	ctx context.Context,
	token string,
	hs *harnessSession,
	storedCookies map[string]string,
) (*bridgev2.LoginStep, error) {
	var expiresAt int64
	var targetKey, officeKey, email, displayName, primaryPhone string
	var phones []string
	if hs != nil {
		expiresAt = hs.Auth.ExpiresIn
		targetKey = hs.Auth.Target
		officeKey = hs.User.OfficeKey
		email = hs.User.PrimaryEmail
		if email == "" {
			email = hs.User.CorrespondenceEmail
		}
		displayName = hs.User.DisplayName
		if displayName == "" {
			displayName = strings.TrimSpace(hs.User.FirstName + " " + hs.User.LastName)
		}
		primaryPhone = formatPhoneNumber(hs.User.PrimaryPhone)
		if primaryPhone == "" {
			primaryPhone = formatPhoneNumber(hs.User.CallerID)
		}
		for _, p := range hs.User.Phones {
			if normalized := formatPhoneNumber(p); normalized != "" {
				phones = append(phones, normalized)
			}
		}
	}

	dl.log.Info().Msg("Attempting Dialpad login")

	client := dialgo.NewClient(nil, dl.log)
	client.SetBearerToken(token)
	if storedCookies == nil {
		storedCookies = map[string]string{}
	}
	if err := client.LoadCookies(storedCookies); err != nil {
		dl.log.Warn().Err(err).Msg("Could not seed cookie jar from login submission")
	}

	user, err := client.GetCurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w (the token may have expired — sign in again at dialpad.com)", err)
	}

	userID := user.ID
	if displayName == "" {
		displayName = user.DisplayName
	}
	if displayName == "" {
		displayName = strings.TrimSpace(user.FirstName + " " + user.LastName)
	}
	if primaryPhone == "" {
		primaryPhone = formatPhoneNumber(user.CallerID)
	}
	if len(phones) == 0 {
		for _, p := range user.Phones {
			if normalized := formatPhoneNumber(p); normalized != "" {
				phones = append(phones, normalized)
			}
		}
	}
	if primaryPhone != "" {
		idx := -1
		for i, p := range phones {
			if p == primaryPhone {
				idx = i
				break
			}
		}
		if idx > 0 {
			phones[0], phones[idx] = phones[idx], phones[0]
		} else if idx == -1 {
			phones = append([]string{primaryPhone}, phones...)
		}
	}

	remoteName := displayName
	loginID := networkid.UserLoginID(userID)

	hasGoogleSession := false
	for k := range storedCookies {
		if strings.Contains(k, "@.google.com") || strings.Contains(k, "@accounts.google.com") {
			hasGoogleSession = true
			break
		}
	}
	dl.log.Info().
		Str("user_id", userID).
		Str("display_name", displayName).
		Str("primary_phone", primaryPhone).
		Strs("phones", phones).
		Bool("has_expiry", expiresAt != 0).
		Bool("has_google_cookies", hasGoogleSession).
		Bool("has_dialpad_cookie", storedCookies["RHSID00@.dialpad.com"] != "").
		Msg("Dialpad login successful")

	ul, err := dl.user.NewLogin(ctx, &database.UserLogin{
		ID:         loginID,
		RemoteName: remoteName,
		Metadata: &UserLoginMetadata{
			BearerToken:  token,
			TargetKey:    targetKey,
			OfficeKey:    officeKey,
			PrimaryPhone: primaryPhone,
			Phones:       phones,
			ExpiresAt:    expiresAt,
			Email:        email,
			Cookies:      storedCookies,
		},
	}, &bridgev2.NewLoginParams{
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			meta := login.Metadata.(*UserLoginMetadata)
			api := &DialpadAPI{
				connector: dl.connector,
				login:     login,
				meta:      meta,
				log:       dl.log.With().Str("login_id", string(login.ID)).Logger(),
				client:    client,
			}
			login.Client = api
			return nil
		},
		DeleteOnConflict: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create user login: %w", err)
	}

	go ul.Client.Connect(context.Background())

	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       LoginStepIDComplete,
		Instructions: fmt.Sprintf("Successfully logged in as %s", displayName),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: loginID,
			UserLogin:   ul,
		},
	}, nil
}
