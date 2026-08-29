package clientcore

import (
	"context"
	"fmt"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

// RosterEntry is a peer merged with its last-known presence.
type RosterEntry struct {
	Peer
	Online        bool
	Status        proto.Status
	StatusMessage string
	Fingerprint   string
}

// Roster returns all known peers with presence folded in, sorted by display
// name.
func (c *Client) Roster() ([]RosterEntry, error) {
	peers, err := c.store.listPeers()
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	presence := make(map[string]proto.PresenceUpdate, len(c.peerPresence))
	for k, v := range c.peerPresence {
		presence[k] = v
	}
	c.mu.RUnlock()

	out := make([]RosterEntry, 0, len(peers))
	for _, p := range peers {
		e := RosterEntry{Peer: p, Status: proto.StatusOffline}
		if pu, ok := presence[p.DeviceID]; ok {
			e.Online = pu.Online
			e.Status = pu.Status
			e.StatusMessage = pu.Message
		}
		if signPub, err := crypto.DecodeSignPub(p.SignPub); err == nil {
			if boxPub, err := crypto.DecodeKey32(p.BoxPub); err == nil {
				e.Fingerprint = crypto.Fingerprint(signPub, boxPub)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// History returns up to limit recent messages exchanged with peerID, oldest
// first.
func (c *Client) History(peerID string, limit int) ([]Message, error) {
	return c.store.listMessages(peerID, limit)
}

// SendText encrypts and sends a text message to peerID. If the client is not
// connected the message is stored with state "queued" and returned; the outbox
// flush on reconnect (milestone 4) will deliver it.
func (c *Client) SendText(ctx context.Context, peerID, text string) (Message, error) {
	peer, err := c.store.getPeer(peerID)
	if err != nil {
		return Message{}, ErrUnknownPeer
	}

	inner, err := proto.NewInner(proto.InnerText, proto.TextBody{Text: text})
	if err != nil {
		return Message{}, err
	}
	nonceB64, ctB64, err := c.sealInner(peer, inner)
	if err != nil {
		return Message{}, err
	}

	msg := Message{
		MsgID:     randID(),
		PeerID:    peerID,
		Direction: DirOut,
		Kind:      proto.InnerText,
		Body:      text,
		TS:        time.Now().UnixMilli(),
		State:     StateQueued,
	}

	sent := false
	if w := c.currentConn(); w != nil && c.State() == StateReady {
		frame := proto.Msg{
			To:         peerID,
			MsgID:      msg.MsgID,
			Nonce:      nonceB64,
			Ciphertext: ctB64,
			TS:         msg.TS,
		}
		if err := w.send(ctx, proto.TypeMsg, "", frame); err == nil {
			msg.State = StateSent
			sent = true
		}
	}

	if !sent {
		// Offline (or the send failed): stash the sealed message in the outbox
		// so the next successful connection delivers it.
		if err := c.store.enqueueOutbox(outboxRow{
			MsgID: msg.MsgID, PeerID: peerID, Nonce: nonceB64, Ciphertext: ctB64, TS: msg.TS,
		}); err != nil {
			return Message{}, err
		}
	}

	if _, err := c.store.insertMessage(msg); err != nil {
		return Message{}, err
	}
	mc := msg
	c.emit(Event{Kind: EventMessage, PeerID: peerID, Message: &mc})
	return msg, nil
}

// flushOutbox resends every message that was composed while offline, in order,
// stopping at the first send failure (the next reconnect retries the rest).
func (c *Client) flushOutbox(ctx context.Context, w *wsConn) {
	rows, err := c.store.listOutbox()
	if err != nil {
		c.emitError(err)
		return
	}
	for _, r := range rows {
		frame := proto.Msg{To: r.PeerID, MsgID: r.MsgID, Nonce: r.Nonce, Ciphertext: r.Ciphertext, TS: r.TS}
		if err := w.send(ctx, proto.TypeMsg, "", frame); err != nil {
			return
		}
		_ = c.store.deleteOutbox(r.MsgID)
		_ = c.store.setMessageState(r.MsgID, r.PeerID, DirOut, StateSent)
		c.emit(Event{Kind: EventMessageState, PeerID: r.PeerID, MsgID: r.MsgID, MsgState: StateSent})
	}
}

// SetStatus updates this device's presence. It is remembered and re-published on
// every reconnect.
func (c *Client) SetStatus(ctx context.Context, status proto.Status, message string) error {
	if !status.Valid() {
		return fmt.Errorf("clientcore: invalid status %q", status)
	}
	c.mu.Lock()
	c.desired = proto.PresenceSet{Status: status, Message: message}
	c.mu.Unlock()

	if w := c.currentConn(); w != nil && c.State() == StateReady {
		return w.send(ctx, proto.TypePresenceSet, "", proto.PresenceSet{Status: status, Message: message})
	}
	return nil
}

// DesiredStatus returns the presence this device publishes.
func (c *Client) DesiredStatus() proto.PresenceSet {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.desired
}

// MarkVerified records that the user has confirmed peerID's key fingerprint out
// of band, clearing any key-changed warning.
func (c *Client) MarkVerified(peerID string) error {
	if err := c.store.setPeerVerified(peerID, true); err != nil {
		return err
	}
	c.emit(Event{Kind: EventRosterChanged})
	return nil
}

// --- admin ---------------------------------------------------------

const adminReplyTimeout = 10 * time.Second

// ListPending returns the devices awaiting admin approval. Requires this device
// to be an admin and connected.
func (c *Client) ListPending(ctx context.Context) ([]proto.DirectoryEntry, error) {
	w := c.currentConn()
	if w == nil || c.State() != StateReady {
		return nil, ErrNotReady
	}
	if !c.IsAdmin() {
		return nil, fmt.Errorf("clientcore: this device is not an admin")
	}

	id := randID()
	ch := c.newPending(id)
	defer c.cancelPending(id)

	if err := w.send(ctx, proto.TypeAdminListPending, id, proto.AdminListPending{}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(adminReplyTimeout):
		return nil, fmt.Errorf("clientcore: timed out waiting for pending list")
	case env := <-ch:
		if env.Type == proto.TypeError {
			var e proto.ErrorBody
			_ = env.Unmarshal(&e)
			return nil, &RelayError{Code: e.Code, Message: e.Message}
		}
		var pl proto.AdminPendingList
		if err := env.Unmarshal(&pl); err != nil {
			return nil, err
		}
		return pl.Entries, nil
	}
}

// Approve activates a pending device. It signs "approve:<deviceID>" with this
// device's signing key.
func (c *Client) Approve(ctx context.Context, deviceID string) error {
	return c.adminAction(ctx, "approve", deviceID)
}

// Deny rejects a pending device.
func (c *Client) Deny(ctx context.Context, deviceID string) error {
	return c.adminAction(ctx, "deny", deviceID)
}

func (c *Client) adminAction(ctx context.Context, action, deviceID string) error {
	w := c.currentConn()
	if w == nil || c.State() != StateReady {
		return ErrNotReady
	}
	if !c.IsAdmin() {
		return fmt.Errorf("clientcore: this device is not an admin")
	}
	sig := crypto.Sign(c.id.SignPriv, proto.AdminActionMessage(action, deviceID))
	switch action {
	case "approve":
		return w.send(ctx, proto.TypeAdminApprove, "", proto.AdminApprove{DeviceID: deviceID, Signature: sig})
	case "deny":
		return w.send(ctx, proto.TypeAdminDeny, "", proto.AdminDeny{DeviceID: deviceID, Signature: sig})
	default:
		return fmt.Errorf("clientcore: unknown admin action %q", action)
	}
}
