package input

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

type rect struct {
	x, y, w, h float64
	t          target
}

// fakeOps records every call and pretends to be macOS: a cursor and a few windows.
type fakeOps struct {
	calls    []string
	cursorAt [2]float64
	windows  []rect // front to back
}

func (f *fakeOps) log(format string, a ...any) { f.calls = append(f.calls, fmt.Sprintf(format, a...)) }

func (f *fakeOps) hidMove(x, y float64) { f.log("hid move %v,%v", x, y) }
func (f *fakeOps) hidButton(b button, down bool, x, y float64) {
	f.log("hid %s %s %v,%v", map[bool]string{true: "down", false: "up"}[down], buttonName(b), x, y)
}
func (f *fakeOps) hidDrag(x, y float64)       { f.log("hid drag %v,%v", x, y) }
func (f *fakeOps) hidScroll(dx, dy int32)     { f.log("hid scroll %d,%d", dx, dy) }
func (f *fakeOps) cursor() (float64, float64) { f.log("cursor?"); return f.cursorAt[0], f.cursorAt[1] }
func (f *fakeOps) warp(x, y float64)          { f.log("warp %v,%v", x, y) }

func (f *fakeOps) windowAt(x, y float64) (target, bool) {
	for _, w := range f.windows {
		if x >= w.x && x < w.x+w.w && y >= w.y && y < w.y+w.h {
			return w.t, true
		}
	}
	return target{}, false
}

func (f *fakeOps) postMouse(t target, eventType int32, b button, x, y float64, clickState int32) {
	f.log("pid%d/w%d event%d %s %v,%v cs%d", t.pid, t.window, eventType, buttonName(b), x, y, clickState)
}
func (f *fakeOps) postScroll(t target, x, y float64, dx, dy int32) {
	f.log("pid%d/w%d scroll %d,%d at %v,%v", t.pid, t.window, dx, dy, x, y)
}
func (f *fakeOps) prime(t target) { f.log("prime pid%d/w%d", t.pid, t.window) }

func buttonName(b button) string {
	if b == buttonRight {
		return "right"
	}
	return "left"
}

var (
	windowA = rect{0, 0, 100, 100, target{pid: 10, window: 1}}
	windowB = rect{200, 0, 100, 100, target{pid: 20, window: 2}}
)

func newTestRouter(mode Mode, windows ...rect) (*pointerRouter, *fakeOps) {
	ops := &fakeOps{cursorAt: [2]float64{900, 900}, windows: windows}
	r := newPointerRouter(mode, ops)
	r.sleep = func(time.Duration) {}
	return r, ops
}

func expect(t *testing.T, ops *fakeOps, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(ops.calls, want) {
		t.Errorf("calls:\n got  %q\n want %q", ops.calls, want)
	}
}

// The default mode is the behaviour before modes existed: everything through the hardware
// path, and the cursor is never saved or put back.
func TestCursorModeUsesHardwarePathOnly(t *testing.T) {
	r, ops := newTestRouter(ModeCursor, windowA)
	r.move(10, 10)
	r.down(buttonLeft, 10, 10)
	r.drag(20, 20)
	r.up(buttonLeft, 20, 20)
	r.scroll(0, 5)
	expect(t, ops,
		"hid move 10,10",
		"hid down left 10,10",
		"hid drag 20,20",
		"hid up left 20,20",
		"hid scroll 0,5",
	)
}

// Restore mode: the cursor jumps for the gesture and goes back where the user had it.
func TestRestoreModePutsCursorBackAfterTheGesture(t *testing.T) {
	r, ops := newTestRouter(ModeRestore, windowA)
	r.move(10, 10) // hover: pointless to jump the cursor only to jump it back
	r.down(buttonLeft, 10, 10)
	r.drag(20, 20)
	r.up(buttonLeft, 20, 20)
	expect(t, ops,
		"cursor?",
		"hid down left 10,10",
		"hid drag 20,20",
		"hid up left 20,20",
		"warp 900,900",
	)
}

// Background mode: a tap on a window reaches only that window's app and never touches the
// hardware cursor (no hid call, no warp, no cursor read).
func TestBackgroundTapGoesOnlyToTheWindowsApp(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA, windowB)
	r.down(buttonLeft, 250, 50)
	r.up(buttonLeft, 250, 50)
	expect(t, ops,
		"prime pid20/w2",
		"pid20/w2 event5 left 250,50 cs0", // primer move: the window's pointer state must be current
		"pid20/w2 event1 left 250,50 cs1",
		"pid20/w2 event2 left 250,50 cs1",
	)
}

// Like a real mouse button held down, a drag and its release stay with the window the
// press started in, even when the finger leaves it.
func TestBackgroundDragStaysWithTheWindowItStartedIn(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA, windowB)
	r.down(buttonLeft, 50, 50)
	r.drag(150, 50) // over no window
	r.drag(250, 50) // over window B
	r.up(buttonLeft, 250, 50)
	expect(t, ops,
		"prime pid10/w1",
		"pid10/w1 event5 left 50,50 cs0",
		"pid10/w1 event1 left 50,50 cs1",
		"pid10/w1 event6 left 150,50 cs1",
		"pid10/w1 event6 left 250,50 cs1",
		"pid10/w1 event2 left 250,50 cs1",
	)
}

// Background mode falls back to the hardware path for what has no window (menu bar, Dock,
// desktop), and then behaves like restore mode.
func TestBackgroundFallsBackToHardwareWhereThereIsNoWindow(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA)
	r.down(buttonLeft, 500, 500)
	r.up(buttonLeft, 500, 500)
	expect(t, ops,
		"cursor?",
		"hid down left 500,500",
		"hid up left 500,500",
		"warp 900,900",
	)
}

// Events posted to a process carry no click count of their own, so the router has to
// provide it for double clicks, but only for clicks close in time and space.
func TestBackgroundDoubleClickCount(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA)
	clock := time.Unix(1000, 0)
	r.now = func() time.Time { return clock }

	click := func(x, y float64) {
		r.down(buttonLeft, x, y)
		r.up(buttonLeft, x, y)
	}
	csOfLastDown := func() string {
		for i := len(ops.calls) - 1; i >= 0; i-- {
			var pid, win, ev int
			var b string
			var cx, cy float64
			var cs int
			if n, _ := fmt.Sscanf(ops.calls[i], "pid%d/w%d event%d %s %f,%f cs%d", &pid, &win, &ev, &b, &cx, &cy, &cs); n == 7 && ev == eventLeftDown {
				return fmt.Sprint(cs)
			}
		}
		return "?"
	}

	click(50, 50)
	if got := csOfLastDown(); got != "1" {
		t.Errorf("first click count %s, want 1", got)
	}
	clock = clock.Add(200 * time.Millisecond)
	click(52, 51)
	if got := csOfLastDown(); got != "2" {
		t.Errorf("second click 200 ms later count %s, want 2 (double click)", got)
	}
	clock = clock.Add(200 * time.Millisecond)
	click(52, 51)
	if got := csOfLastDown(); got != "3" {
		t.Errorf("third click count %s, want 3", got)
	}
	clock = clock.Add(2 * time.Second)
	click(52, 51)
	if got := csOfLastDown(); got != "1" {
		t.Errorf("click after a pause count %s, want 1", got)
	}
	clock = clock.Add(100 * time.Millisecond)
	click(90, 90)
	if got := csOfLastDown(); got != "1" {
		t.Errorf("click far from the last one count %s, want 1", got)
	}
}

func TestBackgroundRightClick(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA)
	r.down(buttonRight, 50, 50)
	r.up(buttonRight, 50, 50)
	expect(t, ops,
		"prime pid10/w1",
		"pid10/w1 event5 right 50,50 cs0",
		"pid10/w1 event3 right 50,50 cs1",
		"pid10/w1 event4 right 50,50 cs1",
	)
}

// Hovering reaches the app under the finger (so hover effects work) and moves nothing.
func TestBackgroundHover(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA)
	r.move(10, 10)
	r.move(500, 500) // no window: nothing at all
	expect(t, ops, "pid10/w1 event5 left 10,10 cs0")
}

// Scroll goes to the window under the finger without touching the cursor; where there is
// no window it scrolls through the hardware path and puts the cursor back.
func TestBackgroundScroll(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA)
	r.move(10, 20)
	ops.calls = nil
	r.scroll(3, -4)
	expect(t, ops, "pid10/w1 scroll 3,-4 at 10,20")

	ops.calls = nil
	r.move(500, 500)
	ops.calls = nil
	r.scroll(0, 7)
	expect(t, ops,
		"cursor?",
		"warp 500,500",
		"hid scroll 0,7",
		"warp 900,900",
	)
}

// A press that never got its release (the phone vanished) must not make the next press
// forget where the cursor really belongs.
func TestRestoreKeepsTheOriginalCursorAcrossAMissedRelease(t *testing.T) {
	r, ops := newTestRouter(ModeRestore)
	r.down(buttonLeft, 10, 10)
	ops.cursorAt = [2]float64{10, 10} // the first press moved the real cursor
	r.down(buttonLeft, 30, 30)
	r.up(buttonLeft, 30, 30)
	last := ops.calls[len(ops.calls)-1]
	if last != "warp 900,900" {
		t.Errorf("cursor restored with %q, want the original position 900,900", last)
	}
}

// Making a window key takes tens of milliseconds, so it is only repeated when the touched
// window changes or enough time has passed that the user may have clicked elsewhere.
func TestBackgroundPrimesWindowOnlyWhenNeeded(t *testing.T) {
	r, ops := newTestRouter(ModeBackground, windowA, windowB)
	clock := time.Unix(1000, 0)
	r.now = func() time.Time { return clock }

	primes := func() (n int) {
		for _, c := range ops.calls {
			if len(c) >= 5 && c[:5] == "prime" {
				n++
			}
		}
		return n
	}
	tap := func(x float64) {
		r.down(buttonLeft, x, 50)
		r.up(buttonLeft, x, 50)
	}

	tap(50)
	tap(50) // same window, moments later
	if got := primes(); got != 1 {
		t.Fatalf("two taps in one window primed %d times, want 1", got)
	}
	tap(250) // another window
	if got := primes(); got != 2 {
		t.Fatalf("tap in a second window: %d primes in total, want 2", got)
	}
	tap(250)
	if got := primes(); got != 2 {
		t.Fatalf("repeat tap in the same window: %d primes in total, want still 2", got)
	}
	clock = clock.Add(primeValidity + time.Second)
	tap(250) // long after: the key window may have changed meanwhile
	if got := primes(); got != 3 {
		t.Fatalf("tap after the validity ran out: %d primes in total, want 3", got)
	}

	// Nothing to prime on the hardware fallback: a real click activates the app itself.
	before := primes()
	tap(500)
	if got := primes(); got != before {
		t.Errorf("a tap on no window primed a window")
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]struct {
		mode Mode
		ok   bool
	}{
		"":           {ModeCursor, true},
		"cursor":     {ModeCursor, true},
		"restore":    {ModeRestore, true},
		"background": {ModeBackground, true},
		"Background": {ModeCursor, false},
		"nonsense":   {ModeCursor, false},
	} {
		mode, ok := ParseMode(in)
		if mode != want.mode || ok != want.ok {
			t.Errorf("ParseMode(%q) = (%q, %v), want (%q, %v)", in, mode, ok, want.mode, want.ok)
		}
	}
}
