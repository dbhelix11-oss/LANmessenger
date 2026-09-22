package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"lanmessenger/internal/clientcore"
	"lanmessenger/internal/update"
)

// selfUpdateTarget must match the "target" this binary is published under in
// a release manifest (see cmd/lanmsg-signrelease).
const selfUpdateTarget = "lanmsg-remote-cli"

// checkForUpdateOneShot best-effort checks for, downloads, verifies, and
// installs a newer build of this binary before a one-shot command
// (enroll/send) exits — over the same SOCKS proxy (Tor) as the connection
// that's already open, so this never dials out directly. See
// cmd/lanmsg-cli/update.go for the fuller rationale (identical here); the
// two tools don't share code, matching this repo's existing split.
func checkForUpdateOneShot(cl *clientcore.Client, dir string) {
	applySelfUpdate(cl, dir, false)
}

// checkForUpdateReexec is checkForUpdateOneShot, but re-execs into the new
// binary on success — for watch, the one long-lived command here.
func checkForUpdateReexec(cl *clientcore.Client, dir string) {
	applySelfUpdate(cl, dir, true)
}

func applySelfUpdate(cl *clientcore.Client, dir string, reexecOnSuccess bool) {
	// Generous timeout: this may be running over Tor, which is slower and
	// more variable than a LAN connection (the same reasoning already
	// applied to waitForPeer's retry window in main.go).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := update.NewFetcher(cl.ServerAddr(), cl.TLSConfig(), cl.SOCKSProxy(), dir)
	manifest, err := f.FetchManifest(ctx)
	if err != nil {
		return // no manifest yet, no release key configured, network hiccup — not worth surfacing
	}
	art, ok := manifest.ArtifactFor(selfUpdateTarget, runtime.GOOS, runtime.GOARCH)
	if !ok || !update.CheckSelf(art) {
		return
	}

	execPath, err := os.Executable()
	if err != nil {
		return
	}
	tmpPath, err := f.DownloadAndVerify(ctx, art, filepath.Dir(execPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[update] found v%s but couldn't download/verify it: %v\n", art.Version, err)
		return
	}

	if reexecOnSuccess {
		fmt.Fprintf(os.Stderr, "[update] updating to v%s and restarting…\n", art.Version)
		if err := update.SwapAndReexec(execPath, tmpPath, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "[update] update failed: %v\n", err)
		}
		return // only reached on failure — SwapAndReexec replaces this process on success
	}
	if err := update.Swap(execPath, tmpPath); err != nil {
		fmt.Fprintf(os.Stderr, "[update] found v%s but couldn't install it: %v\n", art.Version, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[update] updated to v%s — takes effect next run\n", art.Version)
}
