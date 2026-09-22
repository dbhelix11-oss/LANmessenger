//go:build linux || freebsd || openbsd || netbsd

package main

import (
	"sync"

	"fyne.io/fyne/v2"
	"github.com/godbus/dbus/v5"
)

// notifyExpireMS is the fallback expiry when the notification server can't
// tell us it supports being clicked (see notifyServerSupportsClick).
//
// Fyne's built-in App.SendNotification passes expire_timeout=0 to the
// freedesktop Notify method, which the spec defines as "never expire". Full
// desktop environments override that, but a minimal X11 notifier honours it
// literally: the popup sticks forever with no close button. We send the
// D-Bus request ourselves so we can choose per-server: never-expire (and let
// a click dismiss it) when the server can tell us it supports that, this
// fixed timeout otherwise.
const notifyExpireMS = 6000

var (
	notifyCapsOnce      sync.Once
	notifySupportsClick bool

	notifyWatchOnce sync.Once

	notifyPendingMu  sync.Mutex
	notifyPendingIDs = map[uint32]struct{}{}
)

// notifyServerSupportsClick reports whether the session's notification
// server advertises the "actions" capability — i.e. it can tell us a
// notification was clicked, so it's safe to ask it to never auto-expire.
// Checked once per process; a notification server doesn't change mid-session.
func notifyServerSupportsClick(conn *dbus.Conn) bool {
	notifyCapsOnce.Do(func() {
		obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
		var caps []string
		if err := obj.Call("org.freedesktop.Notifications.GetCapabilities", 0).Store(&caps); err == nil {
			for _, c := range caps {
				if c == "actions" {
					notifySupportsClick = true
					break
				}
			}
		}
	})
	return notifySupportsClick
}

// sendNotification shows a desktop notification. When the notification
// server supports it, it persists until clicked (or otherwise dismissed)
// rather than auto-expiring, and clicking it brings the window to front and
// clears the tray's unread badge; otherwise it falls back to a fixed expiry.
func (g *guiApp) sendNotification(n *fyne.Notification) {
	conn, err := dbus.SessionBus() // shared connection, do not close
	if err != nil {
		g.log.Warn("notification: no session bus, using Fyne fallback", "err", err)
		g.fapp.SendNotification(n)
		return
	}

	expire := int32(notifyExpireMS)
	var actions []string
	if notifyServerSupportsClick(conn) {
		expire = 0 // never auto-expire; watchNotifyClicks dismisses it on click
		actions = []string{"default", ""}
		g.ensureNotifyClickWatcher(conn)
	}

	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"lanmessenger",            // app_name
		uint32(0),                 // replaces_id (0 = always a new bubble)
		"",                        // app_icon (theme lookup; empty is fine)
		n.Title,                   // summary
		n.Content,                 // body
		actions,                   // actions ([]string{}: none; ["default", ""]: clickable)
		map[string]dbus.Variant{}, // hints
		expire,                    // expire_timeout in ms (0 = never)
	)
	if call.Err != nil {
		g.log.Warn("notification: D-Bus Notify failed, using Fyne fallback", "err", call.Err)
		g.fapp.SendNotification(n)
		return
	}
	if len(actions) > 0 && len(call.Body) > 0 {
		if id, ok := call.Body[0].(uint32); ok && id != 0 {
			notifyPendingMu.Lock()
			notifyPendingIDs[id] = struct{}{}
			notifyPendingMu.Unlock()
		}
	}
}

// ensureNotifyClickWatcher starts (once) a background goroutine that listens
// for this session's notifications being clicked or otherwise dismissed, so a
// never-expiring notification (see sendNotification) doesn't just sit there
// forever with nothing watching for the click that's supposed to end it.
func (g *guiApp) ensureNotifyClickWatcher(conn *dbus.Conn) {
	notifyWatchOnce.Do(func() { go g.watchNotifyClicks(conn) })
}

func (g *guiApp) watchNotifyClicks(conn *dbus.Conn) {
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.Notifications")); err != nil {
		g.log.Debug("notification click-watch: AddMatchSignal failed", "err", err)
		return
	}
	ch := make(chan *dbus.Signal, 16)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	for {
		select {
		case <-g.ctx.Done():
			return
		case sig, ok := <-ch:
			if !ok {
				return
			}
			if len(sig.Body) == 0 {
				continue
			}
			id, ok := sig.Body[0].(uint32)
			if !ok {
				continue
			}
			switch sig.Name {
			case "org.freedesktop.Notifications.ActionInvoked":
				if forgetNotifyID(id) {
					fyne.Do(func() {
						g.trayHidden.Store(false)
						g.win.Show()
						g.win.RequestFocus()
						g.clearUnread()
					})
				}
			case "org.freedesktop.Notifications.NotificationClosed":
				forgetNotifyID(id) // dismissed some other way (timeout/close call); stop tracking it
			}
		}
	}
}

// forgetNotifyID removes id from the pending set and reports whether it was
// one of ours (as opposed to some other application's notification sharing
// the same session bus).
func forgetNotifyID(id uint32) bool {
	notifyPendingMu.Lock()
	defer notifyPendingMu.Unlock()
	if _, ok := notifyPendingIDs[id]; !ok {
		return false
	}
	delete(notifyPendingIDs, id)
	return true
}
