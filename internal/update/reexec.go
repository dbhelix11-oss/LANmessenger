package update

import "os"

// SwapAndReexec swaps in the downloaded binary (see Swap) and relaunches it
// with the same arguments and environment. For long-lived processes (e.g.
// lanmsg-cli/lanmsg-remote-cli's watch command) that are meant to keep
// running anyway. One-shot commands should call Swap alone and let the next
// invocation naturally pick up the new binary — there's no live session
// worth re-execing into.
func SwapAndReexec(execPath, tmpPath string, args []string) error {
	if err := Swap(execPath, tmpPath); err != nil {
		return err
	}
	return reexec(execPath, args, os.Environ())
}
