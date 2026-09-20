//go:build !linux && !freebsd && !openbsd && !netbsd

package main

// startMinimizeToTray redirects the window manager's minimize action to the
// system tray. It's implemented for X11 only; on macOS and Windows the minimize
// button keeps its native behaviour (the dock / taskbar still holds the app).
// See docs/DESIGN.md for the per-OS plan.
func (g *guiApp) startMinimizeToTray() {}
