package input

// Mode says how a phone's touches reach macOS.
type Mode string

const (
	// ModeCursor moves the real cursor to the touch and clicks there. Works with every
	// app, but the touch takes the cursor away from the user.
	ModeCursor Mode = "cursor"

	// ModeRestore does the same, and puts the cursor back where it was when the gesture
	// ends. The cursor still jumps to the phone's display for the duration of a gesture.
	ModeRestore Mode = "restore"

	// ModeBackground delivers each touch only to the app whose window is under it, without
	// touching the cursor. What it cannot target (menu bar, Dock, desktop) falls back to
	// ModeRestore. Some apps ignore events delivered this way (Chrome, games, canvas apps).
	ModeBackground Mode = "background"
)

// ParseMode converts a settings value. An empty string is the default, ModeCursor; ok is
// false for anything that is not a mode, in which case the default is returned too.
func ParseMode(s string) (mode Mode, ok bool) {
	switch Mode(s) {
	case ModeCursor, ModeRestore, ModeBackground:
		return Mode(s), true
	case "":
		return ModeCursor, true
	}
	return ModeCursor, false
}
