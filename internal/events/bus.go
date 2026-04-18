// Package events provides a goroutine-safe event fan-out bus for
// container, image, network, and volume lifecycle events.
package events

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
)

const subscriberBufSize = 256

// Filters controls which events reach a subscriber.
type Filters struct {
	Types      []events.Type
	Actions    []events.Action
	Containers []string
	Images     []string
	Networks   []string
	Volumes    []string
	Labels     []string
}

type subscriber struct {
	ch      chan events.Message
	filters Filters
}

// Bus fans out published events to all matching subscribers.
type Bus struct {
	subscribers map[uint64]*subscriber
	nextID      uint64
	mu          sync.RWMutex
}

// Subscription represents an active event subscription.
type Subscription struct {
	C  <-chan events.Message
	b  *Bus
	id uint64
}

// New creates a ready-to-use event bus.
func New() *Bus {
	return &Bus{
		subscribers: make(map[uint64]*subscriber),
	}
}

// Publish sends msg to every subscriber whose filters match.
// If a subscriber's buffer is full the message is dropped for that subscriber.
func (b *Bus) Publish(msg events.Message) { //nolint:gocritic // Value semantics intentional for event broadcasting.
	now := time.Now()
	if msg.Time == 0 {
		msg.Time = now.Unix()
	}

	if msg.TimeNano == 0 {
		msg.TimeNano = now.UnixNano()
	}

	if msg.Scope == "" {
		msg.Scope = "local"
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, sub := range b.subscribers {
		if !matchesFilters(msg, sub.filters) {
			continue
		}

		select {
		case sub.ch <- msg:
		default: // slow consumer — drop
		}
	}
}

// Subscribe returns a Subscription whose channel receives matching events.
// The caller must call Close when done.
func (b *Bus) Subscribe(f Filters) *Subscription { //nolint:gocritic // Value semantics intentional.
	ch := make(chan events.Message, subscriberBufSize)

	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subscribers[id] = &subscriber{ch: ch, filters: f}
	b.mu.Unlock()

	return &Subscription{C: ch, id: id, b: b}
}

// Close removes the subscription and closes its channel.
func (s *Subscription) Close() {
	s.b.mu.Lock()
	sub, ok := s.b.subscribers[s.id]
	delete(s.b.subscribers, s.id)
	s.b.mu.Unlock()

	if ok {
		close(sub.ch)
	}
}

// ParseFilters parses Docker-style ?filters={"type":["container"],...} from query values.
func ParseFilters(vals url.Values) Filters {
	raw := vals.Get("filters")
	if raw == "" {
		return Filters{}
	}

	var parsed map[string][]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return Filters{}
	}

	var f Filters

	for _, v := range parsed["type"] {
		f.Types = append(f.Types, events.Type(v))
	}

	for _, v := range parsed["event"] {
		f.Actions = append(f.Actions, events.Action(v))
	}

	f.Containers = parsed["container"]
	f.Images = parsed["image"]
	f.Networks = parsed["network"]
	f.Volumes = parsed["volume"]
	f.Labels = parsed["label"]

	return f
}

// matchesFilters returns true if msg passes all non-empty filter lists.
func matchesFilters(msg events.Message, f Filters) bool { //nolint:gocritic // Value semantics for internal filter check.
	if len(f.Types) > 0 && !containsType(f.Types, msg.Type) {
		return false
	}

	if len(f.Actions) > 0 && !containsAction(f.Actions, msg.Action) {
		return false
	}

	if !matchesActorFilter(msg, f) {
		return false
	}

	if len(f.Labels) > 0 && !matchesLabels(f.Labels, msg.Actor.Attributes) {
		return false
	}

	return true
}

// matchesActorFilter checks type-specific actor filters (container, image, network, volume).
func matchesActorFilter(msg events.Message, f Filters) bool { //nolint:gocritic // Value semantics for internal filter check.
	var filter []string

	switch msg.Type { //nolint:exhaustive // Only container/image/network/volume have actor filters.
	case events.ContainerEventType:
		filter = f.Containers
	case events.ImageEventType:
		filter = f.Images
	case events.NetworkEventType:
		filter = f.Networks
	case events.VolumeEventType:
		filter = f.Volumes
	}

	if len(filter) == 0 {
		return true
	}

	return matchesActor(filter, msg.Actor)
}

func containsType(list []events.Type, t events.Type) bool {
	for _, v := range list {
		if v == t {
			return true
		}
	}

	return false
}

func containsAction(list []events.Action, a events.Action) bool {
	for _, v := range list {
		if v == a {
			return true
		}
	}

	return false
}

// matchesActor checks whether the actor ID or name attribute matches any entry.
func matchesActor(filter []string, actor events.Actor) bool {
	for _, f := range filter {
		if actor.ID == f || strings.HasPrefix(actor.ID, f) {
			return true
		}

		if name := actor.Attributes["name"]; name == f {
			return true
		}
	}

	return false
}

// matchesLabels checks that every requested label filter is present in attrs.
func matchesLabels(filter []string, attrs map[string]string) bool {
	for _, lf := range filter {
		k, v, hasVal := strings.Cut(lf, "=")

		got, ok := attrs[k]
		if !ok {
			return false
		}

		if hasVal && got != v {
			return false
		}
	}

	return true
}
