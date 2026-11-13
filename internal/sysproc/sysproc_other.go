//go:build !windows

package sysproc

import "os/exec"

// HideWindow is a no-op on non-Windows platforms.
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	return cmd
}
