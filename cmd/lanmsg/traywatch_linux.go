//go:build linux || freebsd || openbsd || netbsd

package main

import (
	"os"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
)

// startMinimizeToTray redirects the window manager's "minimize" action to the
// system tray, so minimize behaves like closing to tray.
//
// Fyne has no minimize/iconify hook (only SetCloseIntercept), so we watch the
// window's ICCCM/EWMH state directly over a second X11 connection. When the WM
// iconifies our top-level window we hide it instead, leaving only the tray
// icon; the tray's "Show lanmessenger" item brings it back.
//
// Best-effort: if there's no X display, no tray, or the window can't be found,
// this is a no-op and minimize keeps its default behaviour.
func (g *guiApp) startMinimizeToTray() {
	if !g.hasTray {
		return
	}
	go g.minimizeToTrayLoop()
}

func (g *guiApp) minimizeToTrayLoop() {
	c, err := xgb.NewConn()
	if err != nil {
		g.log.Debug("minimize-to-tray: no X connection", "err", err)
		return
	}
	// xgb panics on a second Close of the same connection, and both the
	// deferred close below and the shutdown goroutine can reach it.
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(c.Close) }
	defer closeConn()

	// Close the connection when the app shuts down; that unblocks WaitForEvent.
	go func() {
		<-g.ctx.Done()
		closeConn()
	}()

	root := xproto.Setup(c).DefaultScreen(c).Root

	atomWMState := internAtom(c, "WM_STATE")
	atomNetWMState := internAtom(c, "_NET_WM_STATE")
	atomNetWMStateHidden := internAtom(c, "_NET_WM_STATE_HIDDEN")
	atomNetClientList := internAtom(c, "_NET_CLIENT_LIST")
	atomNetWMPID := internAtom(c, "_NET_WM_PID")
	if atomWMState == 0 {
		g.log.Debug("minimize-to-tray: WM_STATE atom unavailable")
		return
	}

	win, ok := g.findOwnWindow(c, root, atomWMState, atomNetClientList, atomNetWMPID)
	if !ok {
		g.log.Debug("minimize-to-tray: could not locate own window; minimize unchanged")
		return
	}

	if err := xproto.ChangeWindowAttributesChecked(c, win, xproto.CwEventMask,
		[]uint32{uint32(xproto.EventMaskPropertyChange | xproto.EventMaskFocusChange)}).Check(); err != nil {
		g.log.Debug("minimize-to-tray: cannot select PropertyChange/FocusChange", "err", err)
		return
	}

	for {
		ev, err := c.WaitForEvent()
		if err != nil {
			g.log.Debug("minimize-to-tray: X event error", "err", err)
			return
		}
		if ev == nil { // connection closed (shutdown)
			return
		}
		if g.ctx.Err() != nil {
			// Shutting down: Fyne's own main loop may already have torn
			// itself down by the time a queued-up event reaches here, and
			// fyne.Do on a dead main loop panics ("send on closed channel").
			// Nothing to repaint on our way out anyway.
			return
		}

		// A taskbar/panel click restoring the window (as opposed to our own
		// tray "Show") activates it via a _NET_ACTIVE_WINDOW client message
		// sent straight to the X server — the window manager remaps our
		// window directly, with no call into Fyne's own Show() at all. That
		// makes the OS-level window visible again (so its frame paints) but
		// leaves Fyne's *internal* visibility bookkeeping still saying
		// "hidden" from our own win.Hide() call when it was minimized, so
		// Fyne's render loop never resumes drawing into it — exactly the
		// blank/transparent client area this whole file exists to prevent.
		// A plain repaint nudge (Content().Refresh(), even a resize) cannot
		// fix this, confirmed by testing: the problem isn't "needs a
		// repaint," it's "Fyne doesn't know it's visible." The only real
		// fix is calling Show() ourselves to bring Fyne's state back in
		// sync, exactly as if the user had used our own tray menu.
		//
		// FocusIn is the one signal common to every restore path (taskbar
		// click, alt-tab, title-bar click, our own RequestFocus) regardless
		// of which WM_STATE/_NET_WM_STATE transition (if any) a given
		// panel's restore happens to touch, so it's the primary trigger
		// here rather than PropertyNotify on WM_STATE.
		if fi, isFocus := ev.(xproto.FocusInEvent); isFocus && fi.Event == win {
			restoreIfHidden(g)
			continue
		}

		pn, isProp := ev.(xproto.PropertyNotifyEvent)
		if !isProp || pn.Window != win {
			continue
		}

		minimized := false
		switch pn.Atom {
		case atomWMState:
			minimized = iconicState(c, win, atomWMState)
		case atomNetWMState:
			minimized = hasAtom(c, win, atomNetWMState, atomNetWMStateHidden)
		default:
			continue
		}
		if minimized && !g.trayHidden.Load() {
			g.trayHidden.Store(true)
			fyne.Do(g.win.Hide)
			continue
		}
		if !minimized {
			restoreIfHidden(g)
		}
	}
}

// restoreIfHidden brings Fyne's own visibility state back in sync with the
// window actually being on screen again. If our own win.Hide() (minimize-
// to-tray) is why trayHidden is true, only Fyne's own Show() resumes its
// render loop — see the FocusIn case above for why a mere repaint doesn't.
// If trayHidden was already false, this was a plain iconify our redirect
// never touched (or Fyne was already showing it), so there's nothing to
// bring back in sync; just nudge a repaint in case the GL surface still
// needs one for some other reason.
func restoreIfHidden(g *guiApp) {
	if g.trayHidden.Swap(false) {
		fyne.Do(func() {
			g.win.Show()
			g.win.RequestFocus()
		})
		g.clearUnread()
		return
	}
	fyne.Do(func() {
		if c := g.win.Content(); c != nil {
			c.Refresh()
		}
	})
}

// findOwnWindow locates this process's managed top-level window. It prefers the
// EWMH _NET_CLIENT_LIST and falls back to a depth-limited walk for anything
// carrying WM_STATE, matching on _NET_WM_PID.
func (g *guiApp) findOwnWindow(c *xgb.Conn, root xproto.Window, atomWMState, atomNetClientList, atomNetWMPID xproto.Atom) (xproto.Window, bool) {
	pid := uint32(os.Getpid())
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		if g.ctx.Err() != nil {
			return 0, false
		}

		var candidates []xproto.Window
		if atomNetClientList != 0 {
			candidates = card32Windows(c, root, atomNetClientList)
		}
		if len(candidates) == 0 {
			candidates = managedWindows(c, root, atomWMState, 3)
		}

		for _, w := range candidates {
			if atomNetWMPID != 0 && card32(c, w, atomNetWMPID) == pid {
				return w, true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return 0, false
}

// --- small X helpers -------------------------------------------------------

func internAtom(c *xgb.Conn, name string) xproto.Atom {
	r, err := xproto.InternAtom(c, true, uint16(len(name)), name).Reply()
	if err != nil || r == nil {
		return 0
	}
	return r.Atom
}

func getProp(c *xgb.Conn, w xproto.Window, a xproto.Atom) *xproto.GetPropertyReply {
	if a == 0 {
		return nil
	}
	r, err := xproto.GetProperty(c, false, w, a, xproto.GetPropertyTypeAny, 0, 1024).Reply()
	if err != nil {
		return nil
	}
	return r
}

// card32 reads a single CARD32 property (0 if absent).
func card32(c *xgb.Conn, w xproto.Window, a xproto.Atom) uint32 {
	r := getProp(c, w, a)
	if r == nil || r.Format != 32 || len(r.Value) < 4 {
		return 0
	}
	return xgb.Get32(r.Value)
}

// card32Windows reads a CARD32 array property as window IDs.
func card32Windows(c *xgb.Conn, w xproto.Window, a xproto.Atom) []xproto.Window {
	r := getProp(c, w, a)
	if r == nil || r.Format != 32 {
		return nil
	}
	out := make([]xproto.Window, 0, len(r.Value)/4)
	for i := 0; i+4 <= len(r.Value); i += 4 {
		out = append(out, xproto.Window(xgb.Get32(r.Value[i:])))
	}
	return out
}

// iconicState reports whether WM_STATE's state field is IconicState (3).
func iconicState(c *xgb.Conn, w xproto.Window, atomWMState xproto.Atom) bool {
	r := getProp(c, w, atomWMState)
	if r == nil || r.Format != 32 || len(r.Value) < 4 {
		return false
	}
	const iconic = 3
	return xgb.Get32(r.Value) == iconic
}

// hasAtom reports whether an ATOM-array property contains want.
func hasAtom(c *xgb.Conn, w xproto.Window, prop, want xproto.Atom) bool {
	if want == 0 {
		return false
	}
	r := getProp(c, w, prop)
	if r == nil || r.Format != 32 {
		return false
	}
	for i := 0; i+4 <= len(r.Value); i += 4 {
		if xproto.Atom(xgb.Get32(r.Value[i:])) == want {
			return true
		}
	}
	return false
}

// managedWindows walks the window tree (depth-limited) collecting anything that
// carries a WM_STATE property, i.e. windows the WM is managing.
func managedWindows(c *xgb.Conn, root xproto.Window, atomWMState xproto.Atom, depth int) []xproto.Window {
	var out []xproto.Window
	var walk func(w xproto.Window, d int)
	walk = func(w xproto.Window, d int) {
		if d < 0 {
			return
		}
		tree, err := xproto.QueryTree(c, w).Reply()
		if err != nil || tree == nil {
			return
		}
		for _, child := range tree.Children {
			if r := getProp(c, child, atomWMState); r != nil && r.Format == 32 {
				out = append(out, child)
			}
			walk(child, d-1)
		}
	}
	walk(root, depth)
	return out
}
