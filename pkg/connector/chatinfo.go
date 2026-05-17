package connector

import (
	"context"
	"fmt"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

func (da *DialpadAPI) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	// Only DM portals are described here; groups assemble their own ChatInfo
	// at create/sync time. An unparseable ID can't be enriched — return a
	// minimal ChatInfo rather than panicking on empty fields.
	_, myNumber, externalNumber, _, ok := ParsePortalID(portal.ID)
	if !ok {
		return &bridgev2.ChatInfo{}, nil
	}

	roomType := database.RoomTypeDM

	// Use contact name as chat title if available, otherwise raw phone number
	chatName := externalNumber
	if da.contacts != nil {
		if c := da.contacts.Lookup(externalNumber); c != nil {
			if c.DisplayName != "" {
				chatName = c.DisplayName
			} else if c.FirstName != "" || c.LastName != "" {
				chatName = strings.TrimSpace(c.FirstName + " " + c.LastName)
			}
		}
	}

	// Fallback: fetch from the internal conversations API
	if chatName == externalNumber {
		if da.meta.TargetKey != "" {
			contacts, err := da.client.GetConversations(ctx, da.meta.TargetKey, 100)
			if err == nil {
				normalized := cleanPhoneNumber(externalNumber)
				for _, c := range contacts {
					cPhone := cleanPhoneNumber(c.DialString)
					if cPhone == "" {
						cPhone = cleanPhoneNumber(c.PrimaryPhone)
					}
					if cPhone == normalized && c.DisplayName != "" {
						cleanedName := cleanPhoneNumber(c.DisplayName)
						if cleanedName != normalized {
							chatName = c.DisplayName
							break
						}
					}
				}
			}
		}
	}

	members := &bridgev2.ChatMemberList{
		IsFull:      true,
		OtherUserID: networkid.UserID(externalNumber),
		MemberMap: bridgev2.ChatMemberMap{
			networkid.UserID(externalNumber): bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{
					Sender: networkid.UserID(externalNumber),
				},
				Membership: event.MembershipJoin,
			},
		},
	}
	// Add the bridge user as a member
	members.MemberMap.Set(bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{
			IsFromMe: true,
		},
		Membership: event.MembershipJoin,
	})

	return &bridgev2.ChatInfo{
		Name:    ptr.Ptr(chatName),
		Topic:   ptr.Ptr(makeRoomTopic(myNumber)),
		Members: members,
		Type:    &roomType,
	}, nil
}

func (da *DialpadAPI) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	phoneNumber := string(ghost.ID)

	// Use the phone number as the display name by default
	displayName := phoneNumber
	var contactKey string

	// O(1) lookup from cached contacts
	if da.contacts != nil {
		if c := da.contacts.Lookup(phoneNumber); c != nil {
			if c.DisplayName != "" {
				displayName = c.DisplayName
			} else if c.FirstName != "" || c.LastName != "" {
				displayName = strings.TrimSpace(c.FirstName + " " + c.LastName)
			}
		}
	}

	// Fetch contact details from the internal conversations API.
	// Used for both display name (fallback) and contactKey (for avatar).
	if da.meta.TargetKey != "" {
		contacts, err := da.client.GetConversations(ctx, da.meta.TargetKey, 100)
		if err == nil {
			normalized := cleanPhoneNumber(phoneNumber)
			for _, c := range contacts {
				cPhone := cleanPhoneNumber(c.DialString)
				if cPhone == "" {
					cPhone = cleanPhoneNumber(c.PrimaryPhone)
				}
				if cPhone == normalized {
					contactKey = c.ContactKey
					if displayName == phoneNumber && c.DisplayName != "" {
						cleanedName := cleanPhoneNumber(c.DisplayName)
						if cleanedName != normalized {
							displayName = c.DisplayName
						}
					}
					break
				}
			}
		}
	}

	info := &bridgev2.UserInfo{
		Name:        ptr.Ptr(displayName),
		Identifiers: []string{fmt.Sprintf("tel:%s", phoneNumber)},
	}

	// Fetch avatar from full contact info
	if contactKey != "" {
		contactInfo, err := da.client.GetContact(ctx, contactKey)
		if err == nil && contactInfo.ImageURL != "" {
			info.Avatar = da.makeAvatar(contactInfo.ImageURL)
		}
	}

	return info, nil
}

// makeAvatar creates a bridgev2.Avatar from a Dialpad image URL.
// The URL is used as the avatar ID — if it changes, the avatar is re-uploaded.
func (da *DialpadAPI) makeAvatar(imageURL string) *bridgev2.Avatar {
	client := da.client
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(imageURL),
		Get: func(ctx context.Context) ([]byte, error) {
			return client.DownloadMedia(ctx, imageURL)
		},
	}
}
