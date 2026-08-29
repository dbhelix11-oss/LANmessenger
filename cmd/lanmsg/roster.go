package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/proto"
)

// rosterView is the left-hand list of people.
type rosterView struct {
	g       *guiApp
	entries []clientcore.RosterEntry
	list    *widget.List
}

func newRosterView(g *guiApp) *rosterView {
	r := &rosterView{g: g}
	r.list = widget.NewList(
		func() int { return len(r.entries) },
		func() fyne.CanvasObject {
			dot := canvas.NewCircle(presenceColor(proto.StatusOffline, false))
			name := widget.NewLabel("name")
			name.TextStyle = fyne.TextStyle{Bold: true}
			sub := widget.NewLabel("status")
			return container.NewBorder(
				nil, nil,
				container.NewGridWrap(fyne.NewSize(14, 14), dot),
				nil,
				container.NewVBox(name, sub),
			)
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			if id < 0 || id >= len(r.entries) {
				return
			}
			e := r.entries[id]
			border := o.(*fyne.Container)
			dotWrap := border.Objects[1].(*fyne.Container)
			dot := dotWrap.Objects[0].(*canvas.Circle)
			box := border.Objects[0].(*fyne.Container)
			name := box.Objects[0].(*widget.Label)
			sub := box.Objects[1].(*widget.Label)

			dot.FillColor = presenceColor(e.Status, e.Online)
			dot.Refresh()

			label := e.DisplayName
			if e.Admin {
				label += "  (admin)"
			}
			if e.KeyChanged {
				label += "  ⚠ key changed"
			} else if !e.Verified {
				label += "  • unverified"
			}
			name.SetText(label)

			subText := statusLabel(e.Status)
			if e.StatusMessage != "" {
				subText += " — " + e.StatusMessage
			}
			if e.State == proto.StatePending {
				subText = "awaiting approval"
			}
			sub.SetText(subText)
		},
	)
	r.list.OnSelected = func(id widget.ListItemID) {
		if id >= 0 && id < len(r.entries) {
			r.g.selectPeer(r.entries[id])
		}
	}
	return r
}

func (r *rosterView) object() fyne.CanvasObject { return r.list }

func (r *rosterView) setEntries(entries []clientcore.RosterEntry) {
	r.entries = entries
	r.list.Refresh()
}
