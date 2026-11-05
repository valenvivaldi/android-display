package main

import (
	"context"
	"log"
	"slices"
	"sync"
	"time"
)

// pipelineStats aggregates Mac-side per-frame timings and logs a summary
// every interval:
//
//	latency mac: encode avg 6.1 p95 9.0 ms | send avg 0.3 p95 1.2 ms | 59 fps | 3.9 Mbps
//
// encode = capture → encoded NAL available; send = time spent in SendFrame
// (grows when the transport backs up).
type pipelineStats struct {
	mu      sync.Mutex
	encode  []time.Duration
	send    []time.Duration
	bytes   int
	started time.Time
}

func newPipelineStats() *pipelineStats {
	return &pipelineStats{started: time.Now()}
}

// recordFrame adds one video frame (IDR or P).
func (s *pipelineStats) recordFrame(encode, send time.Duration) {
	s.mu.Lock()
	s.encode = append(s.encode, encode)
	s.send = append(s.send, send)
	s.mu.Unlock()
}

// recordBytes counts every NAL sent, including SPS/PPS.
func (s *pipelineStats) recordBytes(n int) {
	s.mu.Lock()
	s.bytes += n
	s.mu.Unlock()
}

func (s *pipelineStats) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.logAndReset()
		}
	}
}

func (s *pipelineStats) logAndReset() {
	s.mu.Lock()
	encode, send, bytes := s.encode, s.send, s.bytes
	elapsed := time.Since(s.started).Seconds()
	s.encode, s.send, s.bytes, s.started = nil, nil, 0, time.Now()
	s.mu.Unlock()

	if len(encode) == 0 || elapsed <= 0 {
		return
	}
	encAvg, encP95 := avgP95(encode)
	sendAvg, sendP95 := avgP95(send)
	log.Printf("latency mac: encode avg %.1f p95 %.1f ms | send avg %.1f p95 %.1f ms | %.0f fps | %.1f Mbps",
		encAvg, encP95, sendAvg, sendP95, float64(len(encode))/elapsed, float64(bytes)*8/elapsed/1e6)
}

// avgP95 returns the mean and 95th percentile in milliseconds. Sorts d in place.
func avgP95(d []time.Duration) (avg, p95 float64) {
	slices.Sort(d)
	var sum time.Duration
	for _, v := range d {
		sum += v
	}
	ms := func(v time.Duration) float64 { return float64(v) / float64(time.Millisecond) }
	return ms(sum / time.Duration(len(d))), ms(d[(len(d)*95)/100])
}
