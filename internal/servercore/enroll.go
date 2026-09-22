package servercore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
	"lanmessenger/internal/version"
)

const maxDisplayNameLen = 64

// resumeDevice handles a client that already has a device ID.
func (s *Server) resumeDevice(c *conn, deviceID string) error {
	entry, err := s.store.getDevice(deviceID)
	if err != nil {
		if errors.Is(err, errNoDevice) {
			c.sendError("", proto.ErrNotFound, "unknown device; enroll again")
			return errors.New("resume: unknown device")
		}
		return err
	}

	switch entry.State {
	case proto.StateDenied:
		c.sendError("", proto.ErrForbidden, "this device was denied")
		return errors.New("resume: denied device")
	case proto.StatePending:
		c.deviceID = entry.DeviceID
		c.entry = entry
		// Keep last-known status but stay not-ready until approved.
		return c.send(proto.TypeEnrollResult, "", proto.EnrollResult{
			DeviceID: entry.DeviceID, State: proto.StatePending,
		})
	default: // active
		s.markReady(c, entry)
		return nil
	}
}

// enrollDevice handles a first-time client: read the enroll frame, register the
// device, and either activate it or leave it pending.
func (s *Server) enrollDevice(ctx context.Context, c *conn) error {
	env, err := readFrame(ctx, c.ws)
	if err != nil {
		return err
	}
	if env.Type != proto.TypeEnroll {
		return errors.New("expected enroll")
	}
	var en proto.Enroll
	if err := env.Unmarshal(&en); err != nil {
		return err
	}

	name := strings.TrimSpace(en.DisplayName)
	if name == "" || len(name) > maxDisplayNameLen {
		c.sendError("", proto.ErrBadRequest, "display name must be 1..64 characters")
		return errors.New("enroll: bad display name")
	}
	signPub, err := crypto.DecodeSignPub(en.SignPub)
	if err != nil {
		c.sendError("", proto.ErrBadRequest, "invalid signing key")
		return err
	}
	if _, err := crypto.DecodeKey32(en.BoxPub); err != nil {
		c.sendError("", proto.ErrBadRequest, "invalid box key")
		return err
	}

	// The server derives the device ID authoritatively from the signing key so a
	// client cannot claim someone else's ID.
	sum := sha256.Sum256(signPub)
	deviceID := hex.EncodeToString(sum[:16])

	count, err := s.store.countDevices()
	if err != nil {
		return err
	}
	firstDevice := count == 0

	state := proto.StateActive
	admin := firstDevice
	if s.cfg.RequireAdminApproval && !firstDevice {
		state = proto.StatePending
	}

	entry := proto.DirectoryEntry{
		DeviceID:    deviceID,
		DisplayName: name,
		SignPub:     en.SignPub,
		BoxPub:      en.BoxPub,
		State:       state,
		Admin:       admin,
	}
	if err := s.store.upsertDevice(entry); err != nil {
		return err
	}

	if firstDevice {
		s.cfgMu.Lock()
		if s.cfg.AddAdmin(deviceID) {
			if err := s.cfg.Save(); err != nil {
				s.log.Warn("could not persist first-admin to config", "err", err)
			}
		}
		s.cfgMu.Unlock()
		s.log.Info("first device enrolled as admin", "device", deviceID, "name", name)
	}

	if err := c.send(proto.TypeEnrollResult, "", proto.EnrollResult{DeviceID: deviceID, State: state}); err != nil {
		return err
	}

	if state == proto.StateActive {
		s.markReady(c, entry)
	} else {
		c.deviceID = deviceID
		c.entry = entry
		s.log.Info("device enrolled, awaiting approval", "device", deviceID, "name", name)
	}
	return nil
}

// markReady flips a connection into the ready state and records identity. The
// caller (or afterReady) is responsible for the post-ready sends.
func (s *Server) markReady(c *conn, entry proto.DirectoryEntry) {
	c.deviceID = entry.DeviceID
	c.entry = entry
	c.admin = s.cfg.IsAdmin(entry.DeviceID) || entry.Admin
	if last, err := s.store.getPresence(entry.DeviceID); err == nil {
		c.setStatus(last.Status, last.Message)
	}
	c.ready.Store(true)
}

// afterReady performs the sends every freshly-ready connection needs: the ready
// frame, a directory snapshot, presence of everyone else, a drain of the offline
// queue, and broadcasting this device's arrival to others.
func (s *Server) afterReady(c *conn) {
	c.trySend(proto.TypeReady, "", proto.Ready{
		DeviceID:         c.deviceID,
		Admin:            c.admin,
		ServerVersion:    version.Version,
		MinClientVersion: s.cfg.MinClientVersion,
	})

	entries, err := s.store.listDevicesByState(proto.StateActive)
	if err != nil {
		s.log.Warn("directory snapshot failed", "err", err)
	} else {
		filtered := entries[:0]
		for _, e := range entries {
			if e.DeviceID != c.deviceID {
				filtered = append(filtered, e)
			}
		}
		c.trySend(proto.TypeDirectorySnapshot, "", proto.DirectorySnapshot{Entries: filtered})
	}

	// Presence of everyone currently connected.
	for _, other := range s.readyConns(c) {
		c.trySend(proto.TypePresenceUpdate, "", proto.PresenceUpdate{
			DeviceID: other.deviceID,
			Status:   other.getStatus(),
			Message:  other.getStatusMsg(),
			Online:   other.getStatus() != proto.StatusInvisible,
			TS:       nowMillis(),
		})
	}

	// Deliver anything queued while this device was offline.
	if queued, err := s.store.drain(c.deviceID); err != nil {
		s.log.Warn("queue drain failed", "device", c.deviceID, "err", err)
	} else {
		for _, m := range queued {
			c.trySend(proto.TypeMsg, "", proto.Msg{
				From:       m.Sender,
				To:         c.deviceID,
				MsgID:      m.MsgID,
				Nonce:      m.Nonce,
				Ciphertext: m.Ciphertext,
				TS:         m.TS,
			})
		}
	}

	// Tell everyone else this device is now online.
	s.broadcastPresence(c, true)
	s.broadcastDirectoryUpdate(c.entry, false)
}
