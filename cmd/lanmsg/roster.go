package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/proto"
)

// rosterItem is one roster row. It wraps the row's layout so a right-click
// (secondary tap) can open the per-person context menu; the list still handles
// ordinary clicks for selection.
type rosterItem struct {
	widget.BaseWidget
	content     *fyne.Container
	onTap       func()
	onSecondary func(pos fyne.Position)
}

func newRosterItem(content *fyne.Container) *rosterItem {
	it := &rosterItem{content: content}
	it.ExtendBaseWidget(it)
	return it
}

func (it *rosterItem) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(it.content)
}

// Tapped implements fyne.Tappable. A widget that handles only the right-click
// would swallow the left-click before the list sees it (Fyne gives a click to the
// deepest widget that handles any click), so selection is forwarded here.
func (it *rosterItem) Tapped(*fyne.PointEvent) {
	if it.onTap != nil {
		it.onTap()
	}
}

// TappedSecondary implements fyne.SecondaryTappable.
func (it *rosterItem) TappedSecondary(e *fyne.PointEvent) {
	if it.onSecondary != nil {
		it.onSecondary(e.AbsolutePosition)
	}
}

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
			return newRosterItem(container.NewBorder(
				nil, nil,
				container.NewGridWrap(fyne.NewSize(14, 14), dot),
				nil,
				container.NewVBox(name, sub),
			))
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			if id < 0 || id >= len(r.entries) {
				return
			}
			e := r.entries[id]
			item := o.(*rosterItem)
			item.onTap = func() { r.list.Select(id) }
			item.onSecondary = func(pos fyne.Position) { r.g.showPeerMenu(e, pos) }
			border := item.content
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
			// Admin mode only: how this person is connected / last seen.
			if extra := r.g.adminExtra(e.DeviceID); extra != "" {
				subText += "  ·  " + extra
			}
			revoked := e.State == proto.StateRevoked
			if revoked {
				subText = r.g.adminExtra(e.DeviceID)
				label = e.DisplayName + "  (revoked)"
				name.SetText(label)
				dot.FillColor = presenceColor(proto.StatusOffline, false)
				dot.Refresh()
			}
			// Revoked registrations stay in the admin's list but greyed out.
			if revoked {
				name.Importance, sub.Importance = widget.LowImportance, widget.LowImportance
			} else {
				name.Importance, sub.Importance = widget.MediumImportance, widget.MediumImportance
			}
			name.Refresh()
			sub.SetText(subText)
		},
	)
	r.list.OnSelected = func(id widget.ListItemID) {
		if id >= 0 && id < len(r.entries) {
			if r.entries[id].State == proto.StateRevoked {
				r.list.UnselectAll() // nothing to chat with; use the right-click menu
				return
			}
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
