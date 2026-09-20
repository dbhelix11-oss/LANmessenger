// Command lanmsg is the lanmessenger desktop client: a Fyne GUI on top of
// internal/clientcore. It provides first-run setup, a roster with presence, 1:1
// chat with file transfer, a system-tray status switcher, notifications, and an
// admin panel for approving new devices.
package main

import (
	"context"
	"image/color"
	"log/slog"
	"os"
	"sync/atomic"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/proto"
)

// guiApp holds the running application: the Fyne handles, the client core, and
// references to the widgets the event pump needs to refresh.
type guiApp struct {
	fapp   fyne.App
	win    fyne.Window
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	cfg    *clientcore.Config
	client *clientcore.Client

	hasTray bool
	// trayHidden is true while the window is hidden to the tray (via the close
	// button or a minimize redirected by startMinimizeToTray).
	trayHidden atomic.Bool

	// main view widgets
	roster    *rosterView
	convo     *conversationView
	connLabel *widget.Label
	statusSel *widget.Select
	adminBtn  *widget.Button

	pumpStarted bool

	// adminReload, when set, refreshes the open admin panel.
	adminReload func()
}

// presenceColor maps a status to the colour of its roster dot.
func presenceColor(s proto.Status, online bool) color.Color {
	if !online && s != proto.StatusOffline {
		// Known status but the relay reports the device as not connected.
		return color.NRGBA{R: 0x88, G: 0x88, B: 0x88, A: 0xff}
	}
	switch s {
	case proto.StatusAvailable:
		return color.NRGBA{R: 0x2e, G: 0xb8, B: 0x59, A: 0xff}
	case proto.StatusAway:
		return color.NRGBA{R: 0xd6, G: 0x9a, B: 0x2d, A: 0xff}
	case proto.StatusBusy, proto.StatusDND:
		return color.NRGBA{R: 0xcc, G: 0x3a, B: 0x3a, A: 0xff}
	default:
		return color.NRGBA{R: 0x88, G: 0x88, B: 0x88, A: 0xff}
	}
}

// statusLabel is the human name for a status.
func statusLabel(s proto.Status) string {
	switch s {
	case proto.StatusAvailable:
		return "Available"
	case proto.StatusAway:
		return "Away"
	case proto.StatusBusy:
		return "Busy"
	case proto.StatusDND:
		return "Do not disturb"
	case proto.StatusInvisible:
		return "Invisible"
	default:
		return "Offline"
	}
}

var selectableStatuses = []proto.Status{
	proto.StatusAvailable,
	proto.StatusAway,
	proto.StatusBusy,
	proto.StatusDND,
	proto.StatusInvisible,
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LANMSG_DEBUG") != "" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
