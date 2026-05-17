package connector

import (
	"strings"
	"sync"

	"github.com/beeper/dialpad-bridge/pkg/dialgo/types"
)

// contactCache provides O(1) phone-number-to-contact lookups.
// Thread-safe via RWMutex — reads are concurrent, writes are exclusive.
type contactCache struct {
	mu      sync.RWMutex
	byPhone map[string]*types.Contact // E.164 phone → contact
}

// Lookup returns the contact for the given phone number, or nil if not cached.
func (cc *contactCache) Lookup(phone string) *types.Contact {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.byPhone[phone]
}



// All returns all unique contacts in the cache (deduplicated by contact ID).
func (cc *contactCache) All() []types.Contact {
	cc.mu.RLock()
	defer cc.mu.RUnlock()

	seen := make(map[string]bool, len(cc.byPhone))
	var result []types.Contact
	for _, c := range cc.byPhone {
		key := c.ID
		if key == "" {
			key = c.PrimaryPhone
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, *c)
	}
	return result
}

// contactName returns the display name for a contact, or "" if none.
func contactName(c *types.Contact) string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	if c.FirstName != "" || c.LastName != "" {
		return strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	return ""
}


