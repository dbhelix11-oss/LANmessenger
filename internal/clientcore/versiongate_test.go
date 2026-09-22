package clientcore

import (
	"testing"
	"time"

	"lanmessenger/internal/proto"
	"lanmessenger/internal/version"
)

// newBareClient builds a Client with just enough set up to exercise
// applyProtocolGate and its event emission — no store, no network.
func newBareClient() *Client {
	return &Client{events: make(chan Event, 4)}
}

func TestApplyProtocolGateEmitsUpdateAvailable(t *testing.T) {
	c := newBareClient()
	c.applyProtocolGate(proto.Ready{ServerVersion: "99.0.0"})

	select {
	case ev := <-c.events:
		if ev.Kind != EventUpdateAvailable {
			t.Fatalf("got event %s, want %s", ev.Kind, EventUpdateAvailable)
		}
		if ev.ServerVersion != "99.0.0" {
			t.Fatalf("ServerVersion = %q, want 99.0.0", ev.ServerVersion)
		}
	case <-time.After(time.Second):
		t.Fatal("no event emitted")
	}
	if c.ServerVersion() != "99.0.0" {
		t.Fatalf("ServerVersion() = %q, want 99.0.0", c.ServerVersion())
	}
}

func TestApplyProtocolGateSilentWhenNotNewer(t *testing.T) {
	c := newBareClient()
	c.applyProtocolGate(proto.Ready{ServerVersion: version.Version})

	select {
	case ev := <-c.events:
		t.Fatalf("unexpected event emitted: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// expected: no event
	}
}

func TestApplyProtocolGateSilentWhenEmpty(t *testing.T) {
	c := newBareClient()
	c.applyProtocolGate(proto.Ready{})

	select {
	case ev := <-c.events:
		t.Fatalf("unexpected event emitted: %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// expected: no event
	}
}
