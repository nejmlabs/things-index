// Package shortcutasset exposes the signed helper bundled with the Mac worker.
package shortcutasset

import _ "embed"

//go:embed "ThingsIndex Helper.shortcut"
var helperShortcut []byte

// Helper returns an independent copy of the signed ThingsIndex Helper.
func Helper() []byte {
	return append([]byte(nil), helperShortcut...)
}
