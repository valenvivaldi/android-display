package input

import (
	"math"
	"sync"
	"time"
)

type button int

const (
	buttonLeft button = iota
	buttonRight
)

// CGEventType values for the events posted straight to a process.
const (
	eventLeftDown    = 1
	eventLeftUp      = 2
	eventRightDown   = 3
	eventRightUp     = 4
	eventMouseMoved  = 5
	eventLeftDragged = 6
)

// target is the window, and the process that owns it, a touch is delivered to.
type target struct {
	pid    int32
	window uint32
}

// pointerOps is everything the router needs from macOS. The real implementation calls
// CoreGraphics; tests use a fake that records what was asked.
type pointerOps interface {
	// Hardware path: these move the cursor.
	hidMove(x, y float64)
	hidButton(b button, down bool, x, y float64)
	hidDrag(x, y float64)
	hidScroll(dx, dy int32)

	cursor() (x, y float64)
	warp(x, y float64)

	// Per-process path: these do not touch the cursor.
	windowAt(x, y float64) (target, bool)
	postMouse(t target, eventType int32, b button, x, y float64, clickState int32)
	postScroll(t target, x, y float64, dx, dy int32)

	// prime makes the target window its app's key window without raising it or activating
	// the app. An inactive app ignores mouse events posted to it, so this comes before a click.
	prime(t target)
}

// held is the state of one pressed mouse button, from its down to its up.
type held struct {
	pressed bool
	// toPid: the press went to a window's process, and so do its drags and its release,
	// even when the pointer leaves that window (like a real mouse button held down).
	toPid bool
	t     target
	// clickState is what the app sees as the click count (2 = double click).
	clickState int32
	// saved is the cursor position to put back on release; only for presses sent through
	// the hardware path in a mode that restores the cursor.
	saved  bool
	savedX float64
	savedY float64
}

// pointerRouter decides, per mode, how each pointer event reaches macOS.
type pointerRouter struct {
	mode  Mode
	ops   pointerOps
	now   func() time.Time
	sleep func(time.Duration) // lets the hardware scroll fallback settle before the cursor goes back

	mu           sync.Mutex
	lastX, lastY float64 // where the finger last was, for scrolling
	buttons      [2]held

	// primed is the window last made key, and when: priming is slow (tens of ms) and only
	// needed again if the window changes or the user may have clicked elsewhere since.
	primed   target
	primedAt time.Time

	lastClickAt    time.Time
	lastClickX     float64
	lastClickY     float64
	lastClickCount int32
}

func newPointerRouter(mode Mode, ops pointerOps) *pointerRouter {
	return &pointerRouter{mode: mode, ops: ops, now: time.Now, sleep: time.Sleep}
}

const (
	doubleClickInterval = 500 * time.Millisecond
	doubleClickDistance = 8.0
	scrollSettle        = 10 * time.Millisecond
	primeValidity       = 5 * time.Second
)

func (r *pointerRouter) restoresCursor() bool { return r.mode != ModeCursor }

// move is a pointer move with no button pressed (hover).
func (r *pointerRouter) move(x, y float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastX, r.lastY = x, y
	switch r.mode {
	case ModeCursor:
		r.ops.hidMove(x, y)
	case ModeBackground:
		if t, ok := r.ops.windowAt(x, y); ok {
			r.ops.postMouse(t, eventMouseMoved, buttonLeft, x, y, 0)
		}
		// Nothing to hover over: do not move the cursor just to move it back.
	}
}

func (r *pointerRouter) down(b button, x, y float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastX, r.lastY = x, y
	h := &r.buttons[b]

	if r.mode == ModeBackground {
		if t, ok := r.ops.windowAt(x, y); ok {
			*h = held{pressed: true, toPid: true, t: t, clickState: r.nextClickState(x, y)}
			r.primeWindow(t)
			// AppKit hit-tests a click against the pointer state the window last saw; a window
			// that never got a move has stale state and the click lands "outside" its views.
			r.ops.postMouse(t, eventMouseMoved, b, x, y, 0)
			r.ops.postMouse(t, downEvent(b), b, x, y, h.clickState)
			return
		}
	}

	// Hardware path. Keep the position saved by an earlier press that never got its release.
	saved, sx, sy := h.saved, h.savedX, h.savedY
	if r.restoresCursor() && !saved {
		sx, sy = r.ops.cursor()
		saved = true
	}
	*h = held{pressed: true, saved: saved && r.restoresCursor(), savedX: sx, savedY: sy}
	r.ops.hidButton(b, true, x, y)
}

// primeWindow makes the window its app's key window if it is not known to be already.
func (r *pointerRouter) primeWindow(t target) {
	now := r.now()
	if t == r.primed && now.Sub(r.primedAt) < primeValidity {
		return
	}
	r.ops.prime(t)
	r.primed, r.primedAt = t, now
}

func (r *pointerRouter) drag(x, y float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastX, r.lastY = x, y
	h := &r.buttons[buttonLeft]
	switch {
	case h.pressed && h.toPid:
		r.ops.postMouse(h.t, eventLeftDragged, buttonLeft, x, y, h.clickState)
	case h.pressed || r.mode == ModeCursor:
		r.ops.hidDrag(x, y)
	}
}

func (r *pointerRouter) up(b button, x, y float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastX, r.lastY = x, y
	h := r.buttons[b]
	r.buttons[b] = held{}

	switch {
	case h.pressed && h.toPid:
		r.ops.postMouse(h.t, upEvent(b), b, x, y, h.clickState)
	case h.pressed || r.mode == ModeCursor:
		r.ops.hidButton(b, false, x, y)
		if h.saved {
			r.ops.warp(h.savedX, h.savedY)
		}
	}
}

func (r *pointerRouter) scroll(dx, dy int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode == ModeCursor {
		r.ops.hidScroll(dx, dy)
		return
	}
	if r.mode == ModeBackground {
		if t, ok := r.ops.windowAt(r.lastX, r.lastY); ok {
			r.ops.postScroll(t, r.lastX, r.lastY, dx, dy)
			return
		}
	}
	// Hardware scroll at the finger, then put the cursor back.
	sx, sy := r.ops.cursor()
	r.ops.warp(r.lastX, r.lastY)
	r.ops.hidScroll(dx, dy)
	r.sleep(scrollSettle)
	r.ops.warp(sx, sy)
}

// nextClickState counts consecutive clicks close in time and space, so apps see double
// clicks. The hardware path gets this from the system; events posted to a process do not.
func (r *pointerRouter) nextClickState(x, y float64) int32 {
	now := r.now()
	near := math.Hypot(x-r.lastClickX, y-r.lastClickY) <= doubleClickDistance
	if r.lastClickCount > 0 && near && now.Sub(r.lastClickAt) <= doubleClickInterval {
		r.lastClickCount++
	} else {
		r.lastClickCount = 1
	}
	r.lastClickAt, r.lastClickX, r.lastClickY = now, x, y
	return r.lastClickCount
}

func downEvent(b button) int32 {
	if b == buttonRight {
		return eventRightDown
	}
	return eventLeftDown
}

func upEvent(b button) int32 {
	if b == buttonRight {
		return eventRightUp
	}
	return eventLeftUp
}
