package main

import (
	"context"
	"fmt"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/proto"
)

// conversationView is the right-hand chat pane for one peer.
type conversationView struct {
	g *guiApp

	peerID string
	peer   clientcore.RosterEntry

	header  *widget.Label
	note    *widget.Label
	prog    *widget.Label
	msgBox  *fyne.Container
	scroll  *container.Scroll
	rows    map[string]*widget.Label
	input   *widget.Entry
	sendBtn *widget.Button
	attach  *widget.Button
	root    *fyne.Container
}

func newConversationView(g *guiApp) *conversationView {
	c := &conversationView{g: g, rows: map[string]*widget.Label{}}

	c.header = widget.NewLabelWithStyle("Select someone to start chatting", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	c.note = widget.NewLabel("")
	c.prog = widget.NewLabel("")

	c.msgBox = container.NewVBox()
	c.scroll = container.NewVScroll(c.msgBox)

	c.input = widget.NewMultiLineEntry()
	c.input.SetPlaceHolder("Write a message…")
	c.input.Wrapping = fyne.TextWrapWord
	c.input.Disable()

	c.sendBtn = widget.NewButtonWithIcon("Send", theme.MailComposeIcon(), c.send)
	c.sendBtn.Disable()
	c.attach = widget.NewButtonWithIcon("", theme.FolderOpenIcon(), c.attachFile)
	c.attach.Disable()

	inputBar := container.NewBorder(nil, nil, c.attach, c.sendBtn, c.input)

	head := container.NewVBox(c.header, c.note, c.prog, widget.NewSeparator())
	c.root = container.NewBorder(head, inputBar, nil, nil, c.scroll)
	return c
}

func (c *conversationView) object() fyne.CanvasObject { return c.root }

func (c *conversationView) setPeer(e clientcore.RosterEntry, history []clientcore.Message) {
	c.peerID = e.DeviceID
	c.peer = e
	c.rows = map[string]*widget.Label{}
	c.msgBox.RemoveAll()
	for _, m := range history {
		c.msgBox.Add(c.makeRow(m))
	}
	c.msgBox.Refresh()
	c.refreshHeader()
	c.note.SetText("")
	c.prog.SetText("")
	c.input.Enable()
	c.sendBtn.Enable()
	c.attach.Enable()
	c.scroll.ScrollToBottom()
}

func (c *conversationView) refreshHeader() {
	if c.peerID == "" {
		return
	}
	// Pull the freshest presence for this peer.
	entries, _ := c.g.client.Roster()
	for _, e := range entries {
		if e.DeviceID == c.peerID {
			c.peer = e
		}
	}
	title := c.peer.DisplayName + " — " + statusLabel(c.peer.Status)
	fp := c.g.client.PeerFingerprint(c.peerID)
	if c.peer.Verified {
		title += "   ✓ verified"
	} else {
		title += "   (unverified: " + fp + ")"
	}
	c.header.SetText(title)
}

func (c *conversationView) setNote(s string) { c.note.SetText(s) }

func (c *conversationView) showProgress(p *clientcore.FileProgress) {
	if p.Complete {
		if p.Direction == clientcore.DirIn {
			c.prog.SetText(fmt.Sprintf("Received %s", p.Name))
		} else {
			c.prog.SetText(fmt.Sprintf("Sent %s", p.Name))
		}
		return
	}
	pct := 0
	if p.Total > 0 {
		pct = int(p.Done * 100 / p.Total)
	}
	verb := "Sending"
	if p.Direction == clientcore.DirIn {
		verb = "Receiving"
	}
	c.prog.SetText(fmt.Sprintf("%s %s… %d%%", verb, p.Name, pct))
}

func (c *conversationView) appendMessage(m clientcore.Message) {
	c.msgBox.Add(c.makeRow(m))
	c.msgBox.Refresh()
	c.scroll.ScrollToBottom()
}

func (c *conversationView) markState(msgID string, st clientcore.MessageState) {
	if lbl, ok := c.rows[msgID]; ok {
		lbl.SetText(appendMark(lbl.Text, st))
	}
}

func (c *conversationView) makeRow(m clientcore.Message) fyne.CanvasObject {
	who := c.peer.DisplayName
	if m.Direction == clientcore.DirOut {
		who = "You"
	}
	ts := time.UnixMilli(m.TS).Format("Mon 15:04")

	var body string
	switch m.Kind {
	case proto.InnerFileOffer:
		if meta, err := clientcore.DecodeFileMeta(m.Body); err == nil {
			body = fmt.Sprintf("📎 %s  (%s, %s)", meta.Name, humanBytes(meta.Size), meta.Status)
		} else {
			body = "📎 file"
		}
	default:
		body = m.Body
	}

	text := fmt.Sprintf("%s · %s\n%s", who, ts, body)
	if m.Direction == clientcore.DirOut {
		text = appendMark(text, m.State)
	}

	lbl := widget.NewLabel(text)
	lbl.Wrapping = fyne.TextWrapWord
	c.rows[m.MsgID] = lbl
	return lbl
}

func (c *conversationView) send() {
	text := c.input.Text
	if text == "" || c.peerID == "" {
		return
	}
	c.input.SetText("")
	ctx, cancel := context.WithTimeout(c.g.ctx, 10*time.Second)
	defer cancel()
	if _, err := c.g.client.SendText(ctx, c.peerID, text); err != nil {
		dialog.ShowError(err, c.g.win)
	}
}

func (c *conversationView) attachFile() {
	if c.peerID == "" {
		return
	}
	d := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil || rc == nil {
			return
		}
		path := rc.URI().Path()
		_ = rc.Close()
		go func() {
			ctx, cancel := context.WithTimeout(c.g.ctx, 5*time.Minute)
			defer cancel()
			if _, err := c.g.client.SendFile(ctx, c.peerID, path); err != nil {
				fyne.Do(func() { dialog.ShowError(err, c.g.win) })
			}
		}()
	}, c.g.win)
	d.SetFilter(storage.NewExtensionFileFilter(nil)) // all files
	d.Show()
}

// appendMark replaces or adds a delivery mark on the last line of a message.
func appendMark(text string, st clientcore.MessageState) string {
	base := trimMarks(text)
	switch st {
	case clientcore.StateDelivered:
		return base + "  ✓"
	case clientcore.StateSent:
		return base + "  ·"
	case clientcore.StateQueued:
		return base + "  …(queued)"
	case clientcore.StateFailed:
		return base + "  ✗ failed"
	default:
		return base
	}
}

func trimMarks(text string) string {
	for _, suffix := range []string{"  ✓", "  ·", "  …(queued)", "  ✗ failed"} {
		if len(text) >= len(suffix) && text[len(text)-len(suffix):] == suffix {
			return text[:len(text)-len(suffix)]
		}
	}
	return text
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
