package touch

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

type EventHandler func(Event)

// SessionHeaderSize is the size of the session ID a client sends right after
// connecting, before any event. It is the sessionId from ServerHello (uint32,
// big-endian) and routes this connection's events to that session's handler.
const SessionHeaderSize = 4

// handshakeTimeout bounds how long a new connection may take to send its session ID.
const handshakeTimeout = 10 * time.Second

// Server accepts touch connections from any number of phones and routes each
// event to the handler registered for the connection's session.
type Server struct {
	listener net.Listener
	mu       sync.Mutex // protects handlers
	handlers map[uint32]EventHandler
	done     chan struct{}
	wg       sync.WaitGroup
}

func NewServer(port int) (*Server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	return &Server{
		listener: ln,
		handlers: make(map[uint32]EventHandler),
		done:     make(chan struct{}),
	}, nil
}

func (s *Server) Port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Register sets the handler for a session's events, replacing any previous one.
func (s *Server) Register(session uint32, handler EventHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[session] = handler
}

// Unregister removes a session's handler. Later events for it are dropped.
func (s *Server) Unregister(session uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handlers, session)
}

func (s *Server) getHandler(session uint32) EventHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handlers[session]
}

func (s *Server) AcceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				log.Printf("touch accept error: %v", err)
				continue
			}
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	var header [SessionHeaderSize]byte
	conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		log.Printf("touch session header read error: %v", err)
		return
	}
	conn.SetReadDeadline(time.Time{})
	session := binary.BigEndian.Uint32(header[:])

	buf := make([]byte, EventSize)
	for {
		_, err := io.ReadFull(conn, buf)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				log.Printf("touch read error (session %d): %v", session, err)
			}
			return
		}
		if h := s.getHandler(session); h != nil {
			h(Unmarshal(buf))
		}
	}
}

func (s *Server) Stop() {
	close(s.done)
	s.listener.Close()
	s.wg.Wait()
}
