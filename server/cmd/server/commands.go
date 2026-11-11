package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"

	"github.com/luke/android-mac/server/internal/control"
)

// Commands the Mac app writes to the server's stdin, one per line:
//
//	reconnect <session id>   ask that phone to reconnect, so new settings apply

// track registers a live session so commands can reach it.
func (srv *server) track(id uint32, client control.ClientConn) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.clients == nil {
		srv.clients = make(map[uint32]control.ClientConn)
	}
	srv.clients[id] = client
}

func (srv *server) untrack(id uint32) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	delete(srv.clients, id)
}

// requestReconnect asks the phone of a live session to reconnect itself. The phone closes
// its control connection, which ends the session; its new handshake picks up the settings.
func (srv *server) requestReconnect(id uint32) error {
	srv.mu.Lock()
	client, ok := srv.clients[id]
	srv.mu.Unlock()
	if !ok {
		return fmt.Errorf("no live session %d", id)
	}
	return client.RequestReconnect()
}

// parseCommand reads "reconnect <session id>". ok is false for anything else.
func parseCommand(line string) (session uint32, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "reconnect" {
		return 0, false
	}
	id, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(id), true
}

// runCommands executes commands from r until it reaches EOF.
func (srv *server) runCommands(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		id, ok := parseCommand(line)
		if !ok {
			log.Printf("unknown command: %q", line)
			continue
		}
		if err := srv.requestReconnect(id); err != nil {
			log.Printf("session %d: reconnect request failed: %v", id, err)
			continue
		}
		log.Printf("session %d: reconnect requested", id)
	}
}
