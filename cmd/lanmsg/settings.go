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

	form := widget.NewForm(
		widget.NewFormItem("Notifications", notify),
		widget.NewFormItem("Downloads folder", downloads),
		widget.NewFormItem("This device's fingerprint", myFP),
		widget.NewFormItem("Relay", serverInfo),
	)

	content := container.NewVBox(
		form,
		widget.NewSeparator(),
		widget.NewLabel("Read your fingerprint aloud to family so they can\nverify this device in their roster."),
	)

	d := dialog.NewCustomConfirm("Settings", "Save", "Cancel", content, func(ok bool) {
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
