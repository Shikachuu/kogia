package events

import (
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
)

func TestBus_PublishSubscribe(t *testing.T) {
	t.Parallel()

	b := New()
	sub := b.Subscribe(Filters{})

	defer sub.Close()

	b.Publish(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "abc123"},
	})

	select {
	case msg := <-sub.C:
		if msg.Actor.ID != "abc123" {
			t.Errorf("actor id = %q, want %q", msg.Actor.ID, "abc123")
		}

		if msg.Scope != "local" {
			t.Errorf("scope = %q, want %q", msg.Scope, "local")
		}

		if msg.Time == 0 {
			t.Error("time should be auto-set")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestBus_FilteredSubscribe(t *testing.T) {
	t.Parallel()

	b := New()
	sub := b.Subscribe(Filters{Types: []events.Type{events.ContainerEventType}})

	defer sub.Close()

	// Publish image event — should be filtered out.
	b.Publish(events.Message{
		Type:   events.ImageEventType,
		Action: events.ActionPull,
	})

	// Publish container event — should pass.
	b.Publish(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor:  events.Actor{ID: "ctr1"},
	})

	select {
	case msg := <-sub.C:
		if msg.Type != events.ContainerEventType {
			t.Errorf("type = %q, want container", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for filtered message")
	}
}

func TestBus_MultipleSubscribers(t *testing.T) {
	t.Parallel()

	b := New()
	sub1 := b.Subscribe(Filters{})
	sub2 := b.Subscribe(Filters{})

	defer sub1.Close()
	defer sub2.Close()

	b.Publish(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionCreate,
		Actor:  events.Actor{ID: "x"},
	})

	for i, sub := range []*Subscription{sub1, sub2} {
		select {
		case msg := <-sub.C:
			if msg.Actor.ID != "x" {
				t.Errorf("sub%d: actor id = %q, want %q", i, msg.Actor.ID, "x")
			}
		case <-time.After(time.Second):
			t.Fatalf("sub%d: timed out", i)
		}
	}
}

func TestBus_SlowConsumer(t *testing.T) {
	t.Parallel()

	b := New()
	sub := b.Subscribe(Filters{})

	defer sub.Close()

	// Fill the buffer.
	for i := range subscriberBufSize {
		b.Publish(events.Message{
			Type:   events.ContainerEventType,
			Action: events.ActionStart,
			Actor:  events.Actor{ID: "fill", Attributes: map[string]string{"i": string(rune('0' + i%10))}},
		})
	}

	// One more should not block.
	done := make(chan struct{})

	go func() {
		b.Publish(events.Message{
			Type:   events.ContainerEventType,
			Action: events.ActionStart,
			Actor:  events.Actor{ID: "overflow"},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on full subscriber buffer")
	}
}

func TestBus_Close(t *testing.T) {
	t.Parallel()

	b := New()
	sub := b.Subscribe(Filters{})
	sub.Close()

	// Channel should be closed.
	_, ok := <-sub.C
	if ok {
		t.Error("expected channel to be closed")
	}

	// Publishing after close should not panic.
	b.Publish(events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
	})
}

func TestBus_ConcurrentPublish(t *testing.T) {
	t.Parallel()

	b := New()
	sub := b.Subscribe(Filters{})

	defer sub.Close()

	var wg sync.WaitGroup

	for range 10 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 100 {
				b.Publish(events.Message{
					Type:   events.ContainerEventType,
					Action: events.ActionStart,
				})
			}
		}()
	}

	wg.Wait()
}

func TestParseFilters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantTypes  int
		wantEvents int
	}{
		{
			name:       "empty",
			query:      "",
			wantTypes:  0,
			wantEvents: 0,
		},
		{
			name:       "type filter",
			query:      `{"type":["container"]}`,
			wantTypes:  1,
			wantEvents: 0,
		},
		{
			name:       "type and event",
			query:      `{"type":["container","image"],"event":["start","pull"]}`,
			wantTypes:  2,
			wantEvents: 2,
		},
		{
			name:       "invalid json",
			query:      `not json`,
			wantTypes:  0,
			wantEvents: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vals := url.Values{}
			if tt.query != "" {
				vals.Set("filters", tt.query)
			}

			f := ParseFilters(vals)

			if len(f.Types) != tt.wantTypes {
				t.Errorf("types count = %d, want %d", len(f.Types), tt.wantTypes)
			}

			if len(f.Actions) != tt.wantEvents {
				t.Errorf("actions count = %d, want %d", len(f.Actions), tt.wantEvents)
			}
		})
	}
}

func TestMatchesFilters(t *testing.T) {
	t.Parallel()

	msg := events.Message{
		Type:   events.ContainerEventType,
		Action: events.ActionStart,
		Actor: events.Actor{
			ID:         "abc123def456",
			Attributes: map[string]string{"name": "mycontainer", "env": "prod"},
		},
	}

	tests := []struct {
		name    string
		filters Filters
		want    bool
	}{
		{
			name:    "no filters matches all",
			filters: Filters{},
			want:    true,
		},
		{
			name:    "matching type",
			filters: Filters{Types: []events.Type{events.ContainerEventType}},
			want:    true,
		},
		{
			name:    "non-matching type",
			filters: Filters{Types: []events.Type{events.ImageEventType}},
			want:    false,
		},
		{
			name:    "matching action",
			filters: Filters{Actions: []events.Action{events.ActionStart}},
			want:    true,
		},
		{
			name:    "non-matching action",
			filters: Filters{Actions: []events.Action{events.ActionStop}},
			want:    false,
		},
		{
			name:    "matching container by prefix",
			filters: Filters{Containers: []string{"abc123"}},
			want:    true,
		},
		{
			name:    "matching container by name",
			filters: Filters{Containers: []string{"mycontainer"}},
			want:    true,
		},
		{
			name:    "matching label key=value",
			filters: Filters{Labels: []string{"env=prod"}},
			want:    true,
		},
		{
			name:    "non-matching label value",
			filters: Filters{Labels: []string{"env=staging"}},
			want:    false,
		},
		{
			name:    "label key only",
			filters: Filters{Labels: []string{"env"}},
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := matchesFilters(msg, tt.filters)
			if got != tt.want {
				t.Errorf("matchesFilters() = %v, want %v", got, tt.want)
			}
		})
	}
}
