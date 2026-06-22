//go:build !windows

package sdk

import "errors"

// hotkeySupported - mimo Windows zatiaľ stub (reálna impl vyžaduje CGO:
// Cocoa na macOS, X11 na Linuxe), aby ostal CGO-free cross-build.
const hotkeySupported = false

var errHotkeyUnsupported = errors.New("global hotkey not supported on this platform yet")

type hotkeyHandle struct{}

func (h *hotkeyHandle) Stop() {}

func registerGlobalHotkey(combo string, onTrigger func()) (*hotkeyHandle, error) {
	_ = combo
	_ = onTrigger
	return nil, errHotkeyUnsupported
}
