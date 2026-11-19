package servercore

import (
	"encoding/base64"
	"strings"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

// adminAuthWindow is how far an admin frame's timestamp may differ from the
// relay's clock. The nonce cache covers replays inside this window.
const adminAuthWindow = 2 * time.Minute

// adminAllowed decides whether c may perform an admin frame of type t on
// target. With an admin key configured, only a fresh signature from that key
// counts (any ready device may carry it; the key is the credential). Without
// one, only the legacy per-device admin flag is accepted, and only for the
// original approve/deny/list frames (allowLegacy). It sends the error reply
// itself and returns false on refusal.
func (s *Server) adminAllowed(c *conn, env *proto.Envelope, t proto.Type, target string, auth *proto.AdminAuth, allowLegacy bool) bool {
	// Admin is LAN-only, whatever the credentials: a connection that came in
	// through the Tor tunnel (or from a public address) never gets admin
	// powers, so a stolen key or a compromised remote box cannot use them.
	if !adminPathOK(c.path) {
		s.log.Warn("admin frame refused: not on the LAN path", "device", c.deviceID, "type", t, "path", c.path)
		c.sendError(env.ID, proto.ErrForbidden, "admin actions are only allowed from the home network")
		return false
	}
	if s.cfg.AdminKeyPub == "" {
		if allowLegacy && c.admin {
			return true
		}
		if !allowLegacy {
			c.sendError(env.ID, proto.ErrForbidden, "no admin key is configured on this relay")
		} else {
			c.sendError(env.ID, proto.ErrForbidden, "not an admin device")
		}
		return false
	}
	if auth == nil || auth.Sig == "" || len(auth.Nonce) < 8 {
		c.sendError(env.ID, proto.ErrForbidden, "admin key signature required")
		return false
	}
	now := time.Now()
	skew := now.Sub(time.UnixMilli(auth.TS))
	if skew < -adminAuthWindow || skew > adminAuthWindow {
		c.sendError(env.ID, proto.ErrForbidden, "admin signature is stale; check clocks")
		return false
	}
	ok, err := crypto.VerifyFromB64Pub(s.cfg.AdminKeyPub, proto.AdminAuthMessage(t, target, auth.TS, auth.Nonce), auth.Sig)
	if err != nil || !ok {
		s.log.Warn("admin frame rejected: bad signature", "device", c.deviceID, "type", t)
		c.sendError(env.ID, proto.ErrForbidden, "invalid admin signature")
		return false
	}
	if !s.adminSeen.firstUse(auth.Nonce, now, adminAuthWindow) {
		c.sendError(env.ID, proto.ErrForbidden, "admin signature already used")
		return false
	}
	return true
}

// adminPathOK is the LAN-only rule: only the "lan" path may administer.
func adminPathOK(path string) bool { return path == "lan" }

func (s *Server) handleAdminDevices(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var req proto.AdminDevices
	if len(env.Data) > 0 {
		_ = env.Unmarshal(&req)
	}
	if !s.adminAllowed(c, env, proto.TypeAdminDevices, "", req.Auth, false) {
		return
	}
	entries, err := s.store.listDevicesByState()
	if err != nil {
		c.sendError(env.ID, proto.ErrInternal, err.Error())
		return
	}
	out := proto.AdminDeviceList{Devices: make([]proto.DeviceReport, 0, len(entries)), RequireApproval: s.requireApproval()}
	for _, e := range entries {
		rep := proto.DeviceReport{Entry: e, Fingerprint: fingerprintOf(e)}
		if oc, ok := s.lookupConn(e.DeviceID); ok && oc.isReady() {
			rep.Online = true
		}
		rep.CreatedAt, rep.ApprovedAt = s.store.deviceTimes(e.DeviceID)
		s.store.revocationInfo(e.DeviceID, &rep)
		if err := s.store.deviceRecords(e.DeviceID, &rep); err != nil {
			s.log.Warn("device records failed", "device", e.DeviceID, "err", err)
		}
		out.Devices = append(out.Devices, rep)
	}
	c.trySend(proto.TypeAdminDeviceList, env.ID, out)
}

func fingerprintOf(e proto.DirectoryEntry) string {
	sp, err := crypto.DecodeSignPub(e.SignPub)
	if err != nil {
		return ""
	}
	bp, err := crypto.DecodeKey32(e.BoxPub)
	if err != nil {
		return ""
	}
	return crypto.Fingerprint(sp, bp)
}

func (s *Server) handleAdminRemove(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var req proto.AdminRemove
	if err := env.Unmarshal(&req); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	if !s.adminAllowed(c, env, proto.TypeAdminRemove, req.DeviceID, req.Auth, false) {
		return
	}
	target, err := s.store.getDevice(req.DeviceID)
	if err != nil {
		c.sendError(env.ID, proto.ErrNotFound, "no such device")
		return
	}
	if err := s.store.removeDevice(req.DeviceID); err != nil {
		c.sendError(env.ID, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("admin removed device", "via", c.deviceID, "device", req.DeviceID, "name", target.DisplayName)
	s.broadcastDirectoryUpdate(target, true)
	if tc, ok := s.lookupConn(req.DeviceID); ok {
		tc.sendError("", proto.ErrForbidden, "you were removed by the admin; enroll again to rejoin")
		tc.close(proto.ErrForbidden, "removed by admin")
	}
	c.trySend(proto.TypeAdminOK, env.ID, nil)
}

func (s *Server) handleAdminReauth(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var req proto.AdminReauth
	if err := env.Unmarshal(&req); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	if !s.adminAllowed(c, env, proto.TypeAdminReauth, req.DeviceID, req.Auth, false) {
		return
	}
	target, err := s.store.getDevice(req.DeviceID)
	if err != nil {
		c.sendError(env.ID, proto.ErrNotFound, "no such device")
		return
	}
	if err := s.store.setDeviceState(req.DeviceID, proto.StatePending); err != nil {
		c.sendError(env.ID, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("admin forced re-authorization", "via", c.deviceID, "device", req.DeviceID, "name", target.DisplayName)
	s.broadcastDirectoryUpdate(target, true) // peers hide it until it is approved again
	if tc, ok := s.lookupConn(req.DeviceID); ok {
		tc.sendError("", proto.ErrPending, "the admin requires you to be approved again")
		tc.close(proto.ErrPending, "re-authorization required")
	}
	c.trySend(proto.TypeAdminOK, env.ID, nil)
}

// handleClientFacts stores a device's self-declared facts. The signature (made
// with the device's own key) tells us the facts came from the holder of that
// key; it does not make them true.
func (s *Server) handleClientFacts(c *conn, env *proto.Envelope) {
	if c.deviceID == "" {
		return
	}
	var f proto.ClientFacts
	if err := env.Unmarshal(&f); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	for _, v := range []*string{&f.Version, &f.OS, &f.Arch, &f.BuildHash, &f.Model, &f.TZ} {
		*v = clip(strings.TrimSpace(*v), 96)
	}
	verified := false
	if f.Sig != "" {
		if _, err := base64.StdEncoding.DecodeString(f.Sig); err == nil {
			ok, err := crypto.VerifyFromB64Pub(c.entry.SignPub, f.FactsMessage(c.deviceID), f.Sig)
			verified = err == nil && ok
		}
	}
	if err := s.store.recordFacts(c.deviceID, f, verified, time.Now().Unix()); err != nil {
		s.log.Warn("record facts failed", "device", c.deviceID, "err", err)
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// handleAdminRevoke withdraws a registration but keeps the record: the device is
// dropped, hidden from everyone else's directory, and every later attempt to sign
// in or enroll with the same credentials is refused, recorded and reported.
func (s *Server) handleAdminRevoke(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var req proto.AdminRevoke
	if err := env.Unmarshal(&req); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	if !s.adminAllowed(c, env, proto.TypeAdminRevoke, req.DeviceID, req.Auth, false) {
		return
	}
	target, err := s.store.getDevice(req.DeviceID)
	if err != nil {
		c.sendError(env.ID, proto.ErrNotFound, "no such device")
		return
	}
	if req.DeviceID == c.deviceID {
		c.sendError(env.ID, proto.ErrBadRequest, "you cannot revoke the device you are using")
		return
	}
	if err := s.store.setDeviceState(req.DeviceID, proto.StateRevoked); err != nil {
		c.sendError(env.ID, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("admin revoked registration", "via", c.deviceID, "device", req.DeviceID, "name", target.DisplayName)
	s.broadcastDirectoryUpdate(target, true)
	if tc, ok := s.lookupConn(req.DeviceID); ok {
		tc.sendError("", proto.ErrForbidden, "your registration was revoked by the admin")
		tc.close(proto.ErrForbidden, "registration revoked")
	}
	c.trySend(proto.TypeAdminOK, env.ID, nil)
}

// revokedAttempt records and reports something that resembles a revoked
// registration coming back. match is "key", "id" or "fingerprint" (see
// proto.AdminAlert); signals is a comma list for fingerprint matches.
func (s *Server) revokedAttempt(c *conn, e proto.DirectoryEntry, match, signals, newName, newID string) {
	now := time.Now().Unix()
	n, err := s.store.recordRevokedAttempt(e.DeviceID, match, signals, newName, newID, c, now)
	if err != nil {
		s.log.Warn("could not record revoked attempt", "device", e.DeviceID, "err", err)
	}
	s.log.Warn("revoked registration may be returning", "device", e.DeviceID, "name", e.DisplayName,
		"match", match, "signals", signals, "path", c.path, "attempt", n)
	alert := proto.AdminAlert{Kind: "revoked_attempt", DeviceID: e.DeviceID, Name: e.DisplayName,
		Match: match, Path: c.path, Count: n, TS: now, Signals: signals, NewName: newName, NewID: newID}
	// Only admin devices on the LAN are told; the numbers are also kept for
	// Properties and for the console's next refresh.
	for _, oc := range s.readyConns(nil) {
		if oc.admin && adminPathOK(oc.path) {
			oc.trySend(proto.TypeAdminAlert, "", alert)
		}
	}
}

// checkRevokedFingerprints compares a new registration's connection with how each
// revoked device used to connect, and raises an alert when at least two of
// {TLS shape, program/OS/CPU, LAN address} agree. The registration is NOT refused
// (a different key is a different registration; it still waits for approval): the
// admin is told it may be the same machine. Every install of one build shares a TLS
// shape and client string, so this is a hint, never proof.
func (s *Server) checkRevokedFingerprints(c *conn, newName, newID string) {
	profiles, err := s.store.revokedProfiles()
	if err != nil {
		s.log.Warn("revoked profiles failed", "err", err)
		return
	}
	for _, p := range profiles {
		if p.ID == newID {
			continue
		}
		sig := fingerprintSignals(p, c)
		if len(sig) >= 2 {
			s.revokedAttempt(c, proto.DirectoryEntry{DeviceID: p.ID, DisplayName: p.Name}, "fingerprint", strings.Join(sig, ","), newName, newID)
		}
	}
}

// requireApproval reads the live "hold new devices for approval" setting.
func (s *Server) requireApproval() bool {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.cfg.RequireAdminApproval
}

// handleAdminSetApproval changes the setting and writes it to the config file, so it
// survives a restart. If the file cannot be written the change is undone and the
// admin is told, rather than leaving memory and disk disagreeing.
func (s *Server) handleAdminSetApproval(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var req proto.AdminSetApproval
	if err := env.Unmarshal(&req); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	target := "off"
	if req.Value {
		target = "on"
	}
	if !s.adminAllowed(c, env, proto.TypeAdminSetApproval, target, req.Auth, false) {
		return
	}
	s.cfgMu.Lock()
	old := s.cfg.RequireAdminApproval
	s.cfg.RequireAdminApproval = req.Value
	err := s.cfg.Save()
	if err != nil {
		s.cfg.RequireAdminApproval = old
	}
	s.cfgMu.Unlock()
	if err != nil {
		c.sendError(env.ID, proto.ErrInternal, "could not save the setting: "+err.Error())
		return
	}
	s.log.Info("admin changed require_admin_approval", "via", c.deviceID, "now", req.Value, "was", old)
	c.trySend(proto.TypeAdminOK, env.ID, nil)
}

// cleanDisplayName trims a proposed name and checks it is 1..maxDisplayNameLen
// printable characters (no control characters, so a name cannot forge a second line).
func cleanDisplayName(n string) (string, bool) {
	n = strings.TrimSpace(n)
	if n == "" || len([]rune(n)) > maxDisplayNameLen {
		return "", false
	}
	for _, r := range n {
		if r < 32 || r == 127 {
			return "", false
		}
	}
	return n, true
}

// handleAdminRename changes a device's display name for everyone: the stored name,
// every other client's directory, and the renamed device itself (live if online, via
// the ready frame otherwise). Past conversations relabel too, because messages carry
// no names; clients look the sender up by device ID.
func (s *Server) handleAdminRename(c *conn, env *proto.Envelope) {
	if !s.requireReady(c, env.ID) {
		return
	}
	var req proto.AdminRename
	if err := env.Unmarshal(&req); err != nil {
		c.sendError(env.ID, proto.ErrBadRequest, err.Error())
		return
	}
	if !s.adminAllowed(c, env, proto.TypeAdminRename, req.DeviceID+"|"+req.NewName, req.Auth, false) {
		return
	}
	name, ok := cleanDisplayName(req.NewName)
	if !ok {
		c.sendError(env.ID, proto.ErrBadRequest, "a name must be 1..64 printable characters")
		return
	}
	target, err := s.store.getDevice(req.DeviceID)
	if err != nil {
		c.sendError(env.ID, proto.ErrNotFound, "no such device")
		return
	}
	if err := s.store.setDisplayName(req.DeviceID, name); err != nil {
		c.sendError(env.ID, proto.ErrInternal, err.Error())
		return
	}
	s.log.Info("admin renamed device", "via", c.deviceID, "device", req.DeviceID, "from", target.DisplayName, "to", name)
	target.DisplayName = name
	if target.State == proto.StateActive {
		s.broadcastDirectoryUpdate(target, false)
	}
	if tc, ok := s.lookupConn(req.DeviceID); ok {
		tc.trySend(proto.TypeProfileUpdate, "", proto.ProfileUpdate{DisplayName: name})
	}
	c.trySend(proto.TypeAdminOK, env.ID, nil)
}
