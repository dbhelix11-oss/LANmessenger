package proto

import (
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	in := Msg{
		To:         "device-abc",
		MsgID:      "01HZZ",
		Nonce:      "bm9uY2U=",
		Ciphertext: "Y2lwaGVy",
		TS:         1724900000000,
	}
	env, err := NewEnvelope(TypeMsg, "corr-1", 1724900000001, in)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	wire, err := Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got, err := Parse(wire)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Type != TypeMsg || got.ID != "corr-1" || got.V != Version {
		t.Fatalf("envelope header mismatch: %+v", got)
	}

	var out Msg
	if err := got.Unmarshal(&out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("payload round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestParseRejectsWrongVersion(t *testing.T) {
	// V:99 with an otherwise valid frame.
	bad := []byte(`{"v":99,"type":"ping"}`)
	if _, err := Parse(bad); err == nil {
		t.Fatal("expected error for unsupported version, got nil")
	}
}

func TestParseRejectsMissingType(t *testing.T) {
	bad := []byte(`{"v":1}`)
	if _, err := Parse(bad); err == nil {
		t.Fatal("expected error for missing type, got nil")
	}
}

func TestUnmarshalEmptyDataErrors(t *testing.T) {
	env, err := NewEnvelope(TypePing, "", 0, nil)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	var m Msg
	if err := env.Unmarshal(&m); err == nil {
		t.Fatal("expected error unmarshalling empty data, got nil")
	}
}

func TestInnerRoundTrip(t *testing.T) {
	body := TextBody{Text: "dinner's ready"}
	inner, err := NewInner(InnerText, body)
	if err != nil {
		t.Fatalf("NewInner: %v", err)
	}

	// Inner is itself JSON-marshalled before sealing; simulate that hop.
	raw, err := NewEnvelope(TypeMsg, "", 0, inner)
	if err != nil {
		t.Fatalf("wrap inner: %v", err)
	}
	var decodedInner Inner
	if err := raw.Unmarshal(&decodedInner); err != nil {
		t.Fatalf("decode inner: %v", err)
	}
	if decodedInner.Kind != InnerText {
		t.Fatalf("kind mismatch: %q", decodedInner.Kind)
	}
	var out TextBody
	if err := decodedInner.Unmarshal(&out); err != nil {
		t.Fatalf("decode text body: %v", err)
	}
	if out != body {
		t.Fatalf("inner body mismatch: got %+v want %+v", out, body)
	}
}

func TestStatusValid(t *testing.T) {
	for _, s := range []Status{StatusAvailable, StatusAway, StatusBusy, StatusDND, StatusInvisible, StatusOffline} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if Status("nonsense").Valid() {
		t.Error("bogus status reported valid")
	}
}

func TestAdminActionMessageStable(t *testing.T) {
	got := string(AdminActionMessage("approve", "device-xyz"))
	if got != "approve:device-xyz" {
		t.Fatalf("unexpected admin action message: %q", got)
	}
}
