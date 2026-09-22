package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
)

//go:embed icon.png
var appIconPNG []byte

var (
	normalTrayIconOnce sync.Once
	normalTrayIconRes  fyne.Resource

	unreadTrayIconOnce sync.Once
	unreadTrayIconRes  fyne.Resource
)

// normalTrayIcon is the plain app icon, used as a fyne.Resource so it can be
// set explicitly via desktop.App.SetSystemTrayIcon (and so there's a known
// baseline to restore once unreadTrayIcon has been shown).
func normalTrayIcon() fyne.Resource {
	normalTrayIconOnce.Do(func() {
		normalTrayIconRes = fyne.NewStaticResource("icon.png", appIconPNG)
	})
	return normalTrayIconRes
}

// unreadTrayIcon is the app icon with a small red dot badged into the
// top-right corner — the conventional placement for an OS unread/notification
// indicator (macOS dock badges, Windows overlay icons, etc.), computed once
// from icon.png rather than shipped as a second static asset so the two can
// never drift out of sync when icon.png is replaced.
func unreadTrayIcon() fyne.Resource {
	unreadTrayIconOnce.Do(func() {
		badged, err := badgeWithDot(appIconPNG)
		if err != nil {
			// Fall back to the plain icon rather than fail — a missing badge
			// is a cosmetic regression, not worth losing the tray icon over.
			unreadTrayIconRes = normalTrayIcon()
			return
		}
		unreadTrayIconRes = fyne.NewStaticResource("icon_unread.png", badged)
	})
	return unreadTrayIconRes
}

// badgeWithDot decodes a PNG, draws a solid red circle (with a thin white
// ring for contrast against dark or busy icon backgrounds) into its top-right
// corner, and re-encodes it as PNG.
func badgeWithDot(src []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	out := image.NewRGBA(b)
	draw.Draw(out, b, img, b.Min, draw.Src)

	w := b.Dx()
	r := w / 7                     // dot radius
	margin := w / 14               // gap from the edges
	cx, cy := w-r-margin, r+margin // top-right corner, in Min-relative coords
	cx += b.Min.X
	cy += b.Min.Y

	fillCircle(out, cx, cy, r+r/3, color.White)                                // contrast ring
	fillCircle(out, cx, cy, r, color.RGBA{R: 0xe0, G: 0x2e, B: 0x2e, A: 0xff}) // dot

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// markUnread badges the tray icon to show an unseen message arrived while the
// window was hidden. A no-op if there's no tray (hasTray false).
func (g *guiApp) markUnread() {
	if !g.hasTray || g.hasUnread.Swap(true) {
		return // already marked, or no tray to badge
	}
	if desk, ok := g.fapp.(desktop.App); ok {
		desk.SetSystemTrayIcon(unreadTrayIcon())
	}
}

// clearUnread restores the plain tray icon. Safe to call even when nothing
// was marked unread.
func (g *guiApp) clearUnread() {
	if !g.hasTray || !g.hasUnread.Swap(false) {
		return
	}
	if desk, ok := g.fapp.(desktop.App); ok {
		desk.SetSystemTrayIcon(normalTrayIcon())
	}
}

func fillCircle(img *image.RGBA, cx, cy, r int, col color.Color) {
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r {
				img.Set(cx+x, cy+y, col)
			}
		}
	}
}
