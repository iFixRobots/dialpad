package dialgo

// DeltaEvent represents a delta/patch event from Ably push.
// These events contain JSON Patch operations on user profiles, contacts, etc.
// The payload arrives as: {"delta": [...operations...], ...}
//
// Example delta payload (contact name change):
//
//	{
//	  "delta": [
//	    {"op": "replace", "path": "/display_name", "value": "New Name"}
//	  ],
//	  "key": "agxzfnViZXItdm9pY2VyFAsSB0NvbnRhY3QY..."
//	}
type DeltaEvent struct {
	Raw []byte // Full JSON payload — parsed downstream
}

// ActiveCallCheckEvent signals the connector to poll /api/activecalls/me.
// Dispatched by the websocket layer when a delta event indicates the user's
// presence changed to "on_call". The connector then fetches the actual call
// data (caller number, contact name, ringing state) from the REST API.
type ActiveCallCheckEvent struct{}
