package connector

import (
	"context"
	"fmt"

	"github.com/nyaruka/phonenumbers"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"

	"go.mau.fi/util/configupgrade"
)

type UserLoginMetadata struct {
	BearerToken   string            `json:"bearer_token"`
	TargetKey     string            `json:"target_key,omitempty"`
	OfficeKey     string            `json:"office_key,omitempty"`
	PrimaryPhone  string            `json:"primary_phone,omitempty"`
	Phones        []string          `json:"phones,omitempty"`
	ExpiresAt     int64             `json:"expires_at,omitempty"`
	Email         string            `json:"email,omitempty"`
	Cookies       map[string]string `json:"cookies,omitempty"`
	PreferredLine string            `json:"preferred_line,omitempty"`
}

type PortalMetadata struct {
	ContactKey string `json:"contact_key,omitempty"`
	MyNumber   string `json:"my_number,omitempty"`
	// TargetKey is the conversation's per-contact target (UserProfile for
	// personal-line chats, Office for office-line chats). Used as target_key
	// when querying /api/feed/. Falls back to UserLoginMetadata.OfficeKey
	// if unset (legacy portals).
	TargetKey string `json:"target_key,omitempty"`
}

// GhostMetadata stores Dialpad-specific ghost data (placeholder for future use).
type GhostMetadata struct{}

// MessageMetadata stores Dialpad-specific message data (placeholder for future use).
type MessageMetadata struct{}

// DialpadConnector implements bridgev2.NetworkConnector for the Dialpad network.
type DialpadConnector struct {
	br     *bridgev2.Bridge
	log    zerolog.Logger
	Config DialpadConfig
}

var (
	_ bridgev2.NetworkConnector = (*DialpadConnector)(nil)
	_ bridgev2.StoppableNetwork = (*DialpadConnector)(nil)
)

// Declare optional bridgev2 capabilities on the per-login API client.
var (
	_ bridgev2.IdentifierResolvingNetworkAPI = (*DialpadAPI)(nil)
	_ bridgev2.GroupCreatingNetworkAPI       = (*DialpadAPI)(nil)
	_ bridgev2.ContactListingNetworkAPI      = (*DialpadAPI)(nil)
)

func NewDialpadConnector() *DialpadConnector {
	return &DialpadConnector{}
}

func (dc *DialpadConnector) Init(bridge *bridgev2.Bridge) {
	dc.br = bridge
	dc.log = bridge.Log.With().Str("connector", "dialpad").Logger()
	if processor, ok := bridge.Commands.(*commands.Processor); ok {
		processor.AddHandlers(cmdLines, cmdSetLine, cmdClearLine, cmdExpiry, cmdRefreshToken)
	}
}

func (dc *DialpadConnector) Start(ctx context.Context) error {
	dc.log.Info().Msg("Starting Dialpad connector")
	return nil
}

func (dc *DialpadConnector) Stop() {
	dc.log.Info().Msg("Stopping Dialpad connector")
}

func (dc *DialpadConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "Dialpad",
		NetworkURL:       "https://dialpad.com",
		NetworkIcon:      "", // Upload to homeserver, then set MXC URI
		NetworkID:        "dialpad",
		BeeperBridgeType: "sh-dialpad",
		DefaultPort:      29334,
	}
}

func (dc *DialpadConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
		Portal: func() any {
			return &PortalMetadata{}
		},
		Ghost: func() any {
			return &GhostMetadata{}
		},
		Message: func() any {
			return &MessageMetadata{}
		},
	}
}

func (dc *DialpadConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		DisappearingMessages: false,
		AggressiveUpdateInfo: false,
		Provisioning: bridgev2.ProvisioningCapabilities{
			ResolveIdentifier: bridgev2.ResolveIdentifierCapabilities{
				CreateDM:    true,
				LookupPhone: true,
				AnyPhone:    true,
				ContactList: true,
			},
			GroupCreation: map[string]bridgev2.GroupTypeCapabilities{
				"group": {
					TypeDescription: "Group SMS",
					Name:            bridgev2.GroupFieldCapability{Allowed: true},
					Participants:    bridgev2.GroupFieldCapability{Allowed: true, Required: true, MinLength: 2},
				},
			},
		},
	}
}

// DialpadConfig is the bridge-specific configuration.
type DialpadConfig struct {
	// UseSandbox uses the sandbox API instead of production.
	UseSandbox bool `yaml:"use_sandbox"`
	// SMSRateLimit is the max SMS sends per minute (100 for Tier 0, 800 for Tier 1).
	SMSRateLimit int `yaml:"sms_rate_limit"`
	// CallStartNotices bridges incoming call events as Matrix messages.
	CallStartNotices bool `yaml:"call_start_notices"`
}

func (dc *DialpadConnector) GetConfig() (example string, data any, upgrader configupgrade.Upgrader) {
	return `# Dialpad bridge configuration
# Use the sandbox API for development/testing
use_sandbox: false
# Maximum SMS sends per minute (100 = Tier 0, 800 = Tier 1)
sms_rate_limit: 100
# Bridge incoming call events as messages (like WhatsApp call notices)
call_start_notices: true
`, &dc.Config, configupgrade.NoopUpgrader
}

func (dc *DialpadConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Sign in with Google",
		Description: "Sign in via the embedded Google flow. The bridge captures your Dialpad and Google session cookies so it can attempt silent token refreshes later.",
		ID:          LoginFlowIDCookies,
	}}
}

func (dc *DialpadConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowIDCookies {
		return nil, fmt.Errorf("unknown login flow ID %q", flowID)
	}
	return &DialpadLogin{
		connector: dc,
		user:      user,
		log:       dc.log.With().Str("user", string(user.MXID)).Logger(),
	}, nil
}

func (dc *DialpadConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	log := dc.log.With().Str("login_id", string(login.ID)).Logger()
	api := &DialpadAPI{
		connector: dc,
		login:     login,
		meta:      meta,
		log:       log,
	}
	api.newClient()
	login.Client = api
	return nil
}

func (dc *DialpadConnector) GetBridgeInfoVersion() (info, capabilities int) {
	// Bump `capabilities` when GetCapabilities changes — Beeper Desktop
	// caches the response per (info, capabilities) tuple, so an unchanged
	// version means clients keep using the old caps and won't pick up
	// e.g. newly-allowed sticker/file types.
	return 1, 2
}

// formatPhoneNumber normalizes a phone number to E.164 format for use in portal/ghost IDs.
// Uses Google's libphonenumber for full international support. If parsing fails
// (e.g., the input is not a valid phone number), the original value is returned as-is.
// Default region is US for numbers without a country code prefix.
func formatPhoneNumber(number string) string {
	if number == "" {
		return ""
	}

	parsed, err := phonenumbers.Parse(number, "US")
	if err != nil {
		return number
	}

	if !phonenumbers.IsValidNumber(parsed) {
		return number
	}

	return phonenumbers.Format(parsed, phonenumbers.E164)
}

// formatDisplayPhone formats a phone number for human display.
// E.g., "+14155550100" → "+1 (415) 555-0100".
// Falls back to the raw string if parsing fails.
func formatDisplayPhone(number string) string {
	if number == "" {
		return ""
	}
	parsed, err := phonenumbers.Parse(number, "US")
	if err != nil {
		return number
	}
	return phonenumbers.Format(parsed, phonenumbers.NATIONAL)
}

// makeRoomTopic returns the Matrix room topic for a portal,
// surfacing which Dialpad line the conversation is on.
func makeRoomTopic(myNumber string) string {
	if myNumber == "" {
		return "Dialpad"
	}
	return fmt.Sprintf("📞 via %s · Dialpad", formatDisplayPhone(myNumber))
}

// cleanPhoneNumber strips common formatting characters from phone numbers
// like "(415) 555-0102" → "4155550102". If the result has 10+ digits,
// it's likely a valid phone number.
func cleanPhoneNumber(number string) string {
	if number == "" {
		return ""
	}
	var digits []byte
	hasPlus := false
	for i, c := range number {
		if c == '+' && i == 0 {
			hasPlus = true
			continue
		}
		if c >= '0' && c <= '9' {
			digits = append(digits, byte(c))
		}
	}
	if len(digits) < 7 {
		return "" // too short to be a real phone number
	}
	if hasPlus {
		return "+" + string(digits)
	}
	return string(digits)
}
