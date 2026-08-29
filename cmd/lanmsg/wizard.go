package main

import (
	"context"
	"fmt"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
)

// showWizard runs first-run setup, swapping screens into the main window.
func (g *guiApp) showWizard() {
	g.wizardServerScreen()
}

func (g *guiApp) wizardCard(title string, body fyne.CanvasObject) {
	card := container.NewVBox(
		widget.NewLabelWithStyle(title, fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		body,
	)
	// Top-aligned, full-width, with padding — reads like a normal settings page
	// rather than a cramped popover.
	g.win.SetContent(container.NewPadded(container.NewVBox(card)))
}

// Step 1: relay address.
func (g *guiApp) wizardServerScreen() {
	addr := widget.NewEntry()
	addr.SetPlaceHolder("relay-host:8443")
	if g.cfg.ServerAddr != "" {
		addr.SetText(g.cfg.ServerAddr)
	}

	next := widget.NewButton("Connect", func() {
		if addr.Text == "" {
			return
		}
		g.cfg.ServerAddr = addr.Text
		g.wizardFingerprintScreen()
	})
	next.Importance = widget.HighImportance

	g.wizardCard("Connect to your family's relay", container.NewVBox(
		widget.NewLabel("Enter the address your relay is running on.\nAsk whoever set it up, or check `lanmsg-server` output."),
		addr,
		next,
	))
}

// Step 2: confirm the TLS certificate fingerprint.
func (g *guiApp) wizardFingerprintScreen() {
	g.wizardCard("Checking the relay…", widget.NewProgressBarInfinite())

	go func() {
		ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
		defer cancel()
		fp, err := clientcore.FingerprintOfPresentedCert(ctx, g.cfg.ServerAddr)
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(fmt.Errorf("could not reach %s: %w", g.cfg.ServerAddr, err), g.win)
				g.wizardServerScreen()
				return
			}
			g.wizardConfirmFingerprint(fp)
		})
	}()
}

func (g *guiApp) wizardConfirmFingerprint(fp string) {
	fpLabel := widget.NewLabel(fp)
	fpLabel.TextStyle = fyne.TextStyle{Monospace: true}
	fpLabel.Wrapping = fyne.TextWrapBreak

	confirm := widget.NewButton("It matches — continue", func() {
		g.cfg.CertFingerprint = fp
		g.wizardIdentityScreen()
	})
	confirm.Importance = widget.HighImportance

	g.wizardCard("Confirm the relay's fingerprint", container.NewVBox(
		widget.NewLabel("The relay printed a SHA-256 fingerprint when it\nstarted. Check it matches this exactly:"),
		fpLabel,
		confirm,
		widget.NewButton("Back", g.wizardServerScreen),
	))
}

// Step 3: passphrase + display name, then enroll.
func (g *guiApp) wizardIdentityScreen() {
	name := widget.NewEntry()
	name.SetPlaceHolder("e.g. Dad's laptop")
	if g.cfg.DisplayName != "" {
		name.SetText(g.cfg.DisplayName)
	}
	pass := widget.NewPasswordEntry()
	pass.SetPlaceHolder("household passphrase")

	join := widget.NewButton("Join", func() {
		if name.Text == "" || pass.Text == "" {
			return
		}
		g.cfg.DisplayName = name.Text
		if err := g.cfg.Save(); err != nil {
			dialog.ShowError(err, g.win)
			return
		}
		g.wizardEnroll(name.Text, pass.Text)
	})
	join.Importance = widget.HighImportance

	g.wizardCard("Introduce this device", container.NewVBox(
		widget.NewForm(
			widget.NewFormItem("Display name", name),
			widget.NewFormItem("Passphrase", pass),
		),
		join,
		widget.NewButton("Back", g.wizardFingerprintScreen),
	))
}

func (g *guiApp) wizardEnroll(displayName, passphrase string) {
	g.wizardCard("Joining…", widget.NewProgressBarInfinite())

	client, err := clientcore.New(g.cfg, g.log)
	if err != nil {
		dialog.ShowError(err, g.win)
		g.wizardIdentityScreen()
		return
	}
	g.client = client

	go func() {
		ctx, cancel := context.WithTimeout(g.ctx, 20*time.Second)
		defer cancel()
		state, err := client.Enroll(ctx, displayName, passphrase)
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(err, g.win)
				g.wizardIdentityScreen()
				return
			}
			g.startClient()
			if state == clientcore.StatePendingApproval {
				g.wizardPendingScreen()
				return
			}
			g.showMain()
		})
	}()
}

func (g *guiApp) wizardPendingScreen() {
	g.wizardCard("Waiting for approval", container.NewVBox(
		widget.NewLabel("This relay requires an admin to approve new\ndevices. Ask a family member with an approved\ndevice to accept this one.\n\nThis screen updates automatically."),
		widget.NewProgressBarInfinite(),
	))
	// onConnState swaps to the main view when the relay sends `ready`.
	_ = time.Now
}
