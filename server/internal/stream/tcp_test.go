package stream

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// dialVideo connects like a phone: session header first.
func dialVideo(t *testing.T, srv *TCPVideoServer, session uint32) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var header [SessionHeaderSize]byte
	binary.BigEndian.PutUint32(header[:], session)
	if _, err := conn.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	return conn
}

// readFrame reads one length-prefixed frame and returns its payload.
func readFrame(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		t.Fatalf("read length: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(conn, body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body[PacketHeaderSize:]
}

// Two phones share the video port. Each must receive only its own session's
// frames, whichever connects first and whether or not Accept was already waiting.
func TestTCPVideoRoutesConnectionsBySession(t *testing.T) {
	srv, err := NewTCPVideoServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.AcceptLoop()

	ctx := context.Background()

	// Session 2's phone connects before anyone calls Accept for it.
	phone2 := dialVideo(t, srv, 2)
	time.Sleep(100 * time.Millisecond)

	// Session 1 is already waiting when its phone connects.
	type accepted struct {
		stream *TCPVideoStream
		err    error
	}
	got1 := make(chan accepted, 1)
	go func() {
		s, err := srv.Accept(ctx, 1, 2*time.Second)
		got1 <- accepted{s, err}
	}()
	time.Sleep(100 * time.Millisecond)
	phone1 := dialVideo(t, srv, 1)

	stream2, err := srv.Accept(ctx, 2, 2*time.Second)
	if err != nil {
		t.Fatalf("session 2: %v", err)
	}
	a1 := <-got1
	if a1.err != nil {
		t.Fatalf("session 1: %v", a1.err)
	}

	if err := a1.stream.SendFrame([]byte("for-one"), FrameTypeP, 1); err != nil {
		t.Fatal(err)
	}
	if err := stream2.SendFrame([]byte("for-two"), FrameTypeP, 1); err != nil {
		t.Fatal(err)
	}

	if got := string(readFrame(t, phone1)); got != "for-one" {
		t.Errorf("phone 1 received %q, want %q", got, "for-one")
	}
	if got := string(readFrame(t, phone2)); got != "for-two" {
		t.Errorf("phone 2 received %q, want %q", got, "for-two")
	}
}

func TestTCPVideoAcceptTimesOut(t *testing.T) {
	srv, err := NewTCPVideoServer(0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.AcceptLoop()

	if _, err := srv.Accept(context.Background(), 7, 100*time.Millisecond); err == nil {
		t.Fatal("expected a timeout when the phone never connects")
	}
}
