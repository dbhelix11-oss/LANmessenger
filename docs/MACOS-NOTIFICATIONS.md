# macOS: no notification banner on new messages

## Root cause

Fyne only requests macOS notification permission from a **signed `.app`
bundle**. A bare `go build ./cmd/lanmsg` binary — or an unsigned `.app` — falls
back to `osascript`, which recent macOS versions silently drop: no error, no
banner, nothing. `cmd/lanmsg/notify_other.go` (the macOS/Windows notification
path) is a two-line passthrough to Fyne's own `SendNotification`, so there's
no custom logic here to carry a bug — this is almost always a
packaging/signing/permission issue rather than something to fix in code.

## Checklist

Run through these in order on the affected Mac.

**1. Confirm the `.app` is actually signed:**

```sh
codesign -dv --verbose=4 lanmessenger.app
```

If this errors with "code object is not signed at all," rebuild and sign it:

```sh
cd cmd/lanmsg && fyne package -os darwin
codesign --force --deep --sign - lanmessenger.app   # ad-hoc; a Developer ID cert is better
```

**2. Launch it the right way** — `open lanmessenger.app` (or double-click the
`.app` in Finder), never the bare `lanmsg` binary inside it or a raw
`go build` output. Running the executable directly bypasses the bundle
entirely, so macOS never considers it capable of notifying.

**3. Check System Settings → Notifications → lanmessenger exists at all.**
- Not listed → macOS never saw a permission request from it. Usually means
  step 1 or 2 wasn't right — redo them.
- Listed but toggled off (e.g. the first-message prompt was denied) → turn it
  on there.

**4. Trigger a fresh permission prompt if needed** — delete and reinstall the
`.app`, or reset just this app's permission state so macOS asks again:

```sh
tccutil reset Notification net.lanmessenger.desktop
```

## If all four check out and it's still silent

That points to an actual code bug rather than the known signing gap — worth a
fresh investigation at that point (can't be reproduced/tested from this Linux
development machine, so needs to be diagnosed on macOS directly).

See also: [SETUP.md](SETUP.md)'s "Making a double-clickable bundle" section
for the full packaging walkthrough this checklist assumes.
