package ui

import "charm.land/bubbles/v2/viewport"

// navigateReadOnlyViewport centralizes keyboard scrolling for modal documents.
func navigateReadOnlyViewport(vp *viewport.Model, key string) bool {
	switch key {
	case "j", "down":
		vp.ScrollDown(1)
	case "k", "up":
		vp.ScrollUp(1)
	case "pgdown":
		vp.PageDown()
	case "pgup":
		vp.PageUp()
	case "d":
		vp.HalfPageDown()
	case "u":
		vp.HalfPageUp()
	case "g":
		vp.GotoTop()
	case "G":
		vp.GotoBottom()
	default:
		return false
	}
	return true
}
