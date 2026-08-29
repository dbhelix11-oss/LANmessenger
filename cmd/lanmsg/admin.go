package main

import (
	"context"
	"fmt"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/proto"
)

// showAdminPanel lists devices awaiting approval with approve/deny actions.
func (g *guiApp) showAdminPanel() {
	var entries []proto.DirectoryEntry
	list := widget.NewList(
		func() int { return len(entries) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				container.NewHBox(widget.NewButton("Approve", nil), widget.NewButton("Deny", nil)),
				widget.NewLabel("device"),
			)
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			if id < 0 || id >= len(entries) {
				return
			}
			e := entries[id]
			row := o.(*fyne.Container)
			row.Objects[0].(*widget.Label).SetText(fmt.Sprintf("%s\n%s…", e.DisplayName, e.DeviceID[:12]))
			btns := row.Objects[1].(*fyne.Container)
			btns.Objects[0].(*widget.Button).OnTapped = func() { g.adminDecide(e, true) }
			btns.Objects[1].(*widget.Button).OnTapped = func() { g.adminDecide(e, false) }
		},
	)

	reload := func() {
		ctx, cancel := context.WithTimeout(g.ctx, 8*time.Second)
		defer cancel()
		got, err := g.client.ListPending(ctx)
		if err != nil {
			g.setStatusLine(err.Error())
			return
		}
		entries = got
		list.Refresh()
	}
	reload()

	content := container.NewBorder(
		widget.NewLabel("Devices waiting to join. Approve only ones you\nexpect — anyone with the passphrase can reach here."),
		widget.NewButton("Refresh", reload),
		nil, nil,
		list,
	)
	d := dialog.NewCustom("Pending devices", "Close", content, g.win)
	d.Resize(fyne.NewSize(520, 420))
	d.Show()

	// Keep the list fresh while open.
	g.adminReload = func() { fyne.Do(reload) }
	d.SetOnClosed(func() { g.adminReload = nil })
}

func (g *guiApp) adminDecide(e proto.DirectoryEntry, approve bool) {
	ctx, cancel := context.WithTimeout(g.ctx, 8*time.Second)
	defer cancel()

	var err error
	if approve {
		err = g.client.Approve(ctx, e.DeviceID)
	} else {
		err = g.client.Deny(ctx, e.DeviceID)
	}
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	verb := "approved"
	if !approve {
		verb = "denied"
	}
	g.setStatusLine(fmt.Sprintf("%s %s", verb, e.DisplayName))
	if g.adminReload != nil {
		g.adminReload()
	}
}
