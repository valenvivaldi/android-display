package main

import (
	"sync"
	"testing"
	"time"
	"unsafe"
)

// fakeEncoder records what the flusher encodes and releases.
type fakeEncoder struct {
	mu       sync.Mutex
	encoded  []encodeCall
	released []unsafe.Pointer
	clock    int64
}

type encodeCall struct {
	buf unsafe.Pointer
	ts  int64
}

func (e *fakeEncoder) flusher(delay time.Duration, repeats int) *frameFlusher {
	return &frameFlusher{
		encode: func(buf unsafe.Pointer, ts int64) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.encoded = append(e.encoded, encodeCall{buf, ts})
			return nil
		},
		release: func(buf unsafe.Pointer) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.released = append(e.released, buf)
		},
		now:     func() int64 { return e.clock }, // frozen: the flusher must still make timestamps increase
		delay:   delay,
		repeats: repeats,
	}
}

func (e *fakeEncoder) snapshot() ([]encodeCall, []unsafe.Pointer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]encodeCall{}, e.encoded...), append([]unsafe.Pointer{}, e.released...)
}

func newBuf() unsafe.Pointer { return unsafe.Pointer(new(int)) }

// A quiet screen: the last frame is re-sent `repeats` times with increasing timestamps.
func TestFlusherResendsLastFrame(t *testing.T) {
	e := &fakeEncoder{clock: 50} // behind the frame's own timestamp
	f := e.flusher(10*time.Millisecond, 2)
	buf := newBuf()
	f.hold(buf, 1000)

	time.Sleep(200 * time.Millisecond)
	encoded, _ := e.snapshot()
	if len(encoded) != 2 {
		t.Fatalf("re-sent %d times, want 2", len(encoded))
	}
	for i, c := range encoded {
		if c.buf != buf {
			t.Errorf("re-send %d used another buffer", i)
		}
	}
	if !(encoded[0].ts > 1000 && encoded[1].ts > encoded[0].ts) {
		t.Errorf("timestamps %d, %d must increase past the original 1000", encoded[0].ts, encoded[1].ts)
	}
}

// A busy screen: new frames keep arriving, so nothing is re-sent, and each frame that is
// replaced is released exactly once.
func TestFlusherSkipsWhenNewFramesKeepArriving(t *testing.T) {
	e := &fakeEncoder{}
	f := e.flusher(80*time.Millisecond, 2)
	bufs := []unsafe.Pointer{newBuf(), newBuf(), newBuf()}
	for i, b := range bufs {
		f.hold(b, int64(i+1)*1000)
		time.Sleep(10 * time.Millisecond) // well inside the delay
	}

	encoded, released := e.snapshot()
	if len(encoded) != 0 {
		t.Fatalf("re-sent %d frames while new ones kept arriving, want 0", len(encoded))
	}
	if len(released) != 2 || released[0] != bufs[0] || released[1] != bufs[1] {
		t.Errorf("released %v, want the two replaced frames in order", released)
	}

	// Once it goes quiet, only the newest frame is re-sent.
	time.Sleep(400 * time.Millisecond)
	encoded, _ = e.snapshot()
	if len(encoded) != 2 || encoded[0].buf != bufs[2] {
		t.Errorf("after going quiet got %d re-sends, want 2 of the newest frame", len(encoded))
	}
}

// Stopping releases the held frame and nothing is encoded afterwards (the encoder is
// closed right after stop).
func TestFlusherStop(t *testing.T) {
	e := &fakeEncoder{}
	f := e.flusher(20*time.Millisecond, 2)
	buf := newBuf()
	f.hold(buf, 1000)
	f.stop()

	time.Sleep(150 * time.Millisecond)
	encoded, released := e.snapshot()
	if len(encoded) != 0 {
		t.Errorf("encoded %d frames after stop", len(encoded))
	}
	if len(released) != 1 || released[0] != buf {
		t.Errorf("released %v, want the held frame once", released)
	}

	late := newBuf()
	f.hold(late, 2000) // a frame arriving after stop is released, not kept
	if _, released := e.snapshot(); len(released) != 2 || released[1] != late {
		t.Errorf("a frame held after stop must be released immediately, got %v", released)
	}
}
