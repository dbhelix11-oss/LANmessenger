package clientcore

import (
	"context"
	"fmt"

	"lanmessenger/internal/proto"
)

// dispatch routes one frame from the relay.
func (c *Client) dispatch(ctx context.Context, w *wsConn, env *proto.Envelope) {
	switch env.Type {
	case proto.TypeMsg:
		c.handleIncomingMsg(ctx, w, env)
	case proto.TypeMsgAck:
		c.handleAck(env)
	case proto.TypeDirectorySnapshot:
		c.handleSnapshot(env)
	case proto.TypeDirectoryUpdate:
		c.handleDirUpdate(env)
	case proto.TypePresenceUpdate:
		c.handlePresence(env)
	case proto.TypeReady:
		c.handleReady(ctx, w, env)
	case proto.TypeAdminPendingList:
		c.routeReply(env)
		c.emit(Event{Kind: EventPendingChanged})
	case proto.TypeError:
		var e proto.ErrorBody
		_ = env.Unmarshal(&e)
		c.emitError(&RelayError{Code: e.Code, Message: e.Message})
	case proto.TypePong:
		// ignore
	default:
		c.log.Debug("ignoring unexpected frame", "type", env.Type)
	}
}

func (c *Client) handleReady(ctx context.Context, w *wsConn, env *proto.Envelope) {
	var rd proto.Ready
	_ = env.Unmarshal(&rd)
	c.applyProtocolGate(rd)
	c.mu.Lock()
	c.admin = rd.Admin
	desired := c.desired
	c.mu.Unlock()
	c.emitConnState(StateReady)
	if err := w.send(ctx, proto.TypePresenceSet, "", desired); err != nil {
		c.emitError(err)
	}
	c.flushOutbox(ctx, w)
}

func (c *Client) handleIncomingMsg(ctx context.Context, w *wsConn, env *proto.Envelope) {
	var m proto.Msg
	if err := env.Unmarshal(&m); err != nil {
		c.emitError(err)
		return
	}
	if m.From == "" {
		return
	}

	peer, err := c.store.getPeer(m.From)
	if err != nil {
		// We don't know this sender's keys yet. Ack anyway so the relay stops
		// redelivering; the message is lost but the alternative is a redelivery
		// loop on every reconnect.
		c.emitError(fmt.Errorf("message from unknown device %s discarded", m.From))
		c.ack(ctx, w, m.MsgID, m.From)
		return
	}

	inner, err := c.openInner(peer, m.Nonce, m.Ciphertext)
	if err != nil {
		c.emitError(fmt.Errorf("could not decrypt message from %s: %w", peer.DisplayName, err))
		c.ack(ctx, w, m.MsgID, m.From)
		return
	}

	switch inner.Kind {
	case proto.InnerText:
		var body proto.TextBody
		if err := inner.Unmarshal(&body); err != nil {
			c.emitError(err)
			c.ack(ctx, w, m.MsgID, m.From)
			return
		}
		msg := Message{
			MsgID:     m.MsgID,
			PeerID:    m.From,
			Direction: DirIn,
			Kind:      proto.InnerText,
			Body:      body.Text,
			TS:        m.TS,
			State:     StateReceived,
		}
		inserted, err := c.store.insertMessage(msg)
		if err != nil {
			c.emitError(err)
			return
		}
		c.ack(ctx, w, m.MsgID, m.From)
		if inserted {
			mc := msg
			c.emit(Event{Kind: EventMessage, PeerID: m.From, Message: &mc})
		}

	case proto.InnerReceipt:
		var r proto.ReceiptBody
		if err := inner.Unmarshal(&r); err == nil {
			_ = c.store.setMessageState(r.MsgID, m.From, DirOut, StateDelivered)
			c.emit(Event{Kind: EventMessageState, PeerID: m.From, MsgID: r.MsgID, MsgState: StateDelivered})
		}
		c.ack(ctx, w, m.MsgID, m.From)

	case proto.InnerFileOffer:
		c.handleFileOffer(ctx, w, peer, m, inner)

	case proto.InnerFileChunk:
		c.handleFileChunk(ctx, w, m, inner)

	default:
		c.ack(ctx, w, m.MsgID, m.From)
	}
}

func (c *Client) ack(ctx context.Context, w *wsConn, msgID, peerID string) {
	if err := w.send(ctx, proto.TypeMsgAck, "", proto.MsgAck{MsgID: msgID, Peer: peerID}); err != nil {
		c.log.Debug("ack failed", "err", err)
	}
}

func (c *Client) handleAck(env *proto.Envelope) {
	var ack proto.MsgAck
	if err := env.Unmarshal(&ack); err != nil {
		return
	}
	if ack.MsgID == "" || ack.Peer == "" {
		return
	}
	_ = c.store.deleteOutbox(ack.MsgID) // in case an ack races the outbox flush
	_ = c.store.setMessageState(ack.MsgID, ack.Peer, DirOut, StateDelivered)
	c.emit(Event{Kind: EventMessageState, PeerID: ack.Peer, MsgID: ack.MsgID, MsgState: StateDelivered})
}

func (c *Client) handleSnapshot(env *proto.Envelope) {
	var snap proto.DirectorySnapshot
	if err := env.Unmarshal(&snap); err != nil {
		c.emitError(err)
		return
	}
	keep := map[string]bool{}
	for _, e := range snap.Entries {
		keep[e.DeviceID] = true
		if changed, err := c.store.upsertPeer(e); err != nil {
			c.emitError(err)
		} else if changed {
			c.emit(Event{Kind: EventKeyChanged, PeerID: e.DeviceID})
		}
	}
	// Drop local peers the relay no longer lists (denied/removed elsewhere).
	if existing, err := c.store.listPeers(); err == nil {
		for _, p := range existing {
			if !keep[p.DeviceID] {
				_ = c.store.removePeer(p.DeviceID)
			}
		}
	}
	c.emit(Event{Kind: EventRosterChanged})
}

func (c *Client) handleDirUpdate(env *proto.Envelope) {
	var du proto.DirectoryUpdate
	if err := env.Unmarshal(&du); err != nil {
		c.emitError(err)
		return
	}
	if du.Removed {
		_ = c.store.removePeer(du.Entry.DeviceID)
		c.emit(Event{Kind: EventRosterChanged})
		return
	}
	changed, err := c.store.upsertPeer(du.Entry)
	if err != nil {
		c.emitError(err)
		return
	}
	if changed {
		c.emit(Event{Kind: EventKeyChanged, PeerID: du.Entry.DeviceID})
	}
	c.emit(Event{Kind: EventRosterChanged})
}

func (c *Client) handlePresence(env *proto.Envelope) {
	var pu proto.PresenceUpdate
	if err := env.Unmarshal(&pu); err != nil {
		return
	}
	c.mu.Lock()
	c.peerPresence[pu.DeviceID] = pu
	c.mu.Unlock()
	puCopy := pu
	c.emit(Event{Kind: EventPresence, PeerID: pu.DeviceID, Presence: &puCopy})
}

// --- request/response correlation ----------------------------------

func (c *Client) newPending(id string) chan *proto.Envelope {
	ch := make(chan *proto.Envelope, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	return ch
}

func (c *Client) routeReply(env *proto.Envelope) {
	if env.ID == "" {
		return
	}
	c.pendingMu.Lock()
	ch := c.pending[env.ID]
	delete(c.pending, env.ID)
	c.pendingMu.Unlock()
	if ch != nil {
		ch <- env
	}
}

func (c *Client) cancelPending(id string) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}
