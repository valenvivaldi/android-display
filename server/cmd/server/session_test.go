package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The serial is what makes macOS treat phones as different, persistent monitors.
func TestDisplaySerial(t *testing.T) {
	a, b := displaySerial("phone-a"), displaySerial("phone-b")
	if a == 0 || b == 0 {
		t.Fatalf("serial must never be 0 (macOS would assign a random UUID): %d, %d", a, b)
	}
	if a == b {
		t.Fatalf("different phones got the same serial %d", a)
	}
	if displaySerial("phone-a") != a {
		t.Fatal("serial for the same phone changed between calls")
	}
}

// Each phone's position is remembered separately and survives a restart.
func TestOriginStorePerDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "origins.json")
	store := &originStore{path: path}

	if _, _, ok := store.load("a"); ok {
		t.Fatal("empty store reported a saved position")
	}
	if err := store.save("a", 1920, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.save("b", -1280, 300); err != nil {
		t.Fatal(err)
	}
	if err := store.save("a", 1678, 1309); err != nil { // moving a must not touch b
		t.Fatal(err)
	}

	reopened := &originStore{path: path}
	if x, y, ok := reopened.load("a"); !ok || x != 1678 || y != 1309 {
		t.Errorf("a = (%d,%d,%v), want (1678,1309,true)", x, y, ok)
	}
	if x, y, ok := reopened.load("b"); !ok || x != -1280 || y != 300 {
		t.Errorf("b = (%d,%d,%v), want (-1280,300,true)", x, y, ok)
	}
}

func TestOriginStoreRecoversFromCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "origins.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &originStore{path: path}
	if _, _, ok := store.load("a"); ok {
		t.Fatal("corrupt file reported a position")
	}
	if err := store.save("a", 5, 6); err != nil {
		t.Fatalf("save over a corrupt file: %v", err)
	}
	if x, y, ok := store.load("a"); !ok || x != 5 || y != 6 {
		t.Errorf("after recovery a = (%d,%d,%v), want (5,6,true)", x, y, ok)
	}
}
