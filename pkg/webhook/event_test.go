package webhook

import (
	"errors"
	"testing"
)

func TestEventsListsEveryEventOnce(t *testing.T) {
	t.Parallel()

	events := Events()
	if got := len(events); got != 6 {
		t.Fatalf("len(events) = %v, want %v", got, 6)
	}
	seen := map[Event]bool{}
	for _, ev := range events {
		if seen[ev] {
			t.Fatal("not true: !seen[ev]")
		}
		seen[ev] = true
		if ev.String() == "" {
			t.Fatal("not true: ev.String() != \"\"")
		}
	}
}

func TestEventStringRoundTrip(t *testing.T) {
	t.Parallel()

	for _, ev := range Events() {
		parsed, err := ParseEvent(ev.String())
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed; got != ev {
			t.Fatalf("parsed = %v, want %v", got, ev)
		}
	}
}

func TestParseEventRejectsUnknown(t *testing.T) {
	t.Parallel()

	ev, err := ParseEvent("not_an_event")
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("not true: errors.Is(err, ErrInvalidEvent)")
	}
	if got, want := ev, Event(-1); got != want {
		t.Fatalf("ev = %v, want %v", got, want)
	}
}

func TestEventUnmarshalText(t *testing.T) {
	t.Parallel()

	var ev Event
	if err := ev.UnmarshalText([]byte("push")); err != nil {
		t.Fatal(err)
	}
	if got := ev; got != EventPush {
		t.Fatalf("ev = %v, want %v", got, EventPush)
	}

	if !errors.Is(ev.UnmarshalText([]byte("bogus")), ErrInvalidEvent) {
		t.Fatal("not true: errors.Is(ev.UnmarshalText([]byte(\"bogus\")), ErrInvalidEvent)")
	}
	// A failed unmarshal leaves the value unchanged.
	if got := ev; got != EventPush {
		t.Fatalf("ev = %v, want %v", got, EventPush)
	}
}

func TestEventMarshalText(t *testing.T) {
	t.Parallel()

	text, err := EventRepositoryVisibilityChange.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(text); got != "repository_visibility_change" {
		t.Fatalf("string(text) = %v, want %v", got, "repository_visibility_change")
	}

	_, err = Event(0).MarshalText()
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("not true: errors.Is(err, ErrInvalidEvent)")
	}
	_, err = Event(99).MarshalText()
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("not true: errors.Is(err, ErrInvalidEvent)")
	}
}
