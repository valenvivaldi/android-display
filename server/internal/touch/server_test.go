package touch

import (
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

// dial connects to the server and sends the session header, like a phone does.
func dial(t *testing.T, srv *Server, session uint32) net.Conn {
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

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	go srv.AcceptLoop()
	return srv
}

func TestServerReceivesEvent(t *testing.T) {
	srv := newTestServer(t)
	received := make(chan Event, 10)
	srv.Register(1, func(e Event) { received <- e })

	conn := dial(t, srv, 1)
	event := Event{
		Type: TouchTypeFinger, Action: TouchActionDown,
		X: 0.5, Y: 0.3, Pressure: 0.0, PointerID: 0, Timestamp: 100,
	}
	if _, err := conn.Write(event.Marshal()); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-received:
		if got.Type != event.Type {
			t.Fatalf("type mismatch: got %d want %d", got.Type, event.Type)
		}
		if got.Action != event.Action {
			t.Fatalf("action mismatch: got %d want %d", got.Action, event.Action)
		}
		if got.X != event.X || got.Y != event.Y {
			t.Fatalf("coord mismatch: got (%f,%f) want (%f,%f)", got.X, got.Y, event.X, event.Y)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestServerMultipleEvents(t *testing.T) {
	srv := newTestServer(t)
	received := make(chan Event, 100)
	srv.Register(1, func(e Event) { received <- e })

	conn := dial(t, srv, 1)
	events := []Event{
		{Type: TouchTypeFinger, Action: TouchActionDown, X: 0.5, Y: 0.5, PointerID: 0, Timestamp: 100},
		{Type: TouchTypeFinger, Action: TouchActionMove, X: 0.6, Y: 0.5, PointerID: 0, Timestamp: 110},
		{Type: TouchTypeFinger, Action: TouchActionUp, X: 0.6, Y: 0.5, PointerID: 0, Timestamp: 120},
	}
	for _, e := range events {
		if _, err := conn.Write(e.Marshal()); err != nil {
			t.Fatal(err)
		}
	}

	for i, want := range events {
		select {
		case got := <-received:
			if got.Action != want.Action {
				t.Fatalf("event %d: action mismatch: got %d want %d", i, got.Action, want.Action)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}
}

// With several phones connected, each phone's touches must drive only its own
// display, and a session that has been unregistered must not receive events.
func TestServerRoutesEventsBySession(t *testing.T) {
	srv := newTestServer(t)
	got := map[uint32]chan Event{1: make(chan Event, 10), 2: make(chan Event, 10)}
	for id, ch := range got {
		ch := ch
		srv.Register(id, func(e Event) { ch <- e })
	}

	phone1 := dial(t, srv, 1)
	phone2 := dial(t, srv, 2)
	e1 := Event{Type: TouchTypeFinger, Action: TouchActionDown, X: 0.1, Y: 0.1}
	e2 := Event{Type: TouchTypeFinger, Action: TouchActionDown, X: 0.9, Y: 0.9}
	phone1.Write(e1.Marshal())
	phone2.Write(e2.Marshal())

	for id, want := range map[uint32]Event{1: e1, 2: e2} {
		select {
		case ev := <-got[id]:
			if ev.X != want.X {
				t.Fatalf("session %d received X=%v, want %v (events crossed between sessions)", id, ev.X, want.X)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("session %d: timeout waiting for its event", id)
		}
	}

	srv.Unregister(1)
	phone1.Write(e1.Marshal())
	phone2.Write(e2.Marshal())
	select {
	case <-got[2]:
	case <-time.After(2 * time.Second):
		t.Fatal("session 2 stopped receiving after session 1 was unregistered")
	}
	select {
	case ev := <-got[1]:
		t.Fatalf("unregistered session 1 still received %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}
