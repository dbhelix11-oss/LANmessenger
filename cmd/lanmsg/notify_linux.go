//go:build linux || freebsd || openbsd || netbsd

package main

import (
	"fyne.io/fyne/v2"
	"github.com/godbus/dbus/v5"
)

// notifyExpireMS is how long a desktop notification stays on screen before the
// notification server retracts it.
//
// Fyne's built-in App.SendNotification passes expire_timeout=0 to the
// freedesktop Notify method, which the spec defines as "never expire". Full
// desktop environments override that, but a minimal X11 notifier honours it
// literally: the popup sticks forever with no close button. We send the D-Bus
// request ourselves with a real timeout instead, and fall back to Fyne if the
// session bus isn't reachable.
const notifyExpireMS = 6000

// sendNotification shows a desktop notification with a sane expiry.
func (g *guiApp) sendNotification(n *fyne.Notification) {
	conn, err := dbus.SessionBus() // shared connection, do not close
	if err != nil {
		g.log.Warn("notification: no session bus, using Fyne fallback", "err", err)
		g.fapp.SendNotification(n)
		return
	}

	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"lanmessenger",            // app_name
		uint32(0),                 // replaces_id (0 = always a new bubble)
		"",                        // app_icon (theme lookup; empty is fine)
		n.Title,                   // summary
		n.Content,                 // body
		[]string{},                // actions
		map[string]dbus.Variant{}, // hints
		int32(notifyExpireMS),     // expire_timeout in ms
	)
	if call.Err != nil {
		g.log.Warn("notification: D-Bus Notify failed, using Fyne fallback", "err", call.Err)
		g.fapp.SendNotification(n)
	}
}
