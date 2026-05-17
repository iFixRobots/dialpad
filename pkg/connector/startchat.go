package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// ResolveIdentifier handles "start-chat" and "resolve-identifier" bot commands.
// For an SMS bridge, the identifier is always a phone number. We normalize it to E.164,
// create the portal key, and optionally provision the ghost with contact info.
func (da *DialpadAPI) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	normalized := formatPhoneNumber(identifier)
	if normalized == "" {
		return nil, fmt.Errorf("invalid phone number: %q", identifier)
	}

	userID := networkid.UserID(normalized)

	displayName := normalized
	if da.contacts != nil {
		if c := da.contacts.Lookup(normalized); c != nil {
			if name := contactName(c); name != "" {
				displayName = name
			}
		}
	}

	ghost, err := da.connector.br.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get ghost: %w", err)
	}
	userInfo := &bridgev2.UserInfo{
		Name:        ptr.Ptr(displayName),
		Identifiers: []string{fmt.Sprintf("tel:%s", normalized)},
	}

	resp := &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   userID,
		UserInfo: userInfo,
	}

	if createChat {
		myNumber := da.getMyNumber("")
		roomType := database.RoomTypeDM
		resp.Chat = &bridgev2.CreateChatResponse{
			PortalKey: networkid.PortalKey{
				ID:       MakeDMPortalID(myNumber, normalized),
				Receiver: da.login.ID,
			},
			PortalInfo: &bridgev2.ChatInfo{
				Name:        ptr.Ptr(displayName),
				Topic:       ptr.Ptr(makeRoomTopic(myNumber)),
				Type:        &roomType,
				CanBackfill: true,
				Members: &bridgev2.ChatMemberList{
					IsFull:      true,
					OtherUserID: userID,
					MemberMap: bridgev2.ChatMemberMap{
						userID: bridgev2.ChatMember{
							EventSender: bridgev2.EventSender{Sender: userID},
							Membership:  event.MembershipJoin,
						},
						"": bridgev2.ChatMember{
							EventSender: bridgev2.EventSender{IsFromMe: true},
							Membership:  event.MembershipJoin,
						},
					},
				},
				ExtraUpdates: func(_ context.Context, p *bridgev2.Portal) bool {
					meta, ok := p.Metadata.(*PortalMetadata)
					if !ok || meta == nil {
						return false
					}
					if meta.MyNumber != myNumber && myNumber != "" {
						meta.MyNumber = myNumber
						return true
					}
					return false
				},
			},
		}
	}

	return resp, nil
}

func (da *DialpadAPI) CreateGroup(ctx context.Context, params *bridgev2.GroupCreateParams) (*bridgev2.CreateChatResponse, error) {
	if len(params.Participants) < 2 {
		return nil, fmt.Errorf("need at least 2 participants to create a group")
	}

	var phones []string
	for _, uid := range params.Participants {
		normalized := formatPhoneNumber(string(uid))
		if normalized == "" {
			return nil, fmt.Errorf("invalid phone number: %q", uid)
		}
		phones = append(phones, normalized)
	}
	sort.Strings(phones)

	contactKeys, err := da.resolveContactKeysForGroup(ctx, phones)
	if err != nil {
		return nil, fmt.Errorf("resolve participant contacts: %w", err)
	}

	fromLine := da.getMyNumber("")
	if fromLine == "" {
		return nil, fmt.Errorf("no outbound Dialpad line configured — set one with !dialpad set-line")
	}
	if da.meta.OfficeKey == "" {
		return nil, fmt.Errorf("office key is missing — re-login to populate it")
	}

	resp, err := da.client.CreateContactGroup(ctx, &dialgo.ContactGroupRequest{
		ContactKeys:      contactKeys,
		TargetKey:        da.meta.OfficeKey,
		SelectedCallerID: fromLine,
	})
	if err != nil {
		var apiErr *dialgo.APIError
		if errors.As(err, &apiErr) && apiErr.IsGroupSMSDisabled() {
			da.log.Warn().Str("body", apiErr.Body).Strs("phones", phones).Msg("Dialpad rejected group create")
			return nil, fmt.Errorf("%s", ErrGroupSMSDisabled)
		}
		return nil, fmt.Errorf("Dialpad group create failed: %w", err)
	}

	if !resp.CanSMS {
		da.log.Warn().
			Str("contact_key", resp.ContactKey).
			Str("sms_error_details", resp.SMSErrorDetails).
			Strs("phones", phones).
			Msg("Dialpad created group but disabled SMS on it")
		return nil, fmt.Errorf("%s", groupSMSBlockedMessage(resp.SMSErrorDetails))
	}

	da.log.Info().Str("contact_key", resp.ContactKey).Str("from_line", fromLine).Strs("phones", phones).Msg("Dialpad group created")

	memberMap := bridgev2.ChatMemberMap{}
	for _, phone := range phones {
		memberMap[networkid.UserID(phone)] = bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{Sender: networkid.UserID(phone)},
			Membership:  event.MembershipJoin,
		}
	}
	memberMap.Set(bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{IsFromMe: true},
		Membership:  event.MembershipJoin,
	})

	groupName := ""
	if params.Name != nil && params.Name.Name != "" {
		groupName = params.Name.Name
	} else if resp.DisplayName != "" {
		groupName = resp.DisplayName
	} else {
		var names []string
		for _, phone := range phones {
			name := phone
			if da.contacts != nil {
				if c := da.contacts.Lookup(phone); c != nil {
					if n := contactName(c); n != "" {
						name = n
					}
				}
			}
			names = append(names, name)
		}
		groupName = strings.Join(names, ", ")
	}

	roomType := database.RoomTypeGroupDM
	portalKey := networkid.PortalKey{
		ID:       MakeGroupPortalID(phones),
		Receiver: da.login.ID,
	}
	if resp.ContactKey != "" {
		da.contactKeyToPortal.Store(resp.ContactKey, portalKey)
	}
	return &bridgev2.CreateChatResponse{
		PortalKey: portalKey,
		PortalInfo: &bridgev2.ChatInfo{
			Name:        ptr.Ptr(groupName),
			Topic:       ptr.Ptr(makeRoomTopic(fromLine)),
			Type:        &roomType,
			CanBackfill: true,
			Members: &bridgev2.ChatMemberList{
				IsFull:    true,
				MemberMap: memberMap,
			},
			ExtraUpdates: func(_ context.Context, p *bridgev2.Portal) bool {
				meta, ok := p.Metadata.(*PortalMetadata)
				if !ok || meta == nil {
					return false
				}
				changed := false
				if meta.ContactKey != resp.ContactKey {
					meta.ContactKey = resp.ContactKey
					changed = true
				}
				if meta.MyNumber != fromLine {
					meta.MyNumber = fromLine
					changed = true
				}
				if meta.TargetKey != da.meta.OfficeKey && da.meta.OfficeKey != "" {
					meta.TargetKey = da.meta.OfficeKey
					changed = true
				}
				return changed
			},
		},
	}, nil
}

func (da *DialpadAPI) resolveContactKeysForGroup(ctx context.Context, phones []string) ([]string, error) {
	if da.client == nil {
		return nil, fmt.Errorf("client not ready")
	}
	keys := make([]string, 0, len(phones))
	for _, phone := range phones {
		lookup, err := da.client.LookupContactByPhone(ctx, phone, da.meta.OfficeKey)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", phone, err)
		}
		if lookup.ContactKey == "" {
			return nil, fmt.Errorf("Dialpad has no contact for %s — add them in Dialpad first", phone)
		}
		keys = append(keys, lookup.ContactKey)
	}
	return keys, nil
}

// GetContactList returns the cached Dialpad contact list for the Beeper contact picker.
func (da *DialpadAPI) GetContactList(ctx context.Context) ([]*bridgev2.ResolveIdentifierResponse, error) {
	if da.contacts == nil {
		return nil, nil
	}

	entries := da.contacts.All()
	resp := make([]*bridgev2.ResolveIdentifierResponse, 0, len(entries))
	for _, c := range entries {
		phone := c.PrimaryPhone
		if phone == "" && len(c.Phones) > 0 {
			phone = c.Phones[0]
		}
		if phone == "" {
			continue
		}
		normalized := formatPhoneNumber(phone)
		if normalized == "" {
			continue
		}

		name := contactName(&c)
		if name == "" {
			name = normalized
		}

		resp = append(resp, &bridgev2.ResolveIdentifierResponse{
			UserID: networkid.UserID(normalized),
			UserInfo: &bridgev2.UserInfo{
				Name:        ptr.Ptr(name),
				Identifiers: []string{fmt.Sprintf("tel:%s", normalized)},
			},
		})
	}
	return resp, nil
}
