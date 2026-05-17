package dialgo

import "runtime/debug"

// EventHandler is a function that handles incoming events from Dialpad.
type EventHandler func(evt interface{})

// Supported event types that handlers can type-switch on:
//   - *SMSEvent     — incoming/outgoing SMS via WebSocket
//   - *CallEvent    — incoming/outgoing call state changes via WebSocket
//   - *Connected    — WebSocket connection established
//   - *Disconnected — WebSocket connection lost

// AddEventHandler registers a handler that will be called for all events.
func (c *Client) AddEventHandler(handler EventHandler) {
	c.eventHandlersMu.Lock()
	defer c.eventHandlersMu.Unlock()
	c.eventHandlers = append(c.eventHandlers, handler)
}

func (c *Client) dispatchEvent(evt interface{}) {
	c.eventHandlersMu.RLock()
	handlers := make([]EventHandler, len(c.eventHandlers))
	copy(handlers, c.eventHandlers)
	c.eventHandlersMu.RUnlock()

	for _, handler := range handlers {
		go func(h EventHandler) {
			defer func() {
				if r := recover(); r != nil {
					c.Log.Error().
						Interface("panic", r).
						Str("stack", string(debug.Stack())).
						Msg("Panic in event handler (recovered)")
				}
			}()
			h(evt)
		}(handler)
	}
}

// Connection state events.
type Connected struct{}
type Disconnected struct {
	Reason string
}

// Note: SMSEvent is defined in websocket.go since it's tightly coupled
// to the WebSocket event parsing logic.
