package stream

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// framePool recycles frame buffers to avoid per-frame heap allocations.
// Each buffer holds 4-byte length prefix + 17-byte header + payload.
// The pool stores *[]byte so the slice header (pointer, len, cap) lives
// on the heap once and the backing array is reused across frames.
var framePool = sync.Pool{
	New: func() any {
		// Start with 64 KiB — large enough for most NAL units.
		buf := make([]byte, 64*1024)
		return &buf
	},
}

// TCPVideoServer streams H.264 NAL units over TCP with length-prefix framing.
// Used for USB connections where ADB only supports TCP tunneling.
//
// Wire format per frame:
//
//	[4 bytes: total packet length (big-endian)] [17 bytes: packet header] [payload]
//
// Unlike UDP, TCP does not fragment — each NAL is sent as a single "packet"
// with fragIndex=0, fragTotal=1. The 4-byte length prefix provides message
// framing on the TCP stream.
type TCPVideoServer struct {
	listener net.Listener
	conn     net.Conn
	mu       sync.Mutex
	sequence atomic.Uint32
	done     chan struct{}
}

// NewTCPVideoServer creates a TCP listener for video streaming.
// Pass port=0 to auto-assign an available port.
func NewTCPVideoServer(port int) (*TCPVideoServer, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	return &TCPVideoServer{
		listener: ln,
		done:     make(chan struct{}),
	}, nil
}

// Port returns the actual port the server is listening on.
func (s *TCPVideoServer) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// AcceptOne blocks until a single client connects.
// Only one client is supported at a time.
func (s *TCPVideoServer) AcceptOne() error {
	conn, err := s.listener.Accept()
	if err != nil {
		return err
	}
	// Set TCP_NODELAY for low latency
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetWriteBuffer(4 * 1024 * 1024) // 4MB for 8Mbps@60fps headroom
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	log.Printf("TCP video client connected from %s", conn.RemoteAddr())
	return nil
}

// SendFrame sends a NAL unit as a single length-prefixed TCP message.
// Format: [4-byte length][17-byte header][payload]
// This implements the Streamer interface.
//
// Optimized for zero per-frame heap allocations:
//   - Frame buffer is recycled via sync.Pool
//   - Header is serialized inline (no PacketHeader struct or Marshal call)
func (s *TCPVideoServer) SendFrame(nalUnit []byte, frameType byte, captureMicros uint64) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("no TCP video client connected")
	}

	seq := s.sequence.Add(1)
	timestamp := captureMicros

	// Total wire size: 4-byte length prefix + 17-byte header + payload
	totalLen := PacketHeaderSize + len(nalUnit)
	needed := 4 + totalLen

	// Get a reusable buffer from the pool
	bufp := framePool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < needed {
		// Buffer too small — grow it. The old backing array becomes garbage,
		// but this only happens when a larger-than-usual NAL is encountered;
		// subsequent frames reuse the bigger buffer.
		buf = make([]byte, needed)
	} else {
		buf = buf[:needed]
	}

	// 4-byte length prefix (big-endian)
	binary.BigEndian.PutUint32(buf[0:4], uint32(totalLen))

	// Inline header serialization — same layout as PacketHeader.Marshal()
	// Sequence  (4B) @ offset 4
	binary.BigEndian.PutUint32(buf[4:8], seq)
	// Timestamp (8B) @ offset 8
	binary.BigEndian.PutUint64(buf[8:16], timestamp)
	// FrameType (1B) @ offset 16
	buf[16] = frameType
	// FragIndex (2B) @ offset 17  — always 0 for TCP (no fragmentation)
	buf[17] = 0
	buf[18] = 0
	// FragTotal (2B) @ offset 19  — always 1 for TCP
	buf[19] = 0
	buf[20] = 1

	// Payload
	copy(buf[4+PacketHeaderSize:], nalUnit)

	// Set write deadline to avoid blocking on slow clients
	conn.SetWriteDeadline(time.Now().Add(150 * time.Millisecond))
	_, err := conn.Write(buf)

	// Return buffer to pool — must update the pointer in case we grew it
	*bufp = buf
	framePool.Put(bufp)

	if err != nil {
		// For non-keyframes, write timeout is acceptable (skip frame)
		if frameType != FrameTypeIDR && frameType != FrameTypeSPS && frameType != FrameTypePPS {
			return nil // silently skip
		}
		return err
	}

	return nil
}

// Close stops the server and closes all connections.
func (s *TCPVideoServer) Close() error {
	close(s.done)
	s.mu.Lock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	s.mu.Unlock()
	return s.listener.Close()
}
