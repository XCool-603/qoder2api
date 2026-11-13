//go:build windows
// +build windows

package main

import (
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
)

// runDesktopGUI launches the native Windows Edge WebView2 shell window.
// Falls back cleanly to opening the default system browser if WebView2 runtime is missing.
func runDesktopGUI(uiURL string, stopFunc func()) (handled bool) {
	defer func() {
		if r := recover(); r != nil {
			handled = false
		}
	}()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  "Qoder 2API",
			Width:  1280,
			Height: 820,
			IconId: 2,
			Center: true,
		},
	})
	if w != nil {
		defer w.Destroy()
		w.Navigate(uiURL)
		w.Run()
		if stopFunc != nil {
			stopFunc()
		}
		time.Sleep(200 * time.Millisecond)
		return true
	}
	return false
}

// showNativeError displays a native Windows error dialog in windowsgui builds.
func showNativeError(title, msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	procMessageBoxW := user32.NewProc("MessageBoxW")
	tPtr, _ := syscall.UTF16PtrFromString(title)
	mPtr, _ := syscall.UTF16PtrFromString(msg)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(mPtr)), uintptr(unsafe.Pointer(tPtr)), 0x10)
}
