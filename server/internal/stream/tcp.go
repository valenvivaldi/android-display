package stream

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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

// SessionHeaderSize is the size of the session ID a phone sends right after
// connecting to the video port: the sessionId from ServerHello (uint32, big-endian).
const SessionHeaderSize = 4

// TCPVideoServer accepts the video connections of any number of phones.
// Used for USB connections where ADB only supports TCP tunneling.
//
// Every phone connects to the same port and identifies its session with a
// 4-byte header; Accept hands the connection to the session that owns it.
type TCPVideoServer struct {
	listener net.Listener
	mu       sync.Mutex
	slots    map[uint32]chan net.Conn
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
		slots:    make(map[uint32]chan net.Conn),
		done:     make(chan struct{}),
	}, nil
}

// Port returns the actual port the server is listening on.
func (s *TCPVideoServer) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// slot returns the channel on which the session's connection is delivered,
// creating it if needed. Either side may get there first: the phone can
// connect before the pipeline calls Accept.
func (s *TCPVideoServer) slot(session uint32) chan net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.slots[session]
	if !ok {
		ch = make(chan net.Conn, 1)
		s.slots[session] = ch
	}
	return ch
}

// AcceptLoop accepts connections and routes them to their sessions. Run it in a goroutine.
func (s *TCPVideoServer) AcceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				log.Printf("video accept error: %v", err)
				continue
			}
		}
		go s.route(conn)
	}
}

func (s *TCPVideoServer) route(conn net.Conn) {
	var header [SessionHeaderSize]byte
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		log.Printf("video session header read error: %v", err)
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	session := binary.BigEndian.Uint32(header[:])

	// Set TCP_NODELAY for low latency
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetWriteBuffer(4 * 1024 * 1024) // 4MB for 8Mbps@60fps headroom
	}

	select {
	case s.slot(session) <- conn:
	default:
		log.Printf("video: session %d connected twice, closing the extra connection", session)
		conn.Close()
	}
}

// Accept waits until the session's phone has connected to the video port.
func (s *TCPVideoServer) Accept(ctx context.Context, session uint32, timeout time.Duration) (*TCPVideoStream, error) {
	ch := s.slot(session)
	defer func() {
		s.mu.Lock()
		delete(s.slots, session)
		s.mu.Unlock()
	}()

	select {
	case conn := <-ch:
		log.Printf("session %d: TCP video client connected from %s", session, conn.RemoteAddr())
		return &TCPVideoStream{conn: conn}, nil
	case <-ctx.Done():
		drain(ch)
		return nil, ctx.Err()
	case <-time.After(timeout):
		drain(ch)
		return nil, errors.New("timed out waiting for the phone's video connection")
	}
}

func drain(ch chan net.Conn) {
	select {
	case conn := <-ch:
		conn.Close()
	default:
	}
}

// Close stops accepting new connections. Established streams are closed by their sessions.
func (s *TCPVideoServer) Close() error {
	close(s.done)
	return s.listener.Close()
}

// TCPVideoStream streams H.264 NAL units of one session over TCP with length-prefix framing.
//
// Wire format per frame:
//
//	[4 bytes: total packet length (big-endian)] [17 bytes: packet header] [payload]
//
// Unlike UDP, TCP does not fragment — each NAL is sent as a single "packet"
// with fragIndex=0, fragTotal=1. The 4-byte length prefix provides message
// framing on the TCP stream.
type TCPVideoStream struct {
	conn     net.Conn
	sequence atomic.Uint32
}

// SendFrame sends a NAL unit as a single length-prefixed TCP message.
// Format: [4-byte length][17-byte header][payload]
// This implements the Streamer interface.
//
// Optimized for zero per-frame heap allocations:
//   - Frame buffer is recycled via sync.Pool
//   - Header is serialized inline (no PacketHeader struct or Marshal call)
func (s *TCPVideoStream) SendFrame(nalUnit []byte, frameType byte, captureMicros uint64) error {
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
	s.conn.SetWriteDeadline(time.Now().Add(150 * time.Millisecond))
	_, err := s.conn.Write(buf)

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

// Close closes the connection to the phone.
func (s *TCPVideoStream) Close() error {
	return s.conn.Close()
}
