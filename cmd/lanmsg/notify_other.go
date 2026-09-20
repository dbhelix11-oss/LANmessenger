//go:build !linux && !freebsd && !openbsd && !netbsd

package main

import "fyne.io/fyne/v2"

// sendNotification shows a desktop notification. On macOS and Windows Fyne's
// own implementation already uses the platform's native, self-expiring
// notification centre, so there's nothing to work around here.
func (g *guiApp) sendNotification(n *fyne.Notification) {
	g.fapp.SendNotification(n)
}
