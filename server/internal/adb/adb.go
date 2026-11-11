package adb

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Manager handles ADB device detection and reverse port forwarding
// for USB connections.
type Manager struct {
	adbPath string
}

// NewManager creates an ADB manager by finding the adb binary.
// Searches: PATH, ~/Library/Android/sdk, ~/Android/sdk, and project platform-tools.
func NewManager() (*Manager, error) {
	// Try PATH first
	if path, err := exec.LookPath("adb"); err == nil {
		return &Manager{adbPath: path}, nil
	}

	// Try common SDK locations
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "Library", "Android", "sdk", "platform-tools", "adb"),
		filepath.Join(home, "Android", "sdk", "platform-tools", "adb"),
	}

	// Try ANDROID_SDK_ROOT / ANDROID_HOME
	for _, env := range []string{"ANDROID_SDK_ROOT", "ANDROID_HOME"} {
		if root := os.Getenv(env); root != "" {
			candidates = append(candidates, filepath.Join(root, "platform-tools", "adb"))
		}
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return &Manager{adbPath: path}, nil
		}
	}

	return nil, fmt.Errorf("adb not found in PATH or common locations")
}

// Path returns the resolved adb binary path.
func (m *Manager) Path() string {
	return m.adbPath
}

// Devices returns the serials of the authorized devices currently attached.
func (m *Manager) Devices() ([]string, error) {
	out, err := exec.Command(m.adbPath, "devices").Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices failed: %v", err)
	}
	return parseDevices(string(out)), nil
}

// parseDevices extracts the serials in the "device" state from `adb devices` output.
// Unauthorized and offline devices, the header and daemon start-up messages are skipped.
func parseDevices(out string) []string {
	var serials []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "device" {
			serials = append(serials, fields[0])
		}
	}
	return serials
}

// SetupReverse creates a reverse port forwarding on one device: its localhost:port → Mac localhost:port.
// This allows the Android app to connect to localhost:port and reach the Mac server.
func (m *Manager) SetupReverse(serial string, port int) error {
	portStr := fmt.Sprintf("tcp:%d", port)
	out, err := exec.Command(m.adbPath, "-s", serial, "reverse", portStr, portStr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb -s %s reverse %s failed: %v (%s)", serial, portStr, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveAllReverse removes all reverse forwarding rules on one device.
func (m *Manager) RemoveAllReverse(serial string) error {
	out, err := exec.Command(m.adbPath, "-s", serial, "reverse", "--remove-all").CombinedOutput()
	if err != nil {
		return fmt.Errorf("adb -s %s reverse --remove-all failed: %v (%s)", serial, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Watch keeps USB forwarding set up for every attached device until ctx is done:
// phones plugged in later get their ports forwarded within one interval, and the
// rules are removed again from every device it set up when ctx ends.
func (m *Manager) Watch(ctx context.Context, ports []int, interval time.Duration) {
	known := make(map[string]bool)
	defer func() {
		for serial := range known {
			if err := m.RemoveAllReverse(serial); err != nil {
				log.Printf("adb cleanup: %v", err)
			}
		}
	}()

	for {
		m.syncDevices(ports, known)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (m *Manager) syncDevices(ports []int, known map[string]bool) {
	serials, err := m.Devices()
	if err != nil {
		log.Printf("adb: %v", err)
		return
	}

	present := make(map[string]bool, len(serials))
	for _, serial := range serials {
		present[serial] = true
		if known[serial] {
			continue
		}
		// Clear stale rules from a previous run, then forward every port.
		if err := m.RemoveAllReverse(serial); err != nil {
			log.Printf("adb: %v", err)
		}
		ok := true
		for _, port := range ports {
			if err := m.SetupReverse(serial, port); err != nil {
				log.Printf("adb: %v", err)
				ok = false
				break
			}
		}
		if ok {
			known[serial] = true
			log.Printf("adb device connected: %s (USB forwarding enabled)", serial)
		}
	}
	for serial := range known {
		if !present[serial] {
			delete(known, serial)
			log.Printf("adb device disconnected: %s", serial)
		}
	}
}
