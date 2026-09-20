package servercore

import (
	"strings"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

func nowMillis() int64 { return time.Now().UnixMilli() }

// dispatch routes one inbound frame from a connected client.
func (s *Server) dispatch(c *conn, env *proto.Envelope) {
	if !s.frameLimiter.Allow(c.deviceID) {
		c.sendError(env.ID, proto.ErrRateLimited, "too many requests")
		return
	}
	switch env.Type {
	case proto.TypePing:
		c.trySend(proto.TypePong, env.ID, nil)

	case proto.TypePresenceSet:
		s.handlePresenceSet(c, env)

	case proto.TypeMsg:
		s.handleMsg(c, env)

	case proto.TypeMsgAck:
		s.handleMsgAck(c, env)

	case proto.TypeAdminListPending:
		s.handleAdminListPending(c, env)

	case proto.TypeAdminApprove:
		s.handleAdminAction(c, env, "approve")

	case proto.TypeAdminDeny:
		s.handleAdminAction(c, env, "deny")

	default:
		c.sendError(env.ID, proto.ErrBadRequest, "unsupported frame type: "+string(env.Type))
	}
}

// requireReady rejects frames from a device still awaiting admin approval.
func (s *Server) requireReady(c *conn, id string) bool {
	if c.isReady() {
		return true
	}
	c.sendError(id, proto.ErrPending, "device is awaiting admin approval")
	return false
}

func (s *Server) handlePresenceSet(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var ps proto.PresenceSet
	if err := env.Unmarshal(&ps); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	if !ps.Status.Valid() {
		c.sendError(env.ID, proto.ErrBadRequest, "unknown status")
		return
	}
	msg := strings.TrimSpace(ps.Message)
	if len(msg) > 140 {
		msg = msg[:140]
	}
	c.setStatus(ps.Status, msg)
	if err := s.store.setPresence(c.deviceID, ps.Status, msg); err != nil {
		s.log.Warn("persist presence failed", "device", c.deviceID, "err", err)
	}
	s.broadcastPresence(c, true)
}

func (s *Server) handleMsg(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var m proto.Msg
	if err := env.Unmarshal(&m); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	if m.To == "" || m.Ciphertext == "" || m.Nonce == "" || m.MsgID == "" {
		c.sendError(env.ID, proto.ErrBadRequest, "msg missing required fields")
		return
	}
	if len(m.Ciphertext) > s.cfg.MaxFrameBytes {
		c.sendError(env.ID, proto.ErrTooLarge, "message exceeds size limit")
		return
	}

	recipient, err := s.store.getDevice(m.To)
	if err != nil || recipient.State != proto.StateActive {
		c.sendError(env.ID, proto.ErrNotFound, "recipient not found or not active")
		return
	}

	// Server stamps the authenticated sender; clients never set From themselves.
	m.From = c.deviceID
	if m.TS == 0 {
		m.TS = nowMillis()
	}

	if dst, ok := s.lookupConn(m.To); ok && dst.isReady() {
		dst.trySend(proto.TypeMsg, "", m)
		return
	}

	// Offline: queue for later delivery.
	if err := s.store.enqueue(m.To, queuedMsg{
		Sender:     m.From,
		MsgID:      m.MsgID,
		Nonce:      m.Nonce,
		Ciphertext: m.Ciphertext,
		TS:         m.TS,
	}, s.cfg.MaxQueuePerDevice); err != nil {
		s.log.Warn("enqueue failed", "to", m.To, "err", err)
		c.sendError(env.ID, proto.ErrInternal, "could not queue message")
	}
}

func (s *Server) handleMsgAck(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var ack proto.MsgAck
	if err := env.Unmarshal(&ack); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	if ack.MsgID == "" || ack.Peer == "" {
		c.sendError(env.ID, proto.ErrBadRequest, "ack missing msg_id or peer")
		return
	}
	// Drop the delivered message from this device's queue (no-op if not queued).
	if err := s.store.ackQueued(c.deviceID, ack.Peer, ack.MsgID); err != nil {
		s.log.Warn("ack purge failed", "device", c.deviceID, "err", err)
	}
	// Relay the ack to the original sender, rewriting Peer to the acking device.
	if sender, ok := s.lookupConn(ack.Peer); ok && sender.isReady() {
		sender.trySend(proto.TypeMsgAck, "", proto.MsgAck{MsgID: ack.MsgID, Peer: c.deviceID})
	}
}

func (s *Server) handleAdminListPending(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	if !c.admin {
		c.sendError(env.ID, proto.ErrForbidden, "not an admin device")
		return
	}
	pending, err := s.store.listDevicesByState(proto.StatePending)
	if err != nil {
		c.sendError(env.ID, proto.ErrInternal, err.Error())
		return
	}
	c.trySend(proto.TypeAdminPendingList, env.ID, proto.AdminPendingList{Entries: pending})
}

// handleAdminAction processes admin_approve / admin_deny.
func (s *Server) handleAdminAction(c *conn, env *proto.Envelope, action string) {
	if !s.requireReady(c, env.ID) {
		return
	}
	if !c.admin {
		c.sendError(env.ID, proto.ErrForbidden, "not an admin device")
		return
	}

	var (
		targetID string
		sig      string
	)
	switch action {
	case "approve":
		var a proto.AdminApprove
		if err := env.Unmarshal(&a); err != nil {
			c.sendError(env.ID, proto.ErrBadRequest, err.Error())
			return
		}
		targetID, sig = a.DeviceID, a.Signature
	case "deny":
		var d proto.AdminDeny
		if err := env.Unmarshal(&d); err != nil {
			c.sendError(env.ID, proto.ErrBadRequest, err.Error())
			return
		}
		targetID, sig = d.DeviceID, d.Signature
	}

	// The admin signs "approve:<deviceID>" / "deny:<deviceID>" with its own
	// signing key; verify against the key on file for this admin device.
	ok, err := crypto.VerifyFromB64Pub(c.entry.SignPub, proto.AdminActionMessage(action, targetID), sig)
	if err != nil || !ok {
		c.sendError(env.ID, proto.ErrForbidden, "invalid admin signature")
		return
	}

	target, err := s.store.getDevice(targetID)
	if err != nil {
		c.sendError(env.ID, proto.ErrNotFound, "no such device")
		return
	}
	if target.State != proto.StatePending {
		c.sendError(env.ID, proto.ErrBadRequest, "device is not pending")
		return
	}

	if action == "deny" {
		if err := s.store.setDeviceState(targetID, proto.StateDenied); err != nil {
			c.sendError(env.ID, proto.ErrInternal, err.Error())
			return
		}
		s.log.Info("admin denied device", "admin", c.deviceID, "device", targetID)
		if tc, ok := s.lookupConn(targetID); ok {
			tc.sendError("", proto.ErrForbidden, "your enrollment was denied")
			tc.close(proto.ErrForbidden, "denied by admin")
		}
		return
	}

	if err := s.store.setDeviceState(targetID, proto.StateActive); err != nil {
		c.sendError(env.ID, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("admin approved device", "admin", c.deviceID, "device", targetID)

	updated, _ := s.store.getDevice(targetID)
	s.broadcastDirectoryUpdate(updated, false)

	// If the approved device is connected and waiting, promote it live.
	if tc, ok := s.lookupConn(targetID); ok && !tc.isReady() {
		s.markReady(tc, updated)
		s.afterReady(tc)
	}
}
