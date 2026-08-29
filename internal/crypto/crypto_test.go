package crypto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func mustIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	return id
}

func TestIdentitySaveLoadRoundTrip(t *testing.T) {
	id := mustIdentity(t)
	path := filepath.Join(t.TempDir(), "nested", "identity.json")

	if err := id.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("identity file perms = %o, want 600", perm)
	}

	got, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !got.SignPub.Equal(id.SignPub) {
		t.Error("sign pub mismatch after reload")
	}
	if !bytes.Equal(got.SignPriv, id.SignPriv) {
		t.Error("sign priv mismatch after reload")
	}
	if *got.BoxPub != *id.BoxPub || *got.BoxPriv != *id.BoxPriv {
		t.Error("box keys mismatch after reload")
	}
	if got.DeviceID() != id.DeviceID() {
		t.Errorf("device id changed after reload: %s != %s", got.DeviceID(), id.DeviceID())
	}
}

func TestLoadOrCreateIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")

	id1, created, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	if !created {
		t.Error("expected created=true on first call")
	}

	id2, created, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if created {
		t.Error("expected created=false on second call")
	}
	if id1.DeviceID() != id2.DeviceID() {
		t.Error("LoadOrCreate returned a different identity on reload")
	}
}

func TestDeviceIDStableAndDistinct(t *testing.T) {
	a := mustIdentity(t)
	b := mustIdentity(t)
	if a.DeviceID() == b.DeviceID() {
		t.Fatal("two random identities produced the same device id")
	}
	if len(a.DeviceID()) != 32 { // 16 bytes hex
		t.Errorf("device id length = %d, want 32", len(a.DeviceID()))
	}
	if a.DeviceID() != a.Public().DeviceID() {
		t.Error("DeviceID and Public().DeviceID() disagree")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	alice := mustIdentity(t)
	bob := mustIdentity(t)
	plaintext := []byte(`{"kind":"text","data":{"text":"come downstairs"}}`)

	nonce, ct, err := Seal(plaintext, alice.BoxPriv, bob.BoxPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct, []byte("come downstairs")) {
		t.Fatal("plaintext leaked into ciphertext")
	}

	got, err := Open(nonce, ct, alice.BoxPub, bob.BoxPriv)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	alice := mustIdentity(t)
	bob := mustIdentity(t)
	nonce, ct, err := Seal([]byte("secret"), alice.BoxPriv, bob.BoxPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ct[len(ct)/2] ^= 0x01
	if _, err := Open(nonce, ct, alice.BoxPub, bob.BoxPriv); err == nil {
		t.Fatal("Open accepted a tampered ciphertext")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	alice := mustIdentity(t)
	bob := mustIdentity(t)
	eve := mustIdentity(t)

	nonce, ct, err := Seal([]byte("secret"), alice.BoxPriv, bob.BoxPub)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := Open(nonce, ct, alice.BoxPub, eve.BoxPriv); err == nil {
		t.Fatal("Open accepted decryption with the wrong recipient key")
	}
	if _, err := Open(nonce, ct, eve.BoxPub, bob.BoxPriv); err == nil {
		t.Fatal("Open accepted a forged sender key")
	}
}

func TestNonceFromSlice(t *testing.T) {
	if _, err := NonceFromSlice(make([]byte, 10)); err == nil {
		t.Error("expected error for short nonce")
	}
	n, err := NonceFromSlice(bytes.Repeat([]byte{7}, NonceSize))
	if err != nil {
		t.Fatalf("NonceFromSlice: %v", err)
	}
	if n[0] != 7 || n[NonceSize-1] != 7 {
		t.Error("nonce not copied correctly")
	}
}

func TestFingerprintFormatAndSensitivity(t *testing.T) {
	id := mustIdentity(t)
	fp := FingerprintPublic(id.Public())

	// Format: 4 groups of 4 hex chars joined by '-'  => len 19.
	if len(fp) != 19 || fp[4] != '-' || fp[9] != '-' || fp[14] != '-' {
		t.Fatalf("unexpected fingerprint format: %q", fp)
	}

	// Same keys -> same fingerprint.
	if got := Fingerprint(id.SignPub, id.BoxPub); got != fp {
		t.Errorf("fingerprint not stable: %q vs %q", got, fp)
	}

	// Swapping the box key changes the fingerprint.
	otherBoxPub, _, _ := box.GenerateKey(rand.Reader)
	if Fingerprint(id.SignPub, otherBoxPub) == fp {
		t.Error("fingerprint unchanged after box key swap")
	}

	// Swapping the signing key changes the fingerprint.
	otherSignPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if Fingerprint(otherSignPub, id.BoxPub) == fp {
		t.Error("fingerprint unchanged after signing key swap")
	}
}

func TestPassphraseChallengeResponse(t *testing.T) {
	v, err := NewPassphraseVerifier("correct horse battery staple")
	if err != nil {
		t.Fatalf("NewPassphraseVerifier: %v", err)
	}

	salt, err := v.SaltBytes()
	if err != nil {
		t.Fatalf("SaltBytes: %v", err)
	}
	nonce, err := NewChallengeNonce()
	if err != nil {
		t.Fatalf("NewChallengeNonce: %v", err)
	}

	// Client side: derive key from the same passphrase + salt, compute proof.
	clientKey := DeriveKey("correct horse battery staple", salt)
	proof := Proof(clientKey, nonce)

	ok, err := v.CheckResponse(
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(proof),
	)
	if err != nil {
		t.Fatalf("CheckResponse: %v", err)
	}
	if !ok {
		t.Fatal("valid passphrase proof was rejected")
	}

	// Wrong passphrase must fail.
	badProof := Proof(DeriveKey("hunter2", salt), nonce)
	ok, err = v.CheckResponse(
		base64.StdEncoding.EncodeToString(nonce),
		base64.StdEncoding.EncodeToString(badProof),
	)
	if err != nil {
		t.Fatalf("CheckResponse (bad): %v", err)
	}
	if ok {
		t.Fatal("wrong passphrase proof was accepted")
	}
}

func TestSignVerify(t *testing.T) {
	id := mustIdentity(t)
	msg := []byte("approve:device-1234")

	sig := Sign(id.SignPriv, msg)
	if !Verify(id.SignPub, msg, sig) {
		t.Fatal("valid signature rejected")
	}
	if Verify(id.SignPub, []byte("deny:device-1234"), sig) {
		t.Fatal("signature verified against a different message")
	}

	other := mustIdentity(t)
	if Verify(other.SignPub, msg, sig) {
		t.Fatal("signature verified against the wrong public key")
	}

	okB64, err := VerifyFromB64Pub(EncodeSignPub(id.SignPub), msg, sig)
	if err != nil {
		t.Fatalf("VerifyFromB64Pub: %v", err)
	}
	if !okB64 {
		t.Fatal("VerifyFromB64Pub rejected a valid signature")
	}
}
