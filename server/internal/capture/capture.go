package capture

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.0 -Wno-deprecated-declarations -I../../bridge
#cgo LDFLAGS: -framework CoreGraphics -framework CoreMedia -framework CoreVideo -framework IOSurface -framework Foundation
#include "../../bridge/capture_bridge.h"
#include <mach/mach_time.h>

extern void goFrameCallback(void *pixelBuffer, int width, int height, int64_t timestamp, void *userData);

// Same clock and units as Frame.Timestamp (mach absolute time in µs).
static int64_t MachNowMicros(void) {
	mach_timebase_info_data_t tb;
	mach_timebase_info(&tb);
	return (int64_t)((double)mach_absolute_time() * tb.numer / tb.denom / 1000.0);
}
*/
import "C"
import (
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"
)

// Frame represents a single captured screen frame containing a reference
// to the underlying CVPixelBuffer and its metadata.
type Frame struct {
	PixelBuffer unsafe.Pointer // CVPixelBufferRef — caller must call ReleasePixelBuffer when done
	Width       int
	Height      int
	Timestamp   int64 // capture time in microseconds on the mach clock (see NowMicros)
}

// NowMicros returns the current time on the same clock as Frame.Timestamp,
// so callers can measure how long ago a frame was captured.
func NowMicros() int64 {
	return int64(C.MachNowMicros())
}

// FrameHandler is a callback function invoked for each captured frame.
// The handler must call ReleasePixelBuffer on frame.PixelBuffer when
// processing is complete.
type FrameHandler func(Frame)

var (
	handlerMu sync.Mutex
	handlers  = make(map[uintptr]FrameHandler)
	nextID    uintptr
)

func registerHandler(fn FrameHandler) uintptr {
	handlerMu.Lock()
	defer handlerMu.Unlock()
	nextID++
	handlers[nextID] = fn
	return nextID
}

func unregisterHandler(id uintptr) {
	handlerMu.Lock()
	defer handlerMu.Unlock()
	delete(handlers, id)
}

//export goFrameCallback
func goFrameCallback(pixelBuffer unsafe.Pointer, width C.int, height C.int, timestamp C.int64_t, userData unsafe.Pointer) {
	id := uintptr(userData)
	handlerMu.Lock()
	fn, ok := handlers[id]
	handlerMu.Unlock()
	if ok {
		fn(Frame{
			PixelBuffer: pixelBuffer,
			Width:       int(width),
			Height:      int(height),
			Timestamp:   int64(timestamp),
		})
	}
}

// Capturer manages an active CGDisplayStream capture session.
type Capturer struct {
	stream    unsafe.Pointer
	handlerID uintptr
}

// Start begins capturing the specified display at the given frame rate.
// Each captured frame is delivered to handler asynchronously.
// The caller must call Stop when capture is no longer needed.
//
// Uses CGDisplayStream which takes a CGDirectDisplayID directly, avoiding
// ScreenCaptureKit's display discovery limitations with virtual displays.
// Retries up to 10 times (500ms apart) for the display to become ready.
func Start(displayID uint32, fps int, handler FrameHandler) (*Capturer, error) {
	const maxRetries = 10
	const retryDelay = 500 * time.Millisecond

	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		id := registerHandler(handler)

		result := C.StartCapture(
			C.CGDirectDisplayID(displayID),
			C.int(fps),
			(C.FrameCallback)(C.goFrameCallback),
			unsafe.Pointer(id),
		)

		if result.success != 0 {
			return &Capturer{
				stream:    result.stream,
				handlerID: id,
			}, nil
		}

		unregisterHandler(id)
		lastErr = fmt.Errorf("%s", C.GoString(&result.errorMsg[0]))

		if attempt < maxRetries {
			log.Printf("capture: attempt %d failed (%v), retrying...", attempt, lastErr)
			time.Sleep(retryDelay)
		}
	}

	return nil, fmt.Errorf("start capture: %v (after %d attempts)", lastErr, maxRetries)
}

// Stop terminates the capture session and releases all associated resources.
func (c *Capturer) Stop() {
	if c.stream != nil {
		C.StopCapture(c.stream)
		c.stream = nil
	}
	unregisterHandler(c.handlerID)
}

// ReleasePixelBuffer releases the CVPixelBufferRef obtained from a Frame.
// This must be called after the pixel data has been consumed (e.g., after
// encoding) to avoid memory leaks.
func ReleasePixelBuffer(buf unsafe.Pointer) {
	if buf != nil {
		C.ReleasePixelBuffer(buf)
	}
}
