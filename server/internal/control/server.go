package control

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/luke/android-mac/server/internal/protocol"
)

type ClientConn struct {
	Conn      net.Conn
	Hello     protocol.ClientHello
	SessionID uint32 // unique per connection; routes this phone's touch and video connections
	UDPPort   int    // actual UDP port reported by the client via ClientReady (0 for USB)
	// Negotiated stream parameters (also sent to the client in ServerHello).
	Width   int
	Height  int
	Bitrate int
}

// StreamSettings picks the streamed resolution (scale of the phone's native screen) and
// bitrate for a phone, given its device id. The server defaults apply when it is nil.
type StreamSettings func(deviceID string) (scale float64, bitrate int)

type Server struct {
	listener       net.Listener
	clients        []ClientConn
	mu             sync.Mutex
	nextSession    atomic.Uint32
	streamPort     int
	touchPort      int
	videoPort      int // TCP video port for USB mode
	scale          float64
	bitrate        int
	streamSettings StreamSettings
	onClient       func(ClientConn)
	done           chan struct{}
}

func NewServer(port int, scale float64, bitrate int) (*Server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	return &Server{
		listener:   ln,
		streamPort: 5001,
		scale:      scale,
		bitrate:    bitrate,
		done:       make(chan struct{}),
	}, nil
}

// scaleEven scales a dimension and rounds down to an even number (H.264 requirement).
func scaleEven(v int, scale float64) int {
	return int(float64(v)*scale) &^ 1
}

func (s *Server) Addr() string {
	return s.listener.Addr().String()
}

func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func (s *Server) SetStreamPort(port int) {
	s.streamPort = port
}

func (s *Server) SetTouchPort(port int) {
	s.touchPort = port
}

// SetStreamSettings sets how each phone's resolution and bitrate are chosen. It is
// consulted on every handshake, so changes apply the next time a phone connects.
func (s *Server) SetStreamSettings(fn StreamSettings) {
	s.streamSettings = fn
}

func (s *Server) SetVideoPort(port int) {
	s.videoPort = port
}

func (s *Server) OnClient(fn func(ClientConn)) {
	s.onClient = fn
}

func (s *Server) AcceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				log.Printf("accept error: %v", err)
				continue
			}
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	// I6: Enforce a 10-second deadline for the entire handshake phase.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	decoder := json.NewDecoder(conn)

	// Step 1: Read ClientHello
	var hello protocol.ClientHello
	if err := decoder.Decode(&hello); err != nil {
		log.Printf("handshake read error: %v", err)
		conn.Close()
		return
	}

	session := s.nextSession.Add(1)
	connType := "WiFi"
	if hello.IsUSB() {
		connType = "USB"
	}
	log.Printf("session %d: client connected: %s (%dx%d) [%s] id=%s", session, hello.Device, hello.Screen.Width, hello.Screen.Height, connType, hello.DeviceID)

	codec := "h264"
	for _, c := range hello.Codecs {
		if c == "h264" {
			codec = "h264"
			break
		}
	}

	// Step 2: Send ServerHello with the size and bitrate chosen for this phone
	scale, bitrate := s.scale, s.bitrate
	if s.streamSettings != nil {
		scale, bitrate = s.streamSettings(hello.DeviceID)
	}
	width := scaleEven(hello.Screen.Width, scale)
	height := scaleEven(hello.Screen.Height, scale)
	log.Printf("session %d: stream config: %dx%d @ %d bps (scale %.2f)", session, width, height, bitrate, scale)
	response := protocol.ServerHello{
		SessionID: session,
		VirtualDisplay: protocol.DisplayInfo{
			Width:  width,
			Height: height,
		},
		Codec:      codec,
		Bitrate:    bitrate,
		FPS:        60,
		StreamPort: s.streamPort,
		TouchPort:  s.touchPort,
		VideoPort:  s.videoPort,
	}

	enc := json.NewEncoder(conn)
	if err := enc.Encode(response); err != nil {
		log.Printf("handshake write error: %v", err)
		conn.Close()
		return
	}

	// Step 3: Read ClientReady to get the actual UDP port
	var ready protocol.ClientReady
	if err := decoder.Decode(&ready); err != nil {
		log.Printf("client ready read error: %v", err)
		conn.Close()
		return
	}
	if hello.IsUSB() {
		log.Printf("session %d: client ready: USB mode (TCP video)", session)
	} else {
		log.Printf("session %d: client ready: UDP port %d", session, ready.UDPPort)
	}

	// Clear the read deadline after successful handshake.
	conn.SetReadDeadline(time.Time{})

	client := ClientConn{Conn: conn, Hello: hello, SessionID: session, UDPPort: ready.UDPPort, Width: width, Height: height, Bitrate: bitrate}
	s.mu.Lock()
	s.clients = append(s.clients, client)
	s.mu.Unlock()

	if s.onClient != nil {
		s.onClient(client)
	}
}

func (s *Server) Stop() {
	close(s.done)
	s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		c.Conn.Close()
	}
}
