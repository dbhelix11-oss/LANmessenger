package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"

	"lanmessenger/internal/proto"
)

// buildTray installs a system-tray menu when the platform has one.
func (g *guiApp) buildTray() {
	desk, ok := g.fapp.(desktop.App)
	if !ok {
		return
	}
	g.hasTray = true
	desk.SetSystemTrayMenu(g.trayMenu())
}

func (g *guiApp) refreshTray() {
	desk, ok := g.fapp.(desktop.App)
	if !ok {
		return
	}
	desk.SetSystemTrayMenu(g.trayMenu())
}

func (g *guiApp) trayMenu() *fyne.Menu {
	show := fyne.NewMenuItem("Show lanmessenger", func() {
		g.win.Show()
		g.win.RequestFocus()
	})

	var current proto.Status
	if g.client != nil {
		current = g.client.DesiredStatus().Status
	}

	items := []*fyne.MenuItem{show, fyne.NewMenuItemSeparator()}
	for _, s := range selectableStatuses {
		s := s
		it := fyne.NewMenuItem(statusLabel(s), func() { g.setStatus(s) })
		it.Checked = s == current
		items = append(items, it)
	}
	items = append(items,
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", g.doQuit),
	)
	return fyne.NewMenu("lanmessenger", items...)
}
