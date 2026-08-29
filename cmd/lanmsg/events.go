package main

import (
	"fmt"

	"fyne.io/fyne/v2"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/proto"
)

// pumpEvents forwards clientcore events onto the Fyne UI goroutine.
func (g *guiApp) pumpEvents() {
	for {
		select {
		case <-g.ctx.Done():
			return
		case ev, ok := <-g.client.Events():
			if !ok {
				return
			}
			fyne.Do(func() { g.handleEvent(ev) })
		}
	}
}

func (g *guiApp) handleEvent(ev clientcore.Event) {
	switch ev.Kind {
	case clientcore.EventConnState:
		g.onConnState(ev.State)

	case clientcore.EventRosterChanged:
		g.refreshRoster()

	case clientcore.EventPresence:
		g.refreshRoster()
		if g.convo != nil && ev.PeerID == g.convo.peerID {
			g.convo.refreshHeader()
		}

	case clientcore.EventMessage:
		g.onMessage(ev)

	case clientcore.EventMessageState:
		if g.convo != nil && ev.PeerID == g.convo.peerID {
			g.convo.markState(ev.MsgID, ev.MsgState)
		}

	case clientcore.EventKeyChanged:
		g.onKeyChanged(ev.PeerID)

	case clientcore.EventFileProgress:
		if g.convo != nil && ev.Progress != nil && ev.PeerID == g.convo.peerID {
			g.convo.showProgress(ev.Progress)
		}
		if ev.Progress != nil && ev.Progress.Complete && ev.Progress.Direction == clientcore.DirIn {
			g.notify("File received", fmt.Sprintf("%s saved to %s", ev.Progress.Name, ev.Progress.Path))
		}

	case clientcore.EventPendingChanged:
		if g.adminReload != nil {
			g.adminReload()
		}

	case clientcore.EventError:
		if ev.Err != nil {
			g.setStatusLine(ev.Err.Error())
			g.log.Warn("client event error", "err", ev.Err)
		}
	}
}

func (g *guiApp) onConnState(s clientcore.ConnState) {
	if g.connLabel != nil {
		switch s {
		case clientcore.StateReady:
			g.connLabel.SetText("Connected")
		case clientcore.StateConnecting:
			g.connLabel.SetText("Connecting…")
		case clientcore.StatePendingApproval:
			g.connLabel.SetText("Waiting for admin approval…")
		default:
			g.connLabel.SetText("Offline")
		}
	}
	if s == clientcore.StateReady {
		// We may have been on the "pending approval" screen.
		if g.roster == nil {
			g.showMain()
		}
		// Admin status is known only after the handshake completes.
		if g.adminBtn != nil {
			wantHidden := !g.client.IsAdmin()
			if g.adminBtn.Hidden != wantHidden {
				g.adminBtn.Hidden = wantHidden
				g.adminBtn.Refresh()
			}
		}
		g.refreshRoster()
	}
	g.refreshTray()
}

func (g *guiApp) onMessage(ev clientcore.Event) {
	if ev.Message == nil {
		return
	}
	g.refreshRoster() // last-message ordering could change

	if g.convo != nil && ev.PeerID == g.convo.peerID {
		g.convo.appendMessage(*ev.Message)
	}

	if ev.Message.Direction == clientcore.DirIn {
		name := g.peerName(ev.PeerID)
		if g.shouldNotify(ev.PeerID) {
			body := ev.Message.Body
			if ev.Message.Kind == proto.InnerFileOffer {
				if meta, err := clientcore.DecodeFileMeta(ev.Message.Body); err == nil {
					body = "📎 " + meta.Name
				}
			}
			g.notify(name, body)
		}
	}
}

// shouldNotify suppresses notifications under Do-Not-Disturb or when the toggle
// is off.
func (g *guiApp) shouldNotify(_ string) bool {
	if g.client.DesiredStatus().Status == proto.StatusDND {
		return false
	}
	return g.fapp.Preferences().BoolWithFallback(prefNotify, true)
}

func (g *guiApp) notify(title, body string) {
	g.fapp.SendNotification(fyne.NewNotification(title, body))
}
