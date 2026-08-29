package clientcore

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

// randID returns a random 128-bit hex identifier for messages and file
// transfers.
func randID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// nowMillis is the current time as unix milliseconds (sender clock).
func nowMillis() int64 { return time.Now().UnixMilli() }

// sealInner marshals an Inner payload and seals it for the given peer.
func (c *Client) sealInner(peer Peer, inner *proto.Inner) (nonceB64, ciphertextB64 string, err error) {
	plaintext, err := json.Marshal(inner)
	if err != nil {
		return "", "", fmt.Errorf("clientcore: marshal inner: %w", err)
	}
	peerBox, err := crypto.DecodeKey32(peer.BoxPub)
	if err != nil {
		return "", "", fmt.Errorf("clientcore: peer %s has an invalid box key: %w", peer.DeviceID, err)
	}
	nonce, ct, err := crypto.Seal(plaintext, c.id.BoxPriv, peerBox)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(nonce[:]), base64.StdEncoding.EncodeToString(ct), nil
}

// openInner reverses sealInner: it decrypts a relayed message from peer.
func (c *Client) openInner(peer Peer, nonceB64, ciphertextB64 string) (*proto.Inner, error) {
	nonceBytes, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, fmt.Errorf("clientcore: bad nonce: %w", err)
	}
	nonce, err := crypto.NonceFromSlice(nonceBytes)
	if err != nil {
		return nil, err
	}
	ct, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return nil, fmt.Errorf("clientcore: bad ciphertext: %w", err)
	}
	peerBox, err := crypto.DecodeKey32(peer.BoxPub)
	if err != nil {
		return nil, fmt.Errorf("clientcore: peer %s has an invalid box key: %w", peer.DeviceID, err)
	}
	plaintext, err := crypto.Open(nonce, ct, peerBox, c.id.BoxPriv)
	if err != nil {
		return nil, err
	}
	var inner proto.Inner
	if err := json.Unmarshal(plaintext, &inner); err != nil {
		return nil, fmt.Errorf("clientcore: parse decrypted payload: %w", err)
	}
	return &inner, nil
}

// Fingerprint returns this device's own key fingerprint for out-of-band
// verification by peers.
func (c *Client) Fingerprint() string {
	return crypto.FingerprintPublic(c.id.Public())
}

// PeerFingerprint returns the fingerprint of a peer's keys as currently known,
// or "" if the peer is unknown.
func (c *Client) PeerFingerprint(peerID string) string {
	p, err := c.store.getPeer(peerID)
	if err != nil {
		return ""
	}
	signPub, err := crypto.DecodeSignPub(p.SignPub)
	if err != nil {
		return ""
	}
	boxPub, err := crypto.DecodeKey32(p.BoxPub)
	if err != nil {
		return ""
	}
	return crypto.Fingerprint(signPub, boxPub)
}
