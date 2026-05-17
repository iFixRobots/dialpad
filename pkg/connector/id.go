package connector

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

// portalKind discriminates the two portal-ID shapes the bridge produces.
// Kept unexported — callers receive it from ParsePortalID and switch on it.
type portalKind int

const (
	portalKindUnknown portalKind = iota
	portalKindDM
	portalKindGroup
)

const (
	portalPrefixDM    = "sms:"
	portalPrefixGroup = "group:"
)

// MakeDMPortalID builds the DM portal-ID string for a conversation between
// one of the user's Dialpad lines and an external phone number.
//
// Format: "sms:{my_number}:{their_number}". Both numbers must be in E.164.
// The format is what's persisted in `portal.id` — do not change it without a
// migration plan.
func MakeDMPortalID(myNumber, otherNumber string) networkid.PortalID {
	return networkid.PortalID(fmt.Sprintf("%s%s:%s", portalPrefixDM, myNumber, otherNumber))
}

// MakeGroupPortalID builds the group portal-ID string from a slice of
// external participant phone numbers. The caller is responsible for sorting
// the slice — this matches today's behavior in CreateGroup and
// syncGroupConversation, both of which sort before calling.
//
// Format: "group:+ph1,+ph2,...". Phones must be in E.164 and must NOT
// include any of the user's own lines (filter via `ownLines` first).
func MakeGroupPortalID(sortedPhones []string) networkid.PortalID {
	return networkid.PortalID(portalPrefixGroup + strings.Join(sortedPhones, ","))
}

// ParsePortalID decodes a portal ID into its discriminated parts.
//
// For portalKindDM: myNumber and otherNumber are populated; groupPhones is nil.
// For portalKindGroup: groupPhones is populated; myNumber and otherNumber are empty.
// For portalKindUnknown or malformed input: ok=false and all out params are zero.
//
// Malformed cases that return ok=false:
//   - Empty input.
//   - "sms:" prefix with fewer than two parts (e.g. "sms:+14155550100").
//   - "sms:" prefix with empty either side (e.g. "sms::+1...").
//   - "group:" prefix with no phones (e.g. "group:").
//   - Any other prefix.
func ParsePortalID(id networkid.PortalID) (kind portalKind, myNumber, otherNumber string, groupPhones []string, ok bool) {
	s := string(id)
	switch {
	case strings.HasPrefix(s, portalPrefixDM):
		parts := strings.SplitN(strings.TrimPrefix(s, portalPrefixDM), ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return portalKindUnknown, "", "", nil, false
		}
		return portalKindDM, parts[0], parts[1], nil, true
	case strings.HasPrefix(s, portalPrefixGroup):
		rest := strings.TrimPrefix(s, portalPrefixGroup)
		if rest == "" {
			return portalKindUnknown, "", "", nil, false
		}
		phones := strings.Split(rest, ",")
		for _, p := range phones {
			if p == "" {
				return portalKindUnknown, "", "", nil, false
			}
		}
		return portalKindGroup, "", "", phones, true
	default:
		return portalKindUnknown, "", "", nil, false
	}
}

// IsGroupPortalID is a fast prefix check for code that only needs to
// disambiguate DM vs. group without parsing the parts.
func IsGroupPortalID(id networkid.PortalID) bool {
	return strings.HasPrefix(string(id), portalPrefixGroup)
}
