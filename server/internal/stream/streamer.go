package stream

// Streamer is the common interface for video transport (UDP or TCP).
// Both UDPStreamer and TCPVideoServer implement this interface,
// allowing the pipeline to be transport-agnostic.
type Streamer interface {
	// SendFrame sends a complete NAL unit with the given frame type.
	// The implementation handles fragmentation (UDP) or framing (TCP).
	// captureMicros is the frame's capture time in Unix microseconds; it goes
	// into the packet header so the receiver can measure end-to-end latency.
	SendFrame(nalUnit []byte, frameType byte, captureMicros uint64) error

	// Close releases the underlying connection.
	Close() error
}
