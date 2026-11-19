package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/crypto"
	"lanmessenger/internal/proto"
)

// adminIdleLock is how long unlocked admin mode survives without use.
const adminIdleLock = 15 * time.Minute

// adminAvailable reports whether this install can enter admin mode: either an
// admin key file exists in ~/.lanmsg-keys/ (or LANMSG_ADMIN_KEY), or this is
// still a legacy device-flag admin on a relay with no admin key.
func (g *guiApp) adminAvailable() bool {
	if g.client != nil && g.client.IsAdmin() {
		return true
	}
	p, err := crypto.DefaultAdminKeyPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// touchAdmin restarts the idle-lock countdown.
func (g *guiApp) touchAdmin() {
	g.adminMu.Lock()
	defer g.adminMu.Unlock()
	if g.adminLockTimer != nil {
		g.adminLockTimer.Stop()
	}
	g.adminLockAt = time.Now().Add(adminIdleLock)
	g.adminLockTimer = time.AfterFunc(adminIdleLock, func() { g.lockAdmin("idle") })
	g.adminWatchOnce.Do(func() { go g.adminWatch() })
}

// lockAdmin forgets the admin key (in memory only; the file is untouched).
func (g *guiApp) lockAdmin(why string) {
	g.adminMu.Lock()
	if g.adminLockTimer != nil {
		g.adminLockTimer.Stop()
		g.adminLockTimer = nil
	}
	g.adminLockAt = time.Time{}
	g.adminInfo = nil
	g.adminMu.Unlock()
	if g.client != nil {
		g.client.SetAdminKey(nil)
	}
	g.log.Info("admin mode locked", "why", why)
	fyne.Do(func() {
		g.paintAdminState()
		g.refreshRoster()
	})
}

// adminRemaining reports how long until the idle lock fires; ok is false while
// admin mode is locked.
func (g *guiApp) adminRemaining() (d time.Duration, ok bool) {
	g.adminMu.Lock()
	defer g.adminMu.Unlock()
	if g.adminLockAt.IsZero() {
		return 0, false
	}
	return time.Until(g.adminLockAt).Round(time.Second), true
}

func fmtCountdown(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// paintAdminState shows locked / unlocked (with the time left) on the Admin
// button and in the open console. Must run on the UI thread.
func (g *guiApp) paintAdminState() {
	d, unlocked := g.adminRemaining()
	if g.adminBtn != nil {
		text, imp := "Admin", widget.MediumImportance
		if unlocked {
			text, imp = "Admin · unlocked "+fmtCountdown(d), widget.HighImportance
		}
		if g.adminBtn.Text != text || g.adminBtn.Importance != imp {
			g.adminBtn.SetText(text)
			g.adminBtn.Importance = imp
			g.adminBtn.Refresh()
		}
	}
	if g.adminCountdown != nil {
		if unlocked {
			g.adminCountdown.SetText("Unlocked — locks in " + fmtCountdown(d) + " (any admin action restarts the clock)")
		} else {
			g.adminCountdown.SetText("Locked")
		}
	}
}

// adminWatch ticks once a second to keep the lock countdown current and, while
// unlocked, refreshes the admin-only roster details every 30 seconds. It never
// restarts the idle clock itself, so watching cannot keep admin mode open.
func (g *guiApp) adminWatch() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-t.C:
		}
		fyne.Do(g.paintAdminState)
		if _, unlocked := g.adminRemaining(); !unlocked || g.client == nil || !g.client.HasAdminKey() {
			continue
		}
		g.adminMu.Lock()
		stale := time.Since(g.adminInfoAt) > 30*time.Second
		if stale {
			g.adminInfoAt = time.Now()
		}
		g.adminMu.Unlock()
		if !stale {
			continue
		}
		ctx, cancel := context.WithTimeout(g.ctx, 8*time.Second)
		reps, err := g.client.AdminDevices(ctx)
		cancel()
		if err != nil {
			continue
		}
		m := make(map[string]proto.DeviceReport, len(reps))
		for _, r := range reps {
			m[r.Entry.DeviceID] = r
		}
		g.adminMu.Lock()
		if !g.adminLockAt.IsZero() { // still unlocked
			g.adminInfo = m
		}
		g.adminMu.Unlock()
		fyne.Do(g.refreshRoster)
	}
}

// adminExtra is the admin-only roster suffix for one person: how they are
// connected now, or when and how they were last seen. Empty when locked.
func (g *guiApp) adminExtra(deviceID string) string {
	g.adminMu.Lock()
	r, ok := g.adminInfo[deviceID]
	g.adminMu.Unlock()
	if !ok {
		return ""
	}
	if r.Entry.State == proto.StateRevoked {
		out := "registration revoked " + ago(r.RevokedAt)
		if r.RevokeAttempts > 0 {
			out += fmt.Sprintf(" · %d return attempt(s), last %s (%s match)", r.RevokeAttempts, ago(r.LastRevokeAttempt), orDash(r.LastRevokeMatch))
		}
		return out
	}
	if len(r.Conns) == 0 {
		return ""
	}
	c := r.Conns[0]
	what := strings.TrimSpace(c.ClientKind + " " + c.ClientOS)
	if what == "" && r.Facts != nil {
		what = r.Facts.OS
	}
	if r.Online {
		return "via " + orDash(c.Path) + " · " + orDash(what)
	}
	seen := c.DisconnectedAt
	if seen == 0 {
		seen = c.ConnectedAt
	}
	return "last seen " + ago(seen) + " via " + orDash(c.Path)
}

// ago renders a unix time as a short "5 min ago" style phrase.
func ago(sec int64) string {
	d := time.Since(time.Unix(sec, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// withAdmin runs fn once admin mode is unlocked, asking for the admin key
// passphrase first when it is not. With no key file at all (legacy relay) it
// just runs fn.
func (g *guiApp) withAdmin(fn func()) {
	if g.client.HasAdminKey() {
		g.touchAdmin()
		fn()
		return
	}
	path, err := crypto.DefaultAdminKeyPath()
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	if _, err := os.Stat(path); err != nil {
		if g.client.IsAdmin() {
			fn() // legacy mode: device-flag admin on a relay without an admin key
			return
		}
		dialog.ShowError(fmt.Errorf("no admin key at %s", path), g.win)
		return
	}
	pass := widget.NewPasswordEntry()
	form := dialog.NewForm("Unlock admin mode", "Unlock", "Cancel",
		[]*widget.FormItem{
			widget.NewFormItem("Admin key passphrase", pass),
			widget.NewFormItem("", widget.NewLabel("Key file: "+path+"\nAdmin mode locks itself after 15 idle minutes.")),
		},
		func(ok bool) {
			if !ok {
				return
			}
			entered := pass.Text
			go func() {
				key, err := crypto.LoadAdminKey(path, entered)
				fyne.Do(func() {
					if err != nil {
						dialog.ShowError(err, g.win)
						return
					}
					g.client.SetAdminKey(key)
					g.touchAdmin()
					fn()
				})
			}()
		}, g.win)
	form.Show()
	g.win.Canvas().Focus(pass)
}

// showAdminPanel opens the admin console (after unlocking if needed).
func (g *guiApp) showAdminPanel() { g.withAdmin(g.openConsole) }

func (g *guiApp) openConsole() {
	pending := g.pendingTab()
	devices := g.devicesTab()
	tabs := container.NewAppTabs(
		container.NewTabItem("Pending", pending),
		container.NewTabItem("Devices & keys", devices),
		container.NewTabItem("Relay settings", g.settingsTab()),
	)
	lock := widget.NewButton("Lock admin mode", func() {
		g.lockAdmin("manual")
		g.setStatusLine("admin mode locked")
	})
	g.adminCountdown = widget.NewLabel("")
	g.paintAdminState()
	d := dialog.NewCustom("Admin console", "Close",
		container.NewBorder(g.adminCountdown, lock, nil, nil, tabs), g.win)
	d.SetOnClosed(func() { g.adminCountdown = nil })
	d.Resize(fyne.NewSize(640, 520))
	d.Show()
}

// pendingTab lists devices awaiting approval with approve/deny actions.
func (g *guiApp) pendingTab() fyne.CanvasObject {
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
		go func() {
			ctx, cancel := context.WithTimeout(g.ctx, 8*time.Second)
			defer cancel()
			got, err := g.client.ListPending(ctx)
			fyne.Do(func() {
				if err != nil {
					g.setStatusLine(err.Error())
					return
				}
				entries = got
				list.Refresh()
			})
		}()
	}
	reload()
	g.adminReload = func() { fyne.Do(reload) }

	return container.NewBorder(
		widget.NewLabel("Devices waiting to join. Approve only ones you expect — anyone with\nthe household passphrase can reach this list."),
		widget.NewButton("Refresh", reload),
		nil, nil,
		list,
	)
}

func (g *guiApp) adminDecide(e proto.DirectoryEntry, approve bool) {
	g.touchAdmin()
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

// --- Devices & keys ------------------------------------------------------

// normFP reduces a typed fingerprint to lowercase hex so "1A2B 3C4D", "1a2b-3c4d"
// and "1a2b3c4d" compare equal.
func normFP(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// devicesTab is the key-checking console: every device with its fingerprint,
// a compare box, and remove / force re-auth / properties.
func (g *guiApp) devicesTab() fyne.CanvasObject {
	var devs []proto.DeviceReport
	sel := -1

	list := widget.NewList(
		func() int { return len(devs) },
		func() fyne.CanvasObject { return widget.NewLabel("device") },
		func(id widget.ListItemID, o fyne.CanvasObject) {
			if id < 0 || id >= len(devs) {
				return
			}
			d := devs[id]
			on := "offline"
			if d.Online {
				on = "online"
			}
			o.(*widget.Label).SetText(fmt.Sprintf("%s   %s   [%s, %s]", d.Entry.DisplayName, d.Fingerprint, d.Entry.State, on))
		},
	)
	list.OnSelected = func(id widget.ListItemID) { sel = id }
	list.OnUnselected = func(widget.ListItemID) { sel = -1 }

	result := widget.NewLabel("")
	result.Wrapping = fyne.TextWrapWord
	cmp := widget.NewEntry()
	cmp.SetPlaceHolder("Type or paste the fingerprint the person reads out…")
	check := func(s string) {
		want := normFP(s)
		if want == "" {
			result.SetText("")
			return
		}
		var hits []string
		for _, d := range devs {
			if normFP(d.Fingerprint) == want {
				hits = append(hits, d.Entry.DisplayName)
			}
		}
		switch {
		case len(want) < 16:
			result.SetText(fmt.Sprintf("%d of 16 hex digits entered — keep typing.", len(want)))
		case len(hits) == 1:
			result.SetText("✔ Matches " + hits[0] + ". The key the relay holds is the one they see.")
		case len(hits) > 1:
			result.SetText("⚠ Matches several devices: " + strings.Join(hits, ", "))
		default:
			result.SetText("✘ No device has that fingerprint. Do not approve; ask them to re-read it.")
		}
	}
	cmp.OnChanged = check

	reload := func() {
		go func() {
			ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
			defer cancel()
			got, err := g.client.AdminDevices(ctx)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, g.win)
					return
				}
				devs = got
				sel = -1
				list.UnselectAll()
				list.Refresh()
				check(cmp.Text)
			})
		}()
	}
	reload()

	picked := func() (proto.DeviceReport, bool) {
		g.touchAdmin()
		if sel < 0 || sel >= len(devs) {
			dialog.ShowInformation("Admin", "Select a device first.", g.win)
			return proto.DeviceReport{}, false
		}
		return devs[sel], true
	}
	confirm := func(title, msg string, act func(ctx context.Context) error, done string) {
		g.adminConfirm(title, msg, act, done, reload)
	}

	props := widget.NewButton("Properties…", func() {
		if d, ok := picked(); ok {
			g.showReport(d)
		}
	})
	reauth := widget.NewButton("Re-authorize…", func() {
		d, ok := picked()
		if !ok {
			return
		}
		confirm("Force re-authorization",
			fmt.Sprintf("Send %s back to \"waiting for approval\"?\n\nThey are disconnected now and cannot use the relay until you approve them again.", d.Entry.DisplayName),
			func(ctx context.Context) error { return g.client.ReauthDevice(ctx, d.Entry.DeviceID) },
			d.Entry.DisplayName+" must be approved again")
	})
	remove := widget.NewButton("Delete…", func() {
		d, ok := picked()
		if !ok {
			return
		}
		g.confirmDelete(d.Entry.DeviceID, d.Entry.DisplayName, reload)
	})
	rename := widget.NewButton("Rename…", func() {
		if d, ok := picked(); ok {
			g.promptRename(d.Entry.DeviceID, d.Entry.DisplayName, reload)
		}
	})
	revoke := widget.NewButton("Revoke…", func() {
		d, ok := picked()
		if !ok {
			return
		}
		g.confirmRevoke(d.Entry.DeviceID, d.Entry.DisplayName, reload)
	})

	help := widget.NewLabel("A fingerprint is a short digest of a device's two public keys. Ask the person to read the one\n" +
		"shown on THEIR device (Verify fingerprint…) and compare it here; a mismatch means the key the relay holds\n" +
		"is not theirs.")
	help.Wrapping = fyne.TextWrapWord

	top := container.NewVBox(help, cmp, result)
	bottom := container.NewHBox(props, rename, revoke, reauth, remove, widget.NewButton("Refresh", reload))
	return container.NewBorder(top, bottom, nil, nil, list)
}

// --- Properties ----------------------------------------------------------

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (g *guiApp) showReport(d proto.DeviceReport) {
	txt := clientcore.FormatDeviceReport(d)
	lbl := widget.NewLabel(txt)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	copyBtn := widget.NewButton("Copy", func() { g.win.Clipboard().SetContent(txt) })
	dd := dialog.NewCustom("Properties — "+d.Entry.DisplayName, "Close",
		container.NewBorder(nil, copyBtn, nil, nil, container.NewScroll(lbl)), g.win)
	dd.Resize(fyne.NewSize(860, 560))
	dd.Show()
}

// showPropertiesFor fetches the relay's report for deviceID and shows it.
func (g *guiApp) showPropertiesFor(deviceID, name string) {
	g.withAdmin(func() {
		go func() {
			ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
			defer cancel()
			devs, err := g.client.AdminDevices(ctx)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, g.win)
					return
				}
				for _, d := range devs {
					if d.Entry.DeviceID == deviceID {
						g.showReport(d)
						return
					}
				}
				dialog.ShowInformation("Properties", name+" is not known to the relay.", g.win)
			})
		}()
	})
}

// adminConfirm asks, runs an admin action off the UI thread, reports the result and
// refreshes the console (after) and the roster.
func (g *guiApp) adminConfirm(title, msg string, act func(ctx context.Context) error, done string, after func()) {
	g.touchAdmin()
	dialog.ShowConfirm(title, msg, func(ok bool) {
		if !ok {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
			defer cancel()
			err := act(ctx)
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, g.win)
					return
				}
				g.setStatusLine(done)
				g.adminMu.Lock()
				g.adminInfoAt = time.Time{} // refresh the roster's admin details on the next tick
				g.adminMu.Unlock()
				if after != nil {
					after()
				}
				g.refreshRoster()
			})
		}()
	}, g.win)
}

// confirmRevoke withdraws a registration but keeps the record (greyed out in the
// roster). Any later attempt with the same credentials is refused and reported.
func (g *guiApp) confirmRevoke(id, name string, after func()) {
	g.withAdmin(func() {
		g.adminConfirm("Revoke registration",
			fmt.Sprintf("Revoke %s's registration?\n\nThey are disconnected now and cannot sign in. Their entry stays in your list, greyed out. "+
				"If anyone tries to register again with the same credentials, it is refused and you are told straight away.\n\n"+
				"To let them back: Re-authorize, then approve. To forget them entirely: Delete.", name),
			func(ctx context.Context) error { return g.client.RevokeDevice(ctx, id) },
			name+" revoked", after)
	})
}

// confirmDelete deregisters a device and removes it from the household lists.
func (g *guiApp) confirmDelete(id, name string, after func()) {
	g.withAdmin(func() {
		g.adminConfirm("Delete from household",
			fmt.Sprintf("Delete %s from the household?\n\nTheir registration is removed and their device is disconnected. "+
				"Nothing is kept in your list; to return they must register again with the household passphrase and be approved.\n\n"+
				"(Use Revoke instead to keep a greyed-out record and be warned if they try to come back.)", name),
			func(ctx context.Context) error { return g.client.RemoveDevice(ctx, id) },
			name+" deleted", after)
	})
}

// confirmReauth sends a device back to "waiting for approval" (also how a revoked
// registration is restored).
func (g *guiApp) confirmReauth(id, name string, after func()) {
	g.withAdmin(func() {
		g.adminConfirm("Re-authorize",
			fmt.Sprintf("Send %s back to \"waiting for approval\"?\n\nThey cannot use the relay until you approve them again (Admin → Pending).", name),
			func(ctx context.Context) error { return g.client.ReauthDevice(ctx, id) },
			name+" must be approved again", after)
	})
}

// onAdminAlert reacts to the relay's warning that a revoked registration tried to
// return. It is shown even while admin mode is locked: it only reports.
func (g *guiApp) onAdminAlert(a *proto.AdminAlert) {
	if a == nil || a.Kind != "revoked_attempt" {
		return
	}
	var msg string
	switch a.Match {
	case "fingerprint":
		msg = fmt.Sprintf("A NEW registration \"%s\" looks like it could be the same machine as %s, whose registration you revoked.\n\n"+
			"Matching: %s.\nPath: %s. It is waiting for your approval like any new device.\n\n"+
			"This is a hint, not proof: every install of the same build shares a TLS shape and client string. Compare fingerprints before approving.",
			orDash(a.NewName), a.Name, describeSignals(a.Signals), orDash(a.Path))
	case "key":
		msg = fmt.Sprintf("%s, whose registration you revoked, tried to register again with the SAME signing key over the %s path. Attempt %d. It was refused.",
			a.Name, orDash(a.Path), a.Count)
	default:
		msg = fmt.Sprintf("%s, whose registration you revoked, tried to sign in again with the same device ID over the %s path. Attempt %d. It was refused.",
			a.Name, orDash(a.Path), a.Count)
	}
	g.notify("Revoked device tried to return", msg)
	g.setStatusLine("⚠ revoked device " + a.Name + " tried to return")
	dialog.ShowInformation("Revoked device tried to return", msg, g.win)
	g.adminMu.Lock()
	g.adminInfoAt = time.Time{}
	g.adminMu.Unlock()
}

// withRevoked adds the revoked registrations (known only to the unlocked admin) to
// the roster, to be drawn greyed out.
func (g *guiApp) withRevoked(entries []clientcore.RosterEntry) []clientcore.RosterEntry {
	g.adminMu.Lock()
	defer g.adminMu.Unlock()
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.DeviceID] = true
	}
	var extra []clientcore.RosterEntry
	for id, r := range g.adminInfo {
		if r.Entry.State != proto.StateRevoked || have[id] {
			continue
		}
		extra = append(extra, clientcore.RosterEntry{
			Peer:        clientcore.Peer{DeviceID: id, DisplayName: r.Entry.DisplayName, State: proto.StateRevoked, SignPub: r.Entry.SignPub, BoxPub: r.Entry.BoxPub},
			Fingerprint: r.Fingerprint,
		})
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].DisplayName < extra[j].DisplayName })
	return append(entries, extra...)
}

// describeSignals turns "tls,client,address" into words.
func describeSignals(sig string) string {
	words := map[string]string{"tls": "same TLS handshake shape", "client": "same program, OS and CPU", "address": "same LAN address"}
	var out []string
	for _, p := range strings.Split(sig, ",") {
		if w, ok := words[p]; ok {
			out = append(out, w)
		}
	}
	return orDash(strings.Join(out, ", "))
}

// settingsTab holds the relay settings an admin may change. Today: whether new
// devices must be approved before they can use the relay. It is saved in the
// relay's own config, so it survives restarts.
func (g *guiApp) settingsTab() fyne.CanvasObject {
	note := widget.NewLabel("Loading…")
	note.Wrapping = fyne.TextWrapWord
	updating := false
	var chk *widget.Check

	describe := func(on bool) string {
		if on {
			return "New devices wait under Pending until you approve them."
		}
		return "⚠ Approval is OFF: anyone who has the household passphrase and can reach the relay joins immediately."
	}
	setChecked := func(on bool) {
		updating = true
		chk.SetChecked(on)
		updating = false
		note.SetText(describe(on))
	}
	chk = widget.NewCheck("Require admin approval for new devices", func(on bool) {
		if updating {
			return
		}
		g.touchAdmin()
		title, msg := "Turn approval on", "New devices will wait in Pending until you approve them. Devices already approved are not affected."
		if !on {
			title, msg = "Turn approval OFF", "Anyone who has the household passphrase and can reach the relay will be let in immediately, with no approval step.\n\nOnly do this briefly, for example while enrolling the family."
		}
		dialog.ShowConfirm(title, msg, func(ok bool) {
			if !ok {
				setChecked(!on)
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
				defer cancel()
				err := g.client.SetRequireApproval(ctx, on)
				fyne.Do(func() {
					if err != nil {
						dialog.ShowError(err, g.win)
						setChecked(!on)
						return
					}
					setChecked(on)
					g.setStatusLine("approval for new devices: " + map[bool]string{true: "required", false: "OFF"}[on])
				})
			}()
		}, g.win)
	})
	chk.Disable()

	go func() {
		ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
		defer cancel()
		ov, err := g.client.AdminOverview(ctx)
		fyne.Do(func() {
			if err != nil {
				note.SetText("Could not read the relay's settings: " + err.Error())
				return
			}
			chk.Enable()
			setChecked(ov.RequireApproval)
		})
	}()
	return container.NewVBox(chk, note)
}

// promptRename asks for a new display name for a person and applies it for everyone.
func (g *guiApp) promptRename(id, current string, after func()) {
	g.withAdmin(func() {
		entry := widget.NewEntry()
		entry.SetText(current)
		note := widget.NewLabel("Everyone sees the new name, including " + current + " on their own device, and old conversations show it too.\n1 to 64 characters.")
		note.Wrapping = fyne.TextWrapWord
		dialog.NewForm("Rename "+current, "Rename", "Cancel",
			[]*widget.FormItem{widget.NewFormItem("New name", entry), widget.NewFormItem("", note)},
			func(ok bool) {
				if !ok {
					return
				}
				name := strings.TrimSpace(entry.Text)
				if name == "" || name == current {
					return
				}
				g.touchAdmin()
				go func() {
					ctx, cancel := context.WithTimeout(g.ctx, 10*time.Second)
					defer cancel()
					err := g.client.RenameDevice(ctx, id, name)
					fyne.Do(func() {
						if err != nil {
							dialog.ShowError(err, g.win)
							return
						}
						g.setStatusLine(current + " is now " + name)
						g.adminMu.Lock()
						g.adminInfoAt = time.Time{}
						g.adminMu.Unlock()
						if after != nil {
							after()
						}
						g.refreshRoster()
					})
				}()
			}, g.win).Show()
		g.win.Canvas().Focus(entry)
	})
}
