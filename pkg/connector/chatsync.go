package connector

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/beeper/dialpad-bridge/pkg/dialgo"
)

// syncExistingConversations fetches the user's conversation list from the
// internal API and emits a ChatResync event for each one. This causes
// bridgev2 to create portals (rooms) and trigger FetchMessages (backfill)
// for conversations that existed before the bridge connected.
//
// Without this, the bridge is "invisible" to existing conversations —
// rooms only appear when a NEW message arrives via Ably push.
type syncEntry struct {
	portalKey  networkid.PortalKey
	contactKey string
}

func (da *DialpadAPI) syncExistingConversations() {
	ctx := da.bgCtx

	if da.meta.TargetKey == "" {
		da.log.Debug().Msg("No target_key, skipping conversation sync")
		return
	}

	userConvs, err := da.client.GetConversations(ctx, da.meta.TargetKey, 100)
	if err != nil {
		da.log.Warn().Err(err).Msg("Failed to fetch conversations from UserProfile target")
	}

	var officeConvs []dialgo.FeedContact
	if da.meta.OfficeKey != "" && da.meta.OfficeKey != da.meta.TargetKey {
		officeConvs, err = da.client.GetConversations(ctx, da.meta.OfficeKey, 100)
		if err != nil {
			da.log.Warn().Err(err).Msg("Failed to fetch conversations from Office target")
		}
	}

	conversations := mergeConversationsByContactKey(userConvs, officeConvs, da.meta.OfficeKey)
	if len(conversations) == 0 {
		da.log.Warn().Msg("No conversations returned from either target scope")
		return
	}

	da.log.Info().Int("count", len(conversations)).Msg("Syncing existing conversations")

	ownLines := map[string]bool{}
	for _, p := range da.meta.Phones {
		ownLines[p] = true
	}
	if da.meta.PrimaryPhone != "" {
		ownLines[da.meta.PrimaryPhone] = true
	}

	// Identify the first office line (typically there's only one). Used as
	// my_number for office-target chats that don't carry SelectedCallerID.
	var officeLine string
	for _, p := range da.meta.Phones {
		if p != "" && p != da.meta.PrimaryPhone {
			officeLine = p
			break
		}
	}

	for _, conv := range conversations {
		if conv.IsGroup() {
			da.syncGroupConversation(conv, ownLines, officeLine)
			continue
		}

		normalized, skipReason := classifyDMConv(conv, ownLines)
		if skipReason != "" {
			// "no phone" is a silent skip — matches prior behaviour and isn't
			// actionable. Self-reference cases get a debug line so a future
			// regression in the filter is visible in logs.
			if skipReason != "no phone" {
				da.log.Debug().
					Str("display_name", conv.DisplayName).
					Str("phone", normalized).
					Str("reason", skipReason).
					Msg("Skipping self-reference conversation")
			}
			continue
		}
		// SelectedCallerID is the user's line for this specific conversation.
		// When empty, infer from the target_key: an office-scoped conv lives
		// under the office line; otherwise default to the user's primary.
		// Without this split, personal- and office-line chats with the same
		// contact collide into one portal.
		myNumber := formatPhoneNumber(conv.SelectedCallerID)
		if myNumber == "" {
			if conv.TargetKey == da.meta.OfficeKey && officeLine != "" {
				myNumber = officeLine
			} else {
				myNumber = da.getMyNumber("")
			}
		}
		portalKey := networkid.PortalKey{
			ID:       MakeDMPortalID(myNumber, normalized),
			Receiver: da.login.ID,
		}

		da.log.Debug().
			Str("phone", normalized).
			Str("contact_key", conv.ContactKey).
			Str("display_name", conv.DisplayName).
			Str("my_number", myNumber).
			Str("target_key", conv.TargetKey).
			Int64("date_description", conv.DateDescription).
			Msg("Syncing conversation")

		convContactKey := conv.ContactKey
		convMyNumber := myNumber
		convTargetKey := conv.TargetKey
		if convContactKey != "" {
			da.contactKeyToPortal.Store(convContactKey, portalKey)
		}
		da.login.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				LogContext: func(c zerolog.Context) zerolog.Context {
					return c.
						Str("phone", normalized).
						Str("display_name", conv.DisplayName)
				},
			},
			ChatInfo: &bridgev2.ChatInfo{
				Name:        &conv.DisplayName,
				Topic:       ptr.Ptr(makeRoomTopic(myNumber)),
				Type:        ptr.Ptr(database.RoomTypeDM),
				CanBackfill: true,
				Members: &bridgev2.ChatMemberList{
					IsFull:      true,
					OtherUserID: networkid.UserID(normalized),
					MemberMap: bridgev2.ChatMemberMap{
						networkid.UserID(normalized): bridgev2.ChatMember{
							EventSender: bridgev2.EventSender{
								Sender: networkid.UserID(normalized),
							},
							Membership: event.MembershipJoin,
						},
						"": bridgev2.ChatMember{
							EventSender: bridgev2.EventSender{
								IsFromMe: true,
							},
							Membership: event.MembershipJoin,
						},
					},
				},
				ExtraUpdates: func(_ context.Context, p *bridgev2.Portal) bool {
					meta, ok := p.Metadata.(*PortalMetadata)
					if !ok || meta == nil {
						return false
					}
					changed := false
					if meta.ContactKey != convContactKey && convContactKey != "" {
						meta.ContactKey = convContactKey
						changed = true
					}
					if meta.MyNumber != convMyNumber && convMyNumber != "" {
						meta.MyNumber = convMyNumber
						changed = true
					}
					if meta.TargetKey != convTargetKey && convTargetKey != "" {
						meta.TargetKey = convTargetKey
						changed = true
					}
					return changed
				},
			},
			LatestMessageTS: latestMessageTime(conv),
		})
	}
}

// syncGroupConversation emits a ChatResync for a contact_group from the
// conversation list. Returns the sync entry to backfill, plus ok=false if
// the group should be skipped (no resolvable participants).
func (da *DialpadAPI) syncGroupConversation(conv dialgo.FeedContact, ownLines map[string]bool, officeLine string) {
	participants := resolveGroupParticipants(conv.Phones, ownLines)
	if participants == nil {
		da.log.Debug().Str("contact_key", conv.ContactKey).Str("display_name", conv.DisplayName).Msg("Skipping group with <2 external participants")
		return
	}
	portalKey := networkid.PortalKey{
		ID:       MakeGroupPortalID(participants),
		Receiver: da.login.ID,
	}

	myNumber := formatPhoneNumber(conv.SelectedCallerID)
	if myNumber == "" {
		if conv.TargetKey == da.meta.OfficeKey && officeLine != "" {
			myNumber = officeLine
		} else {
			myNumber = da.getMyNumber("")
		}
	}

	memberMap := bridgev2.ChatMemberMap{}
	for _, p := range participants {
		memberMap[networkid.UserID(p)] = bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{Sender: networkid.UserID(p)},
			Membership:  event.MembershipJoin,
		}
	}
	memberMap[""] = bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{IsFromMe: true},
		Membership:  event.MembershipJoin,
	}

	da.log.Debug().
		Str("contact_key", conv.ContactKey).
		Str("display_name", conv.DisplayName).
		Strs("participants", participants).
		Str("my_number", myNumber).
		Str("target_key", conv.TargetKey).
		Msg("Syncing group conversation")

	convTargetKey := conv.TargetKey
	if conv.ContactKey != "" {
		da.contactKeyToPortal.Store(conv.ContactKey, portalKey)
	}
	roomType := database.RoomTypeGroupDM
	da.login.QueueRemoteEvent(&simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    portalKey,
			CreatePortal: true,
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Str("display_name", conv.DisplayName).Str("contact_key", conv.ContactKey)
			},
		},
		ChatInfo: &bridgev2.ChatInfo{
			Name:        &conv.DisplayName,
			Topic:       ptr.Ptr(makeRoomTopic(myNumber)),
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
				if meta.ContactKey != conv.ContactKey {
					meta.ContactKey = conv.ContactKey
					changed = true
				}
				if meta.MyNumber != myNumber && myNumber != "" {
					meta.MyNumber = myNumber
					changed = true
				}
				if meta.TargetKey != convTargetKey && convTargetKey != "" {
					meta.TargetKey = convTargetKey
					changed = true
				}
				return changed
			},
		},
		LatestMessageTS: latestMessageTime(conv),
	})

}

// latestMessageTime returns the timestamp of the most recent interaction in a conversation,
// for use in ChatResync's LatestMessageTS field. Returns zero time if unknown.
func latestMessageTime(conv dialgo.FeedContact) time.Time {
	if conv.LastMessage != nil && conv.LastMessage.Date > 0 {
		return time.UnixMilli(conv.LastMessage.Date)
	}
	// Fallback: date_description is set by filter=recent for all interaction types
	// (calls, SMS, voicemails). This is essential for call-only contacts.
	if conv.DateDescription > 0 {
		return time.UnixMilli(conv.DateDescription)
	}
	return time.Time{}
}

// updateGhostName updates the ghost display name and portal title when
// a contact name is received from Ably push (e.g., call or SMS events).
func (da *DialpadAPI) updateGhostName(phone string, name string) {
	ctx := da.bgCtx

	ghostID := networkid.UserID(phone)
	ghost, err := da.connector.br.GetGhostByID(ctx, ghostID)
	if err != nil || ghost == nil {
		return
	}

	currentName := ghost.Name
	if currentName == name {
		return // no change
	}

	da.log.Info().
		Str("phone", phone).
		Str("old_name", currentName).
		Str("new_name", name).
		Msg("Updating ghost name from Ably event")

	ghost.UpdateInfo(ctx, &bridgev2.UserInfo{
		Name:        ptr.Ptr(name),
		Identifiers: []string{fmt.Sprintf("tel:%s", phone)},
	})

	// Also update portal title
	portalKey := networkid.PortalKey{
		ID:       MakeDMPortalID(da.getMyNumber(""), phone),
		Receiver: da.login.ID,
	}
	portal, err := da.connector.br.GetPortalByKey(ctx, portalKey)
	if err != nil || portal == nil {
		return
	}
	portal.UpdateInfo(ctx, &bridgev2.ChatInfo{
		Name: ptr.Ptr(name),
	}, da.login, nil, time.Time{})
}

// syncContactName fetches the contact's display name from the Dialpad API
// using the contact_key and updates the ghost name if it's a real name.
func (da *DialpadAPI) syncContactName(phone string, contactKey string) {
	ctx := da.bgCtx

	contact, err := da.client.GetContact(ctx, contactKey)
	if err != nil {
		da.log.Debug().Err(err).Str("contact_key", contactKey).Msg("Failed to fetch contact info")
		return
	}

	name := contact.DisplayName
	if name == "" && contact.FirstName != "" {
		name = contact.FirstName
		if contact.LastName != "" {
			name += " " + contact.LastName
		}
	}
	if name == "" {
		return
	}

	// Don't update if the name is just the phone number
	cleaned := cleanPhoneNumber(name)
	if cleaned == phone || cleaned == cleanPhoneNumber(contact.DialString) {
		return
	}

	da.updateGhostName(phone, name)
}

// contactNameSyncLoop periodically polls the Dialpad conversation list
// and updates ghost + portal names for any renamed contacts.
// Dialpad does NOT push contact rename events via Ably — it's REST-only —
// so polling is the only reliable way to detect renames.
func (da *DialpadAPI) contactNameSyncLoop() {
	const syncInterval = 2 * time.Minute

	// Wait a bit before first sync to let the bridge fully initialize
	select {
	case <-da.bgCtx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	da.log.Info().Dur("interval", syncInterval).Msg("Starting contact name sync loop")

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		da.doContactNameSync()

		select {
		case <-da.bgCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

// doContactNameSync fetches all conversations and updates ghost/portal names.
func (da *DialpadAPI) doContactNameSync() {
	ctx := da.bgCtx

	if da.meta.TargetKey == "" {
		return // Can't fetch conversations without target_key
	}

	contacts, err := da.client.GetConversations(ctx, da.meta.TargetKey, 100)
	if err != nil {
		da.log.Debug().Err(err).Msg("Contact name sync: failed to fetch conversations")
		return
	}

	for _, c := range contacts {
		if c.DisplayName == "" {
			continue
		}

		// Determine the phone number for this contact
		phone := c.DialString
		if phone == "" && c.PrimaryPhone != "" {
			phone = c.PrimaryPhone
		}
		if phone == "" && len(c.Phones) > 0 {
			phone = c.Phones[0]
		}
		if phone == "" {
			continue
		}

		normalized := cleanPhoneNumber(phone)
		if normalized == "" {
			continue
		}

		// Skip if display_name is just the phone number
		cleanedName := cleanPhoneNumber(c.DisplayName)
		if cleanedName == normalized {
			continue
		}

		// Check if the ghost name is different — only update if changed
		ghostID := networkid.UserID(normalized)
		ghost, err := da.connector.br.GetGhostByID(ctx, ghostID)
		if err != nil || ghost == nil {
			continue // Ghost doesn't exist — no portal for this number
		}

		if ghost.Name == c.DisplayName {
			continue // Already up to date
		}

		da.log.Info().
			Str("phone", normalized).
			Str("old_name", ghost.Name).
			Str("new_name", c.DisplayName).
			Msg("Contact name sync: detected rename")

		da.updateGhostName(normalized, c.DisplayName)
	}
}

// mergeConversationsByContactKey merges UserProfile- and Office-scoped
// conversation responses into one slice. Entries from the office scope that
// share a contact_key with a UserProfile entry are dropped. Office-scoped
// entries with empty TargetKey get stamped with officeKey so backfill
// queries the right scope. Entries with empty contact_key are kept (the
// self-reference filter runs in classifyDMConv at sync time, not here).
func mergeConversationsByContactKey(user, office []dialgo.FeedContact, officeKey string) []dialgo.FeedContact {
	seen := make(map[string]bool, len(user))
	merged := make([]dialgo.FeedContact, 0, len(user)+len(office))
	for _, c := range user {
		if c.ContactKey != "" {
			seen[c.ContactKey] = true
		}
		merged = append(merged, c)
	}
	for _, c := range office {
		if c.ContactKey != "" && seen[c.ContactKey] {
			continue
		}
		if c.ContactKey != "" {
			seen[c.ContactKey] = true
		}
		if c.TargetKey == "" {
			c.TargetKey = officeKey
		}
		merged = append(merged, c)
	}
	return merged
}

// classifyDMConv decides whether a DM-style FeedContact should be synced.
// Returns the normalized external phone (when resolvable) and a non-empty
// skipReason if the entry must be filtered out.
//
// Skip reasons:
//   - "empty contact_key": /api/contact/ returns self-reference rows
//     (the user's own line) with empty contact_key — they aren't real chats.
//   - "no phone": no DialString/PrimaryPhone — can't build a portal.
//   - "phone matches own line": defence-in-depth for self-reference rows
//     that *do* carry a contact_key but point at one of the user's lines.
func classifyDMConv(conv dialgo.FeedContact, ownLines map[string]bool) (normalizedPhone, skipReason string) {
	if conv.ContactKey == "" {
		return "", "empty contact_key"
	}
	raw := conv.DialString
	if raw == "" {
		raw = conv.PrimaryPhone
	}
	if raw == "" {
		return "", "no phone"
	}
	normalized := formatPhoneNumber(raw)
	if ownLines[normalized] {
		return normalized, "phone matches own line"
	}
	return normalized, ""
}

// resolveGroupParticipants filters a contact_group's phone list down to the
// external participants (drops own-line entries and unparseable numbers),
// sorts the result, and returns nil if fewer than 2 externals remain — the
// signal that the group should be skipped.
func resolveGroupParticipants(phones []string, ownLines map[string]bool) []string {
	parts := make([]string, 0, len(phones))
	for _, p := range phones {
		n := formatPhoneNumber(p)
		if n == "" || ownLines[n] {
			continue
		}
		parts = append(parts, n)
	}
	if len(parts) < 2 {
		return nil
	}
	sort.Strings(parts)
	return parts
}
