//go:build windows

package sdk

import (
	"fmt"
	"strings"

	"golang.design/x/hotkey"
)

// hotkeySupported - Windows má reálnu globálnu skratku (RegisterHotKey, bez CGO).
const hotkeySupported = true

type hotkeyHandle struct {
	hk   *hotkey.Hotkey
	stop chan struct{}
}

func (h *hotkeyHandle) Stop() {
	if h == nil || h.hk == nil {
		return
	}
	close(h.stop)
	_ = h.hk.Unregister()
}

// registerGlobalHotkey zaregistruje OS-level skratku z combo stringu
// ("ctrl+shift+r") a spustí goroutine ktorá pri stlačení volá onTrigger.
func registerGlobalHotkey(combo string, onTrigger func()) (*hotkeyHandle, error) {
	mods, key, err := parseCombo(combo)
	if err != nil {
		return nil, err
	}
	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		return nil, fmt.Errorf("hotkey register %q: %w", combo, err)
	}
	h := &hotkeyHandle{hk: hk, stop: make(chan struct{})}
	go func() {
		for {
			select {
			case <-h.stop:
				return
			case <-hk.Keydown():
				onTrigger()
			}
		}
	}()
	return h, nil
}

func parseCombo(combo string) ([]hotkey.Modifier, hotkey.Key, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(combo)), "+")
	var mods []hotkey.Modifier
	var key hotkey.Key
	keySet := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch p {
		case "ctrl", "control":
			mods = append(mods, hotkey.ModCtrl)
		case "shift":
			mods = append(mods, hotkey.ModShift)
		case "alt":
			mods = append(mods, hotkey.ModAlt)
		case "win", "super", "cmd", "meta":
			mods = append(mods, hotkey.ModWin)
		default:
			k, ok := hotkeyKeyMap[p]
			if !ok {
				return nil, 0, fmt.Errorf("unknown key %q in combo %q", p, combo)
			}
			key = k
			keySet = true
		}
	}
	if !keySet {
		return nil, 0, fmt.Errorf("combo %q has no key", combo)
	}
	if len(mods) == 0 {
		return nil, 0, fmt.Errorf("combo %q needs a modifier (ctrl/shift/alt/win)", combo)
	}
	return mods, key, nil
}

var hotkeyKeyMap = map[string]hotkey.Key{
	"a": hotkey.KeyA, "b": hotkey.KeyB, "c": hotkey.KeyC, "d": hotkey.KeyD,
	"e": hotkey.KeyE, "f": hotkey.KeyF, "g": hotkey.KeyG, "h": hotkey.KeyH,
	"i": hotkey.KeyI, "j": hotkey.KeyJ, "k": hotkey.KeyK, "l": hotkey.KeyL,
	"m": hotkey.KeyM, "n": hotkey.KeyN, "o": hotkey.KeyO, "p": hotkey.KeyP,
	"q": hotkey.KeyQ, "r": hotkey.KeyR, "s": hotkey.KeyS, "t": hotkey.KeyT,
	"u": hotkey.KeyU, "v": hotkey.KeyV, "w": hotkey.KeyW, "x": hotkey.KeyX,
	"y": hotkey.KeyY, "z": hotkey.KeyZ,
	"0": hotkey.Key0, "1": hotkey.Key1, "2": hotkey.Key2, "3": hotkey.Key3,
	"4": hotkey.Key4, "5": hotkey.Key5, "6": hotkey.Key6, "7": hotkey.Key7,
	"8": hotkey.Key8, "9": hotkey.Key9,
	"f1": hotkey.KeyF1, "f2": hotkey.KeyF2, "f3": hotkey.KeyF3, "f4": hotkey.KeyF4,
	"f5": hotkey.KeyF5, "f6": hotkey.KeyF6, "f7": hotkey.KeyF7, "f8": hotkey.KeyF8,
	"f9": hotkey.KeyF9, "f10": hotkey.KeyF10, "f11": hotkey.KeyF11, "f12": hotkey.KeyF12,
	"space": hotkey.KeySpace,
}
