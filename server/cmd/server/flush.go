package main

import (
	"sync"
	"time"
	"unsafe"
)

const (
	// flushDelay is how long after the last captured frame the flusher re-sends it.
	flushDelay = 30 * time.Millisecond
	// flushRepeats is how many times it re-sends it, one flushDelay apart. One is enough for the
	// decoders seen so far (they hold a single frame); two gave the same latency and more traffic.
	flushRepeats = 1
)

// frameFlusher re-encodes the last captured frame shortly after it, unless a new one
// arrives first.
//
// The screen only produces frames when it changes. Some decoders (Qualcomm's OMX decoder
// on Android 5) keep the newest decoded frame until the next one arrives, so on a quiet
// desktop the last change of a burst stays invisible for as long as nothing else moves
// (measured: up to 24 s). Re-encoding the same picture gives the decoder that next frame:
// it costs a few dozen bytes, since the encoder codes it as "unchanged".
//
// The flusher takes ownership of every pixel buffer given to hold and releases it.
type frameFlusher struct {
	encode  func(buf unsafe.Pointer, timestamp int64) error
	release func(buf unsafe.Pointer)
	now     func() int64 // microseconds, on the clock the frames' timestamps use
	delay   time.Duration
	repeats int

	mu      sync.Mutex
	buf     unsafe.Pointer
	lastTS  int64
	sent    int
	gen     int // bumped by every hold, so a timer armed for an older frame does nothing
	timer   *time.Timer
	stopped bool
}

// hold takes ownership of the newest captured frame and schedules its re-send.
func (f *frameFlusher) hold(buf unsafe.Pointer, timestamp int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		f.release(buf)
		return
	}
	if f.buf != nil {
		f.release(f.buf)
	}
	f.buf, f.lastTS, f.sent = buf, timestamp, 0
	f.gen++
	f.arm()
}

// arm schedules the next re-send for the current frame. Caller holds f.mu.
func (f *frameFlusher) arm() {
	if f.timer != nil {
		f.timer.Stop()
	}
	gen := f.gen
	f.timer = time.AfterFunc(f.delay, func() { f.fire(gen) })
}

func (f *frameFlusher) fire(gen int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || gen != f.gen || f.buf == nil {
		return
	}
	// The encoder needs strictly increasing timestamps.
	ts := max(f.now(), f.lastTS+1)
	f.lastTS = ts
	f.sent++
	// Under the lock: a concurrent hold must not release the buffer while it is being encoded.
	f.encode(f.buf, ts) //nolint:errcheck // a failed re-send only means the next real frame flushes
	if f.sent < f.repeats {
		f.arm()
	}
}

// stop ends the re-sends and releases the held frame. Call it before closing the encoder.
func (f *frameFlusher) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	if f.timer != nil {
		f.timer.Stop()
	}
	if f.buf != nil {
		f.release(f.buf)
		f.buf = nil
	}
}
