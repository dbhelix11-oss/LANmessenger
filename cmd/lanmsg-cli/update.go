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
const selfUpdateTarget = "lanmsg-cli"

// checkForUpdateOneShot best-effort checks for, downloads, verifies, and
// installs a newer build of this binary before a one-shot command
// (enroll/send/status/roster) exits. It never fails or delays the command it
// runs alongside beyond its own bounded timeout — anything short of "found
// and failed to apply" is swallowed silently, since the overwhelming common
// case (no manifest published, no release key configured yet) isn't worth
// surfacing on every invocation.
//
// Deliberately synchronous, not a fire-and-forget goroutine: a one-shot
// process's main() returning kills any goroutines still in flight, so a
// detached check would rarely finish downloading anything. checkForUpdateReexec
// (watch, a long-lived process) is the one place a background goroutine
// actually gets to run to completion.
func checkForUpdateOneShot(cl *clientcore.Client, dir string) {
	applySelfUpdate(cl, dir, false)
}

// checkForUpdateReexec is checkForUpdateOneShot, but re-execs into the new
// binary on success rather than just swapping it in for next time — meant to
// be launched with `go` from a long-lived command (watch) that's fine
// restarting mid-session.
func checkForUpdateReexec(cl *clientcore.Client, dir string) {
	applySelfUpdate(cl, dir, true)
}

func applySelfUpdate(cl *clientcore.Client, dir string, reexecOnSuccess bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
