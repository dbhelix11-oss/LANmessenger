package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
)

func main() {
	dirFlag := flag.String("config", "", "configuration directory (default: OS config dir + /lanmessenger)")
	startMinimized := flag.Bool("start-minimized", false, "start hidden in the system tray instead of opening the main window (requires a tray icon)")
	flag.Parse()

	dir := *dirFlag
	if dir == "" {
		d, err := clientcore.DefaultDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		dir = d
	}

	cfg, err := clientcore.LoadConfig(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error loading config:", err)
		os.Exit(1)
	}

	fapp := app.NewWithID("net.lanmessenger.desktop")
	ctx, cancel := context.WithCancel(context.Background())

	g := &guiApp{
		fapp:   fapp,
		log:    newLogger(),
		ctx:    ctx,
		cancel: cancel,
		cfg:    cfg,
	}
	g.win = fapp.NewWindow("lanmessenger")
	g.win.Resize(fyne.NewSize(940, 620))
	g.win.SetMaster()

	g.buildTray()
	g.win.SetCloseIntercept(func() {
		if g.hasTray {
			g.trayHidden.Store(true)
			g.win.Hide()
			return
		}
		g.doQuit()
	})
	g.startMinimizeToTray()

	if cfg.Configured() && cfg.Enrolled() {
		g.startExistingClient()
	} else {
		g.showWizard()
	}

	if *startMinimized && g.hasTray {
		g.trayHidden.Store(true)
		g.fapp.Run()
	} else {
		g.win.ShowAndRun()
	}

	g.cancel()
	if g.client != nil {
		g.client.Stop()
		_ = g.client.Close()
	}
}

// startExistingClient builds the client for an already-configured install and
// shows the main view (prompting for the passphrase first if it isn't stored).
func (g *guiApp) startExistingClient() {
	client, err := clientcore.New(g.cfg, g.log)
	if err != nil {
		dialog.ShowError(err, g.win)
		g.showWizard()
		return
	}
	g.client = client

	if !client.HasPassphrase() {
		g.promptPassphrase(func() {
			g.startClient()
			g.showMain()
		})
		return
	}
	g.startClient()
	g.showMain()
}

// startClient starts the connection loop and the event pump exactly once.
func (g *guiApp) startClient() {
	if err := g.client.Start(g.ctx); err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	if !g.pumpStarted {
		g.pumpStarted = true
		go g.pumpEvents()
	}
}

func (g *guiApp) doQuit() {
	g.cancel()
	g.fapp.Quit()
}

// promptPassphrase asks for the household passphrase and stores it, then calls
// onDone.
func (g *guiApp) promptPassphrase(onDone func()) {
	entry := widget.NewPasswordEntry()
	entry.PlaceHolder = "household passphrase"
	form := dialog.NewForm("Household passphrase", "Save", "Quit",
		[]*widget.FormItem{widget.NewFormItem("Passphrase", entry)},
		func(ok bool) {
			if !ok {
				g.doQuit()
				return
			}
			if err := g.client.SetPassphrase(entry.Text); err != nil {
				dialog.ShowError(err, g.win)
				return
			}
			onDone()
		}, g.win)
	form.Resize(fyne.NewSize(420, 160))
	form.Show()
}
