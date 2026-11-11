package input

/*
#cgo CFLAGS: -x objective-c -I../../bridge
#cgo LDFLAGS: -framework CoreGraphics -framework ApplicationServices -framework Foundation
#include "input_bridge.h"
*/
import "C"

// cgOps is pointerOps backed by CoreGraphics.
type cgOps struct{}

func (cgOps) hidMove(x, y float64) { C.InjectMouseMove(C.double(x), C.double(y)) }

func (cgOps) hidButton(b button, down bool, x, y float64) {
	switch {
	case b == buttonLeft && down:
		C.InjectLeftMouseDown(C.double(x), C.double(y))
	case b == buttonLeft:
		C.InjectLeftMouseUp(C.double(x), C.double(y))
	case down:
		C.InjectRightMouseDown(C.double(x), C.double(y))
	default:
		C.InjectRightMouseUp(C.double(x), C.double(y))
	}
}

func (cgOps) hidDrag(x, y float64) { C.InjectLeftMouseDragged(C.double(x), C.double(y)) }

func (cgOps) hidScroll(dx, dy int32) { C.InjectScrollWheel(C.int32_t(dx), C.int32_t(dy)) }

func (cgOps) cursor() (x, y float64) {
	var cx, cy C.double
	C.GetCursorPosition(&cx, &cy)
	return float64(cx), float64(cy)
}

func (cgOps) warp(x, y float64) { C.WarpCursor(C.double(x), C.double(y)) }

func (cgOps) windowAt(x, y float64) (target, bool) {
	var pid C.int32_t
	var window C.uint32_t
	if !bool(C.WindowAt(C.double(x), C.double(y), &pid, &window)) {
		return target{}, false
	}
	return target{pid: int32(pid), window: uint32(window)}, true
}

func (cgOps) postMouse(t target, eventType int32, b button, x, y float64, clickState int32) {
	C.PostMouseToPid(C.int32_t(t.pid), C.uint32_t(t.window), C.int32_t(eventType), C.int32_t(b), C.double(x), C.double(y), C.int32_t(clickState))
}

func (cgOps) postScroll(t target, x, y float64, dx, dy int32) {
	C.PostScrollToPid(C.int32_t(t.pid), C.uint32_t(t.window), C.double(x), C.double(y), C.int32_t(dx), C.int32_t(dy))
}

func (cgOps) prime(t target) { C.PrimeWindowForInput(C.int32_t(t.pid), C.uint32_t(t.window)) }
