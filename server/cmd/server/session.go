package main

import (
	"encoding/json"
	"errors"
	"hash/fnv"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// session is one connected phone: its own virtual display, encoder, capture and touch routing.
type session struct {
	id       uint32
	deviceID string // stable per phone; keys the display identity and its saved position
	name     string // phone model, used as the display's name
}

// logf logs with a "session N: " prefix; the Mac app parses these lines per session.
func (s *session) logf(format string, args ...any) {
	log.Printf("session %d: "+format, append([]any{s.id}, args...)...)
}

// displaySerial derives the virtual display's serial number from the phone's id.
// macOS keys a display's saved arrangement on a UUID built from vendor, product and
// serial, so it must be stable for a phone and different between phones. Never 0:
// with serial 0 macOS assigns a random UUID on every connection.
func displaySerial(deviceID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(deviceID))
	if serial := h.Sum32(); serial != 0 {
		return serial
	}
	return 1
}

type origin struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// originStore remembers the position of each phone's display, keyed by device id, in a
// JSON file. Safe for concurrent use: every session saves its own entry.
type originStore struct {
	path string
	mu   sync.Mutex
}

func defaultOriginStore() (*originStore, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &originStore{path: filepath.Join(dir, "AndroidDisplay", "display-origins.json")}, nil
}

// read loads the whole file. A missing file is an empty store. Caller holds s.mu.
func (s *originStore) read() (map[string]origin, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]origin{}, nil
	}
	if err != nil {
		return nil, err
	}
	origins := map[string]origin{}
	if err := json.Unmarshal(data, &origins); err != nil {
		return nil, err
	}
	return origins, nil
}

func (s *originStore) load(deviceID string) (x, y int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	origins, err := s.read()
	if err != nil {
		log.Printf("read display positions: %v", err)
		return 0, 0, false
	}
	o, ok := origins[deviceID]
	return o.X, o.Y, ok
}

func (s *originStore) save(deviceID string, x, y int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	origins, err := s.read()
	if err != nil {
		// A corrupt file must not stop the position from being saved from now on.
		log.Printf("read display positions (starting over): %v", err)
		origins = map[string]origin{}
	}
	origins[deviceID] = origin{X: x, Y: y}
	data, err := json.MarshalIndent(origins, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	// Write then rename so a crash never leaves a half-written file.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
