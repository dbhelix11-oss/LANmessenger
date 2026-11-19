package main

import (
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

const (
	prefNotify = "notifications_enabled"
)

// showSettings opens the settings dialog.
func (g *guiApp) showSettings() {
	prefs := g.fapp.Preferences()

	notify := widget.NewCheck("Show desktop notifications for new messages", func(v bool) {
		prefs.SetBool(prefNotify, v)
	})
	notify.SetChecked(prefs.BoolWithFallback(prefNotify, true))

	downloads := widget.NewEntry()
	dl, _ := g.cfg.ResolvedDownloadsDir()
	downloads.SetText(dl)

	myFP := widget.NewLabel(g.client.Fingerprint())
	myFP.TextStyle = fyne.TextStyle{Monospace: true}

	serverInfo := widget.NewLabel(fmt.Sprintf("%s\n%s", g.cfg.ServerAddr, g.cfg.CertFingerprint))
	serverInfo.TextStyle = fyne.TextStyle{Monospace: true}

	// Your own name: shown as it is now; an admin (you) can change it here. Everyone
	// else's roster, and this device, take the new name.
	var d dialog.Dialog
	myName := container.NewHBox(widget.NewLabel(g.client.DisplayName()))
	if g.adminAvailable() {
		myName.Add(widget.NewButton("Change… (admin)", func() {
			if d != nil {
				d.Hide()
			}
			g.promptRename(g.client.DeviceID(), g.client.DisplayName(), nil)
		}))
	}

	form := widget.NewForm(
		widget.NewFormItem("My display name", myName),
		widget.NewFormItem("Notifications", notify),
		widget.NewFormItem("Downloads folder", downloads),
		widget.NewFormItem("This device's fingerprint", myFP),
		widget.NewFormItem("Relay", serverInfo),
	)

	checkBtn := widget.NewButton("Check for updates", func() { g.runUpdateCheck(true) })
	resetBtn := widget.NewButton("Delete connection settings and re-enroll…", func() {
		if d != nil {
			d.Hide()
		}
		g.confirmReset()
	})
	resetBtn.Importance = widget.DangerImportance

	content := container.NewVBox(
		form,
		widget.NewSeparator(),
		container.NewHBox(widget.NewLabel("Version "+versionLabel()), checkBtn),
		widget.NewSeparator(),
		resetBtn,
		widget.NewSeparator(),
		widget.NewLabel("Read your fingerprint aloud to family so they can\nverify this device in their roster."),
	)

	d = dialog.NewCustomConfirm("Settings", "Save", "Cancel", content, func(ok bool) {
		if !ok {
			return
		}
		g.cfg.DownloadsDir = downloads.Text
		if err := g.cfg.Save(); err != nil {
			dialog.ShowError(err, g.win)
		}
	}, g.win)
	d.Resize(fyne.NewSize(560, 420))
	d.Show()
}

// showVerifyDialog lets the user confirm a peer's fingerprint out of band.
func (g *guiApp) showVerifyDialog(peerID string) {
	fp := g.client.PeerFingerprint(peerID)
	name := g.peerName(peerID)

	lbl := widget.NewLabel(fp)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}

	body := container.NewVBox(
		widget.NewLabel(fmt.Sprintf("Ask %s to read their device fingerprint aloud\n(Settings → This device's fingerprint) and check\nit matches:", name)),
		lbl,
	)
	dialog.ShowCustomConfirm("Verify "+name, "It matches", "Cancel", body, func(ok bool) {
		if !ok {
			return
		}
		if err := g.client.MarkVerified(peerID); err != nil {
			dialog.ShowError(err, g.win)
			return
		}
		g.refreshRoster()
		if g.convo != nil && g.convo.peerID == peerID {
			g.convo.refreshHeader()
		}
	}, g.win)
}

// confirmReset explains what resetting does and asks before doing it.
func (g *guiApp) confirmReset() {
	wipe := widget.NewCheck("Also delete message history and this device's keys (start as a brand-new device)", nil)
	body := container.NewVBox(
		widget.NewLabel("This forgets the relay address, the pinned certificate, the saved\n"+
			"household passphrase and this device's enrollment, then opens the\n"+
			"setup screen so you can join a relay again.\n\n"+
			"By default your message history and device keys are kept, so you\n"+
			"re-enroll as the same device. A relay that has been reset or\n"+
			"replaced will treat you as new and ask for the passphrase."),
		wipe,
	)
	dialog.ShowCustomConfirm("Re-enroll this device", "Delete and re-enroll", "Cancel", body, func(ok bool) {
		if ok {
			g.resetConnection(wipe.Checked)
		}
	}, g.win)
}
