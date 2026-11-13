//go:build !windows
// +build !windows

package main

// runDesktopGUI returns false on non-Windows hosts so headless / browser fallback applies.
func runDesktopGUI(uiURL string, stopFunc func()) bool {
	return false
}

func showNativeError(title, msg string) {}
