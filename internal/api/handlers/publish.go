package handlers

import (
	"github.com/moby/moby/api/types/events"
)

// publishEvent is a convenience wrapper for publishing lifecycle events.
func (h *Handlers) publishEvent(t events.Type, action events.Action, id string, attrs map[string]string) {
	if h.events == nil {
		return
	}

	h.events.Publish(events.Message{
		Type:   t,
		Action: action,
		Actor: events.Actor{
			ID:         id,
			Attributes: attrs,
		},
	})
}
