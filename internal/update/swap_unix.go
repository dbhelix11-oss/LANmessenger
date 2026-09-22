//go:build !windows

package update

import (
	"fmt"
	"os"
	"syscall"
)

// Swap atomically replaces execPath with tmpPath via rename(2). tmpPath must
// be on the same filesystem as execPath (DownloadAndVerify's destDir
// argument should be execPath's own directory) — a same-filesystem rename is
// atomic, so any process still holding the old file open (including this one,
// mid-syscall) keeps working against the old inode until it actually exits.
func Swap(execPath, tmpPath string) error {
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("update: chmod new binary: %w", err)
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		return fmt.Errorf("update: replace binary: %w", err)
	}
	return nil
}

// reexec replaces the current process image with execPath, in place — no
// new PID, no parent/child relationship to manage.
func reexec(execPath string, args []string, env []string) error {
	return syscall.Exec(execPath, append([]string{execPath}, args...), env)
}
