package servercore

import (
	"path/filepath"
	"testing"
	"time"

	"lanmessenger/internal/proto"
)

func testStore(t *testing.T) *serverStore {
	t.Helper()
	st, err := openServerStore(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("openServerStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sampleEntry(id, name string, state proto.EnrollState) proto.DirectoryEntry {
	return proto.DirectoryEntry{
		DeviceID:    id,
		DisplayName: name,
		SignPub:     "c2lnbnB1Yg==",
		BoxPub:      "Ym94cHVi",
		State:       state,
	}
}

func TestDeviceUpsertGetList(t *testing.T) {
	st := testStore(t)

	if err := st.upsertDevice(sampleEntry("dev1", "Dad", proto.StateActive)); err != nil {
		t.Fatalf("upsert dev1: %v", err)
	}
	if err := st.upsertDevice(sampleEntry("dev2", "Mom", proto.StatePending)); err != nil {
		t.Fatalf("upsert dev2: %v", err)
	}

	got, err := st.getDevice("dev1")
	if err != nil {
		t.Fatalf("getDevice: %v", err)
	}
	if got.DisplayName != "Dad" || got.State != proto.StateActive {
		t.Fatalf("unexpected dev1: %+v", got)
	}

	if _, err := st.getDevice("nope"); err != errNoDevice {
		t.Fatalf("expected errNoDevice, got %v", err)
	}

	active, err := st.listDevicesByState(proto.StateActive)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 1 || active[0].DeviceID != "dev1" {
		t.Fatalf("active list wrong: %+v", active)
	}

	pending, err := st.listDevicesByState(proto.StatePending)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].DeviceID != "dev2" {
		t.Fatalf("pending list wrong: %+v", pending)
	}

	// Upsert must not clobber state.
	if err := st.upsertDevice(sampleEntry("dev2", "Mom's laptop", proto.StateActive)); err != nil {
		t.Fatalf("re-upsert dev2: %v", err)
	}
	got2, _ := st.getDevice("dev2")
	if got2.State != proto.StatePending {
		t.Fatalf("upsert clobbered state: %+v", got2)
	}
	if got2.DisplayName != "Mom's laptop" {
		t.Fatalf("upsert did not update display name: %+v", got2)
	}
}

func TestSetDeviceState(t *testing.T) {
	st := testStore(t)
	_ = st.upsertDevice(sampleEntry("dev1", "Kid", proto.StatePending))

	if err := st.setDeviceState("dev1", proto.StateActive); err != nil {
		t.Fatalf("setDeviceState: %v", err)
	}
	got, _ := st.getDevice("dev1")
	if got.State != proto.StateActive {
		t.Fatalf("state not updated: %+v", got)
	}
	if err := st.setDeviceState("ghost", proto.StateActive); err != errNoDevice {
		t.Fatalf("expected errNoDevice for missing device, got %v", err)
	}
}

func TestPresenceDefaultAndRoundTrip(t *testing.T) {
	st := testStore(t)
	_ = st.upsertDevice(sampleEntry("dev1", "Dad", proto.StateActive))

	got, err := st.getPresence("dev1")
	if err != nil {
		t.Fatalf("getPresence default: %v", err)
	}
	if got.Status != proto.StatusAvailable {
		t.Fatalf("default presence should be available, got %q", got.Status)
	}

	if err := st.setPresence("dev1", proto.StatusDND, "on a call"); err != nil {
		t.Fatalf("setPresence: %v", err)
	}
	got, _ = st.getPresence("dev1")
	if got.Status != proto.StatusDND || got.Message != "on a call" {
		t.Fatalf("presence round-trip wrong: %+v", got)
	}
}

func TestQueueEnqueueDrainAck(t *testing.T) {
	st := testStore(t)
	_ = st.upsertDevice(sampleEntry("rcpt", "Sleepy", proto.StateActive))

	for i, id := range []string{"m1", "m2", "m3"} {
		err := st.enqueue("rcpt", queuedMsg{
			Sender: "sender", MsgID: id, Nonce: "n", Ciphertext: "c", TS: int64(i),
		}, 0)
		if err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}

	// Duplicate is ignored.
	if err := st.enqueue("rcpt", queuedMsg{Sender: "sender", MsgID: "m2", Nonce: "n", Ciphertext: "c"}, 0); err != nil {
		t.Fatalf("enqueue dup: %v", err)
	}

	msgs, err := st.drain("rcpt")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(msgs) != 3 || msgs[0].MsgID != "m1" || msgs[2].MsgID != "m3" {
		t.Fatalf("drain order/count wrong: %+v", msgs)
	}

	if err := st.ackQueued("rcpt", "sender", "m2"); err != nil {
		t.Fatalf("ackQueued: %v", err)
	}
	msgs, _ = st.drain("rcpt")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 after ack, got %d", len(msgs))
	}
}

func TestQueueCapDropsOldest(t *testing.T) {
	st := testStore(t)
	_ = st.upsertDevice(sampleEntry("rcpt", "Sleepy", proto.StateActive))

	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if err := st.enqueue("rcpt", queuedMsg{Sender: "s", MsgID: id, Nonce: "n", Ciphertext: "c"}, 3); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	msgs, _ := st.drain("rcpt")
	if len(msgs) != 3 {
		t.Fatalf("cap not enforced: %d rows", len(msgs))
	}
	if msgs[0].MsgID != "m3" || msgs[2].MsgID != "m5" {
		t.Fatalf("cap kept the wrong rows: %+v", msgs)
	}
}

func TestQueuePurgeExpired(t *testing.T) {
	st := testStore(t)
	_ = st.upsertDevice(sampleEntry("rcpt", "Sleepy", proto.StateActive))
	_ = st.enqueue("rcpt", queuedMsg{Sender: "s", MsgID: "old", Nonce: "n", Ciphertext: "c"}, 0)

	// Backdate the row two hours.
	if _, err := st.db.Exec(`UPDATE queue SET enqueued_at = enqueued_at - 7200 WHERE msg_id = 'old'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := st.purgeExpired(time.Hour)
	if err != nil {
		t.Fatalf("purgeExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 purged, got %d", n)
	}
	if n, _ := st.purgeExpired(0); n != 0 {
		t.Fatalf("retention 0 should purge nothing, purged %d", n)
	}
}
