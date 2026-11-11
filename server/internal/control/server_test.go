package control

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/luke/android-mac/server/internal/protocol"
)

func TestHandshake(t *testing.T) {
	srv, err := NewServer(0, 0.75, 4_000_000) // port 0 = random available port
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Stop()

	srv.SetTouchPort(9001)

	clientReady := make(chan ClientConn, 1)
	srv.OnClient(func(c ClientConn) {
		clientReady <- c
	})

	go srv.AcceptLoop()

	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Step 1: Send ClientHello
	hello := protocol.ClientHello{
		Device:       "Tab S6 Lite",
		Screen:       protocol.ScreenInfo{Width: 2304, Height: 1020, DPI: 224},
		Capabilities: []string{"touch", "pen"},
		Codecs:       []string{"h264"},
	}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(hello); err != nil {
		t.Fatalf("failed to send hello: %v", err)
	}

	// Step 2: Read ServerHello
	var response protocol.ServerHello
	decoder := json.NewDecoder(conn)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	if response.Codec != "h264" {
		t.Errorf("expected codec h264, got %s", response.Codec)
	}
	// Server applies its scale and rounds down to even dimensions (1020*0.75 = 765 → 764).
	if response.VirtualDisplay.Width != 1728 || response.VirtualDisplay.Height != 764 {
		t.Errorf("expected 1728x764, got %dx%d", response.VirtualDisplay.Width, response.VirtualDisplay.Height)
	}
	if response.Bitrate != 4_000_000 {
		t.Errorf("expected bitrate 4000000, got %d", response.Bitrate)
	}
	if response.StreamPort == 0 {
		t.Error("stream port should not be 0")
	}
	if response.TouchPort != 9001 {
		t.Errorf("expected touch port 9001, got %d", response.TouchPort)
	}
	if response.SessionID == 0 {
		t.Error("session id should not be 0")
	}

	// Step 3: Send ClientReady with the actual UDP port
	ready := protocol.ClientReady{UDPPort: 12345}
	if err := enc.Encode(ready); err != nil {
		t.Fatalf("failed to send client ready: %v", err)
	}

	// Verify the server received the UDP port
	select {
	case client := <-clientReady:
		if client.UDPPort != 12345 {
			t.Errorf("expected UDP port 12345, got %d", client.UDPPort)
		}
		if client.Width != 1728 || client.Height != 764 || client.Bitrate != 4_000_000 {
			t.Errorf("client conn has %dx%d @ %d, want 1728x764 @ 4000000", client.Width, client.Height, client.Bitrate)
		}
		if client.SessionID != response.SessionID {
			t.Errorf("session id %d sent to the phone, %d given to the pipeline", response.SessionID, client.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client ready callback")
	}
}

// Several phones connect at the same time. Each needs its own session id: the
// touch and video connections are routed by it, so a repeat would cross two phones.
func TestHandshakeAssignsDistinctSessions(t *testing.T) {
	srv, err := NewServer(0, 1, 8_000_000)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Stop()

	ready := make(chan ClientConn, 3)
	srv.OnClient(func(c ClientConn) { ready <- c })
	go srv.AcceptLoop()

	sessions := map[uint32]string{}
	for _, device := range []string{"moto g(9) plus", "XT1025", "Pixel"} {
		conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
		if err != nil {
			t.Fatalf("failed to connect: %v", err)
		}
		defer conn.Close()

		enc := json.NewEncoder(conn)
		hello := protocol.ClientHello{Device: device, DeviceID: "id-" + device, Screen: protocol.ScreenInfo{Width: 1080, Height: 2400}, Codecs: []string{"h264"}}
		if err := enc.Encode(hello); err != nil {
			t.Fatal(err)
		}
		var response protocol.ServerHello
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := json.NewDecoder(conn).Decode(&response); err != nil {
			t.Fatalf("%s: read server hello: %v", device, err)
		}
		if other, dup := sessions[response.SessionID]; dup {
			t.Fatalf("%s got session %d, already given to %s", device, response.SessionID, other)
		}
		sessions[response.SessionID] = device

		if err := enc.Encode(protocol.ClientReady{}); err != nil {
			t.Fatal(err)
		}
		select {
		case c := <-ready:
			if c.Hello.DeviceID != "id-"+device {
				t.Errorf("pipeline got device id %q, want %q", c.Hello.DeviceID, "id-"+device)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: timed out waiting for the client callback", device)
		}
	}
}

// Each phone gets the resolution and bitrate chosen for it, not one global value, and
// the choice is made per handshake (the Mac app can change it while the server runs).
func TestHandshakeUsesPerDeviceStreamSettings(t *testing.T) {
	srv, err := NewServer(0, 1, 8_000_000) // defaults for phones the settings don't cover
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Stop()
	small := true
	srv.SetStreamSettings(func(deviceID string) (float64, int) {
		switch deviceID {
		case "tablet":
			return 0.5, 2_000_000
		case "phone":
			if small {
				return 0.75, 4_000_000
			}
			return 1, 6_000_000
		}
		return 1, 8_000_000
	})
	go srv.AcceptLoop()

	handshake := func(deviceID string) protocol.ServerHello {
		t.Helper()
		conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
		if err != nil {
			t.Fatalf("failed to connect: %v", err)
		}
		defer conn.Close()
		hello := protocol.ClientHello{Device: deviceID, DeviceID: deviceID, Screen: protocol.ScreenInfo{Width: 2000, Height: 1000}, Codecs: []string{"h264"}}
		if err := json.NewEncoder(conn).Encode(hello); err != nil {
			t.Fatal(err)
		}
		var response protocol.ServerHello
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := json.NewDecoder(conn).Decode(&response); err != nil {
			t.Fatalf("%s: read server hello: %v", deviceID, err)
		}
		return response
	}

	check := func(deviceID string, width, height, bitrate int) {
		t.Helper()
		r := handshake(deviceID)
		if r.VirtualDisplay.Width != width || r.VirtualDisplay.Height != height || r.Bitrate != bitrate {
			t.Errorf("%s: got %dx%d @ %d, want %dx%d @ %d", deviceID, r.VirtualDisplay.Width, r.VirtualDisplay.Height, r.Bitrate, width, height, bitrate)
		}
	}
	check("tablet", 1000, 500, 2_000_000)
	check("phone", 1500, 750, 4_000_000)
	check("unknown", 2000, 1000, 8_000_000)

	small = false
	check("phone", 2000, 1000, 6_000_000)
}

func TestHandshakeTimeout(t *testing.T) {
	srv, err := NewServer(0, 1, 8_000_000)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	defer srv.Stop()

	go srv.AcceptLoop()

	// Connect but never send anything — server should close the connection
	// due to the 10-second handshake timeout.
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	// The server has a 10s handshake timeout. We wait a bit longer to verify
	// it closes the connection. Read should eventually fail.
	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(12 * time.Second))
	_, readErr := conn.Read(buf)
	if readErr == nil {
		t.Error("expected read to fail after handshake timeout, but it succeeded")
	}
}
