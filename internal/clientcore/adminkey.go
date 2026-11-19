package clientcore

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
	"lanmessenger/internal/version"
)

// ErrNoAdminKey is returned by admin calls made while admin mode is locked.
var ErrNoAdminKey = errors.New("clientcore: admin mode is locked (no admin key loaded)")

// SetAdminKey unlocks admin mode with the household admin private key. Pass nil
// to lock again. The key lives only in memory.
func (c *Client) SetAdminKey(k ed25519.PrivateKey) {
	c.mu.Lock()
	c.adminKey = k
	c.mu.Unlock()
}

// HasAdminKey reports whether admin mode is unlocked.
func (c *Client) HasAdminKey() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.adminKey) == ed25519.PrivateKeySize
}

// adminAuth signs an admin frame, or returns nil auth when no key is loaded
// (a relay still in legacy mode then falls back to the device-admin check).
func (c *Client) adminAuth(t proto.Type, target string) (*proto.AdminAuth, error) {
	c.mu.RLock()
	k := c.adminKey
	c.mu.RUnlock()
	if len(k) != ed25519.PrivateKeySize {
		return nil, nil
	}
	nonce, err := crypto.NewAdminAuthNonce()
	if err != nil {
		return nil, err
	}
	ts := time.Now().UnixMilli()
	return &proto.AdminAuth{TS: ts, Nonce: nonce, Sig: crypto.Sign(k, proto.AdminAuthMessage(t, target, ts, nonce))}, nil
}

// adminRequest sends an admin frame and waits for its reply envelope.
func (c *Client) adminRequest(ctx context.Context, t proto.Type, target string, build func(*proto.AdminAuth) any) (*proto.Envelope, error) {
	if !c.HasAdminKey() {
		return nil, ErrNoAdminKey
	}
	w := c.currentConn()
	if w == nil || c.State() != StateReady {
		return nil, ErrNotReady
	}
	auth, err := c.adminAuth(t, target)
	if err != nil {
		return nil, err
	}
	id := randID()
	ch := c.newPending(id)
	defer c.cancelPending(id)
	if err := w.send(ctx, t, id, build(auth)); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(adminReplyTimeout):
		return nil, fmt.Errorf("clientcore: timed out waiting for the relay")
	case env := <-ch:
		if env.Type == proto.TypeError {
			var e proto.ErrorBody
			_ = env.Unmarshal(&e)
			return nil, &RelayError{Code: e.Code, Message: e.Message}
		}
		return env, nil
	}
}

// AdminOverview returns the relay's full report on every device plus its settings.
// Needs the admin key.
func (c *Client) AdminOverview(ctx context.Context) (proto.AdminDeviceList, error) {
	env, err := c.adminRequest(ctx, proto.TypeAdminDevices, "", func(a *proto.AdminAuth) any {
		return proto.AdminDevices{Auth: a}
	})
	if err != nil {
		return proto.AdminDeviceList{}, err
	}
	var l proto.AdminDeviceList
	if err := env.Unmarshal(&l); err != nil {
		return proto.AdminDeviceList{}, err
	}
	return l, nil
}

// AdminDevices returns the relay's full report on every device. Needs the admin key.
func (c *Client) AdminDevices(ctx context.Context) ([]proto.DeviceReport, error) {
	l, err := c.AdminOverview(ctx)
	return l.Devices, err
}

// SetRequireApproval turns "hold every new device for admin approval" on or off on
// the relay (saved to its config).
func (c *Client) SetRequireApproval(ctx context.Context, on bool) error {
	target := "off"
	if on {
		target = "on"
	}
	_, err := c.adminRequest(ctx, proto.TypeAdminSetApproval, target, func(a *proto.AdminAuth) any {
		return proto.AdminSetApproval{Value: on, Auth: a}
	})
	return err
}

// RemoveDevice deletes a device from the relay; it must enroll again.
func (c *Client) RemoveDevice(ctx context.Context, deviceID string) error {
	_, err := c.adminRequest(ctx, proto.TypeAdminRemove, deviceID, func(a *proto.AdminAuth) any {
		return proto.AdminRemove{DeviceID: deviceID, Auth: a}
	})
	return err
}

// RevokeDevice withdraws a device's registration but keeps its record. Every later
// attempt by those credentials to return is refused, recorded and reported to the
// admin as an EventAdminAlert.
func (c *Client) RevokeDevice(ctx context.Context, deviceID string) error {
	_, err := c.adminRequest(ctx, proto.TypeAdminRevoke, deviceID, func(a *proto.AdminAuth) any {
		return proto.AdminRevoke{DeviceID: deviceID, Auth: a}
	})
	return err
}

// RenameDevice changes the display name the relay holds for a device. Everyone's
// roster (and the renamed device itself) picks it up.
func (c *Client) RenameDevice(ctx context.Context, deviceID, newName string) error {
	_, err := c.adminRequest(ctx, proto.TypeAdminRename, deviceID+"|"+newName, func(a *proto.AdminAuth) any {
		return proto.AdminRename{DeviceID: deviceID, NewName: newName, Auth: a}
	})
	return err
}

// ReauthDevice sends a device back to pending so it must be approved again.
func (c *Client) ReauthDevice(ctx context.Context, deviceID string) error {
	_, err := c.adminRequest(ctx, proto.TypeAdminReauth, deviceID, func(a *proto.AdminAuth) any {
		return proto.AdminReauth{DeviceID: deviceID, Auth: a}
	})
	return err
}

// SetDeviceModel lets a host app (e.g. Android) declare a hardware model in
// the signed facts it sends the relay. Optional.
func (c *Client) SetDeviceModel(m string) {
	c.mu.Lock()
	c.deviceModel = m
	c.mu.Unlock()
}

var (
	buildHashOnce sync.Once
	buildHashVal  string
)

// runningBinaryHash is the sha256 of this program's executable (same digest the
// update manifest uses), computed once. Empty if it cannot be read.
func runningBinaryHash() string {
	buildHashOnce.Do(func() {
		p, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(p)
		if err != nil {
			return
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			buildHashVal = hex.EncodeToString(h.Sum(nil))
		}
	})
	return buildHashVal
}

// sendFacts tells the relay what this client is, signed with the device key.
// Best effort: failure only means the admin sees less.
func (c *Client) sendFacts(ctx context.Context, w *wsConn) {
	c.mu.RLock()
	model := c.deviceModel
	c.mu.RUnlock()
	zone, _ := time.Now().Zone()
	f := proto.ClientFacts{
		Version:   version.Version,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		BuildHash: runningBinaryHash(),
		Model:     model,
		TZ:        zone,
	}
	f.Sig = crypto.Sign(c.id.SignPriv, f.FactsMessage(c.cfg.DeviceID))
	if err := w.send(ctx, proto.TypeClientFacts, "", f); err != nil {
		c.log.Debug("send facts failed", "err", err)
	}
}

// AdminDeviceView is the relay's report on one device (alias for callers that
// should not import proto directly).
type AdminDeviceView = proto.DeviceReport

// ClientKind names the program for the relay's Properties view ("gui", "cli",
// "remote-cli"; the Android app sets its own). Each main sets it at startup.
// It is declared, not proven.
var ClientKind = "client"

// adoptName makes the relay's name for this device our own (an admin renamed us).
func (c *Client) adoptName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	c.mu.Lock()
	same := c.cfg.DisplayName == name
	if !same {
		c.cfg.DisplayName = name
	}
	c.mu.Unlock()
	if same {
		return
	}
	if err := c.cfg.Save(); err != nil {
		c.log.Warn("could not save the new display name", "err", err)
	}
	c.emit(Event{Kind: EventSelfRenamed, Name: name})
}

// DisplayName is the name this device currently goes by (an admin may change it).
func (c *Client) DisplayName() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.DisplayName
}
