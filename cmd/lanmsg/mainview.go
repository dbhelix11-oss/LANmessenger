package main

import (
	"context"
	"fmt"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/proto"
)

// showMain builds and installs the primary two-pane view.
func (g *guiApp) showMain() {
	g.roster = newRosterView(g)
	g.convo = newConversationView(g)

	g.connLabel = widget.NewLabel("Connecting…")

	g.statusSel = widget.NewSelect(statusNames(), func(name string) {
		g.setStatus(statusFromName(name))
	})
	g.statusSel.SetSelected(statusLabel(g.client.DesiredStatus().Status))

	toolbar := container.NewHBox(
		widget.NewLabel("Status:"),
		g.statusSel,
		widget.NewSeparator(),
		g.connLabel,
	)

	// The admin button is always built but only shown for admin devices. Admin
	// status is learned asynchronously during the handshake, so onConnState
	// toggles its visibility when the connection becomes ready.
	g.adminBtn = widget.NewButtonWithIcon("Pending devices", theme.ConfirmIcon(), g.showAdminPanel)
	g.adminBtn.Hidden = !g.client.IsAdmin()

	versionLbl := widget.NewLabel(versionLabel())
	versionLbl.TextStyle = fyne.TextStyle{Italic: true}

	right := container.NewHBox(
		g.adminBtn,
		widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), g.showSettings),
		widget.NewSeparator(),
		versionLbl,
	)

	top := container.NewBorder(nil, nil, toolbar, right)

	split := container.NewHSplit(g.roster.object(), g.convo.object())
	split.Offset = 0.3

	g.win.SetContent(container.NewBorder(container.NewVBox(top, widget.NewSeparator()), nil, nil, nil, split))
	g.refreshRoster()
}

func statusNames() []string {
	out := make([]string, len(selectableStatuses))
	for i, s := range selectableStatuses {
		out[i] = statusLabel(s)
	}
	return out
}

func statusFromName(name string) proto.Status {
	for _, s := range selectableStatuses {
		if statusLabel(s) == name {
			return s
		}
	}
	return proto.StatusAvailable
}

// setStatus publishes a new presence and updates the tray.
func (g *guiApp) setStatus(s proto.Status) {
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	defer cancel()
	if err := g.client.SetStatus(ctx, s, ""); err != nil {
		g.setStatusLine(err.Error())
	}
	if g.statusSel != nil && g.statusSel.Selected != statusLabel(s) {
		g.statusSel.SetSelected(statusLabel(s))
	}
	g.refreshTray()
}

// refreshRoster reloads the roster list from the client.
func (g *guiApp) refreshRoster() {
	if g.roster == nil {
		return
	}
	entries, err := g.client.Roster()
	if err != nil {
		g.setStatusLine(err.Error())
		return
	}
	g.roster.setEntries(entries)
}

func (g *guiApp) peerName(id string) string {
	entries, _ := g.client.Roster()
	for _, e := range entries {
		if e.DeviceID == id {
			return e.DisplayName
		}
	}
	return id[:8]
}

// setStatusLine shows a short transient message in the conversation header area.
func (g *guiApp) setStatusLine(msg string) {
	if g.convo != nil {
		g.convo.setNote(msg)
	} else {
		g.log.Info("status", "msg", msg)
	}
}

// onKeyChanged warns that a peer's keys rotated and offers to open verification.
func (g *guiApp) onKeyChanged(peerID string) {
	name := g.peerName(peerID)
	dialog.ShowCustomConfirm(
		"Security key changed",
		"Verify now", "Later",
		widget.NewLabel(fmt.Sprintf(
			"%s's security keys have changed.\n\nThis is expected if they reinstalled or set up a\nnew device. If you did not expect it, confirm the\nnew fingerprint with them over another channel\nbefore sending anything sensitive.", name)),
		func(verify bool) {
			if verify {
				g.showVerifyDialog(peerID)
			}
		}, g.win)
}

// selectPeer loads a conversation into the right pane.
func (g *guiApp) selectPeer(e clientcore.RosterEntry) {
	history, err := g.client.History(e.DeviceID, 500)
	if err != nil {
		g.setStatusLine(err.Error())
		return
	}
	g.convo.setPeer(e, history)
	g.clearUnreadFrom(e.DeviceID)
}

// showPeerMenu pops up the right-click menu for one person in the roster.
func (g *guiApp) showPeerMenu(e clientcore.RosterEntry, pos fyne.Position) {
	menu := fyne.NewMenu("",
		fyne.NewMenuItem("Verify fingerprint…", func() { g.showVerifyDialog(e.DeviceID) }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Delete history (for me only)…", func() { g.confirmClearHistory(e, false) }),
		fyne.NewMenuItem("Delete history (for me and "+e.DisplayName+")…", func() { g.confirmClearHistory(e, true) }),
	)
	widget.ShowPopUpMenuAtPosition(menu, g.win.Canvas(), pos)
}

// confirmClearHistory asks before deleting a conversation, then does it.
func (g *guiApp) confirmClearHistory(e clientcore.RosterEntry, both bool) {
	msg := fmt.Sprintf("Delete your copy of the conversation with %s?\n\n%s's copy is not touched.", e.DisplayName, e.DisplayName)
	title := "Delete history for me"
	if both {
		title = "Delete history for both"
		msg = fmt.Sprintf("Delete the conversation with %s on this device AND ask %s's device to delete it too?\n\n"+
			"Their app performs the deletion when it receives the request (immediately if they are online,\n"+
			"otherwise the next time they connect). A device running an older version, or one that is\n"+
			"modified, may ignore the request. This cannot be undone.", e.DisplayName, e.DisplayName)
	}
	dialog.ShowConfirm(title, msg, func(ok bool) {
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
		defer cancel()
		if err := g.client.ClearHistory(ctx, e.DeviceID, both); err != nil {
			dialog.ShowError(err, g.win)
			return
		}
		g.reloadConversation(e.DeviceID)
		g.refreshRoster()
	}, g.win)
}

// reloadConversation redraws the open conversation if it is with peerID.
func (g *guiApp) reloadConversation(peerID string) {
	if g.convo == nil || g.convo.peerID != peerID {
		return
	}
	history, err := g.client.History(peerID, 500)
	if err != nil {
		g.setStatusLine(err.Error())
		return
	}
	g.convo.setPeer(g.convo.peer, history)
}
