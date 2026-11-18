package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/update"
)

// guiTarget is the name this program is published under in the signed update
// list (see cmd/lanmsg-signrelease); the list also says which OS and CPU each
// build is for.
const guiTarget = "lanmsg"

// updateCheckEvery limits how often a reconnect triggers a check; the list is a
// few hundred bytes, but there is no reason to ask on every flap of the network.
const updateCheckEvery = 10 * time.Minute

// checkForUpdate asks the relay, in the background, whether a newer build of
// this program has been published and signed. Called after each (re)connect.
// It only offers: nothing is downloaded until the person agrees.
func (g *guiApp) checkForUpdate() { g.runUpdateCheck(false) }

// runUpdateCheck is checkForUpdate; manual = true (the Settings button) skips the
// reconnect throttle, re-offers a version already postponed, and reports every
// outcome (up to date, couldn't check) instead of staying quiet.
func (g *guiApp) runUpdateCheck(manual bool) {
	if g.client == nil {
		if manual {
			dialog.ShowInformation("Check for updates", "Not connected to a relay yet.", g.win)
		}
		return
	}
	if !g.updateChecking.CompareAndSwap(false, true) {
		if manual {
			dialog.ShowInformation("Check for updates", "A check is already running.", g.win)
		}
		return
	}
	if last := g.lastUpdateCheck; !manual && !last.IsZero() && time.Since(last) < updateCheckEvery {
		g.updateChecking.Store(false)
		return
	}
	if manual {
		g.offeredVersion = "" // let the dialog appear again for a version that was postponed
	}
	g.lastUpdateCheck = time.Now()
	f := update.NewFetcher(g.client.ServerAddr(), g.client.TLSConfig(), g.client.SOCKSProxy(), filepath.Join(g.cfg.Dir(), "gui-update"))
	go func() {
		defer g.updateChecking.Store(false)
		ctx, cancel := context.WithTimeout(g.ctx, 3*time.Minute)
		defer cancel()
		// CheckManifest accepts the same list again (so the notice survives
		// reconnects and a just-installed release reads as "up to date") and writes
		// nothing; the sequence mark moves only when an update is installed.
		m, err := f.CheckManifest(ctx)
		if err != nil {
			g.log.Debug("update check", "err", err)
			if manual {
				fyne.Do(func() {
					dialog.ShowInformation("Check for updates", "Could not check for updates: "+err.Error(), g.win)
				})
			}
			return
		}
		art, ok := m.ArtifactFor(guiTarget, runtime.GOOS, runtime.GOARCH)
		if !ok || !update.CheckSelf(art) {
			if manual {
				fyne.Do(func() {
					dialog.ShowInformation("Check for updates", "You have the latest version ("+versionLabel()+").", g.win)
				})
			}
			return
		}
		fyne.Do(func() { g.offerUpdate(f, m.Seq, art) })
	}()
}

// offerUpdate shows the "update available" notice (status line every time,
// a dialog once per version per run).
func (g *guiApp) offerUpdate(f *update.Fetcher, seq int64, art update.Artifact) {
	g.setStatusLine(fmt.Sprintf("Update available: v%s (you have v%s)", art.Version, versionLabel()))
	if g.offeredVersion == art.Version {
		return
	}
	g.offeredVersion = art.Version

	if runtime.GOOS == "darwin" {
		// A Mac app is a signed bundle; swapping the program inside it would break
		// the signature, so the person replaces the whole app by hand.
		dialog.ShowInformation("Update available", fmt.Sprintf(
			"Version %s is available from your relay (you have %s).\n\nOn macOS, replace the app with the new build; it cannot update itself in place.",
			art.Version, versionLabel()), g.win)
		return
	}
	dialog.ShowConfirm("Update available", fmt.Sprintf(
		"Version %s is available from your relay (you have %s).\n\nIt is checked against the household release signature before it replaces this program, and the app restarts to finish.",
		art.Version, versionLabel()), func(ok bool) {
		if ok {
			g.installUpdate(f, seq, art)
		}
	}, g.win)
	// "Later" simply dismisses; the status line keeps the notice and the next
	// reconnect offers again.
}

// installUpdate downloads the build, verifies its SHA-256 against the signed
// list, swaps it over the running program's file and restarts into it.
func (g *guiApp) installUpdate(f *update.Fetcher, seq int64, art update.Artifact) {
	bar := widget.NewProgressBarInfinite()
	body := container.NewVBox(widget.NewLabel(fmt.Sprintf("Downloading version %s…", art.Version)), bar)
	wait := dialog.NewCustomWithoutButtons("Updating", body, g.win)
	wait.Show()
	go func() {
		ctx, cancel := context.WithTimeout(g.ctx, 10*time.Minute)
		defer cancel()
		fail := func(err error) {
			fyne.Do(func() {
				wait.Hide()
				dialog.ShowError(fmt.Errorf("the update was not installed: %w", err), g.win)
			})
		}
		execPath, err := os.Executable()
		if err != nil {
			fail(err)
			return
		}
		tmp, err := f.DownloadAndVerify(ctx, art, filepath.Dir(execPath))
		if err != nil {
			fail(err)
			return
		}
		_ = f.RestoreSeq(seq) // this release is accepted now; an older list cannot replace it
		g.cancel()            // stop the connection loop before the process image is replaced
		if g.client != nil {
			g.client.Stop()
			_ = g.client.Close()
		}
		if err := update.SwapAndReexec(execPath, tmp, os.Args[1:]); err != nil {
			fail(err) // only reached when the swap or restart failed
		}
	}()
}
