//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
)

// Swap replaces execPath with tmpPath. Windows won't let you overwrite a
// running executable's file directly (the mapped file is locked), so the
// current binary is renamed aside first — freeing the original path — and
// the new one moved into place.
func Swap(execPath, tmpPath string) error {
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("update: chmod new binary: %w", err)
	}
	old := execPath + ".old"
	_ = os.Remove(old) // leftover from a previous swap, if any
	if err := os.Rename(execPath, old); err != nil {
		return fmt.Errorf("update: rename running binary aside: %w", err)
	}
	if err := os.Rename(tmpPath, execPath); err != nil {
		// Best effort: put the original back rather than leave nothing runnable.
		_ = os.Rename(old, execPath)
		return fmt.Errorf("update: move new binary into place: %w", err)
	}
	return nil
}

// reexec spawns the new binary as a fresh, detached process and exits this
// one — Windows has no exec() to replace the current process image in place.
func reexec(execPath string, args []string, env []string) error {
	cmd := exec.Command(execPath, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("update: relaunch: %w", err)
	}
	os.Exit(0)
	return nil // unreachable
}
