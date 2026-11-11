package main

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/luke/android-mac/server/internal/control"
)

func TestParseCommand(t *testing.T) {
	for line, want := range map[string]struct {
		id uint32
		ok bool
	}{
		"reconnect 3":       {3, true},
		"  reconnect   12 ": {12, true},
		"reconnect":         {0, false},
		"reconnect x":       {0, false},
		"reconnect -1":      {0, false},
		"reconnect 1 2":     {0, false},
		"stop 3":            {0, false},
		"":                  {0, false},
	} {
		id, ok := parseCommand(line)
		if id != want.id || ok != want.ok {
			t.Errorf("parseCommand(%q) = (%d, %v), want (%d, %v)", line, id, ok, want.id, want.ok)
		}
	}
}

// The Mac app's "reconnect" button reaches exactly the phone of that session, and only
// while the session is live.
func TestReconnectCommandReachesOnlyThatPhone(t *testing.T) {
	phone1, server1 := net.Pipe()
	phone2, server2 := net.Pipe()
	defer phone1.Close()
	defer phone2.Close()

	srv := &server{}
	srv.track(1, control.ClientConn{Conn: server1})
	srv.track(2, control.ClientConn{Conn: server2})

	// net.Pipe is synchronous: write from the server side while the phones read.
	lines := make(chan string, 2)
	for i, conn := range []net.Conn{phone1, phone2} {
		go func(i int, conn net.Conn) {
			line, _ := bufio.NewReader(conn).ReadString('\n')
			lines <- strings.TrimSpace(line) + "@" + string(rune('1'+i))
		}(i, conn)
	}

	srv.runCommands(strings.NewReader("reconnect 2\n"))

	select {
	case got := <-lines:
		if got != `{"type":"reconnect"}@2` {
			t.Fatalf("got %q, want the reconnect message on phone 2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("phone 2 never received the reconnect message")
	}
	select {
	case got := <-lines:
		t.Fatalf("phone 1 received %q, but only session 2 was asked to reconnect", got)
	case <-time.After(200 * time.Millisecond):
	}

	srv.untrack(2)
	if err := srv.requestReconnect(2); err == nil {
		t.Error("requesting a reconnect for an ended session should fail")
	}
}
