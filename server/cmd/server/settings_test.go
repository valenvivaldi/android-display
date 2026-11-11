package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/luke/android-mac/server/internal/input"
)

func newSettings(t *testing.T, contents string) *settingsStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "device-settings.json")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &settingsStore{path: path, flagScale: 1.0, flagBitrate: 8_000_000}
}

// Phone's own entry > file defaults > flags, resolved independently for scale and bitrate.
func TestSettingsPrecedence(t *testing.T) {
	s := newSettings(t, `{
		"defaults": {"scale": 0.75, "bitrate": 4000000},
		"devices": {
			"both":      {"name": "A", "scale": 0.5, "bitrate": 2000000},
			"scaleOnly": {"name": "B", "scale": 0.9},
			"unset":     {"name": "C"}
		}
	}`)

	for _, tc := range []struct {
		id      string
		scale   float64
		bitrate int
	}{
		{"both", 0.5, 2_000_000},
		{"scaleOnly", 0.9, 4_000_000}, // bitrate falls through to the file defaults
		{"unset", 0.75, 4_000_000},    // known phone with no own values
		{"stranger", 0.75, 4_000_000}, // phone the file has never seen
	} {
		scale, bitrate := s.lookup(tc.id)
		if scale != tc.scale || bitrate != tc.bitrate {
			t.Errorf("%s: got (%v, %d), want (%v, %d)", tc.id, scale, bitrate, tc.scale, tc.bitrate)
		}
	}
}

func TestSettingsFallBackToFlags(t *testing.T) {
	for name, contents := range map[string]string{
		"missing file":   "",
		"empty object":   `{}`,
		"corrupt file":   `{not json`,
		"defaults unset": `{"defaults": {}, "devices": {"x": {"name": "X"}}}`,
	} {
		s := newSettings(t, contents)
		if scale, bitrate := s.lookup("x"); scale != 1.0 || bitrate != 8_000_000 {
			t.Errorf("%s: got (%v, %d), want the flags (1, 8000000)", name, scale, bitrate)
		}
	}
}

// A hand-edited or buggy file must not be able to request an unusable stream.
func TestSettingsIgnoreOutOfRangeValues(t *testing.T) {
	s := newSettings(t, `{
		"defaults": {"scale": 7, "bitrate": -5},
		"devices": {"x": {"scale": 0.1, "bitrate": 0}}
	}`)
	if scale, bitrate := s.lookup("x"); scale != 1.0 || bitrate != 8_000_000 {
		t.Errorf("got (%v, %d), want the flags (1, 8000000)", scale, bitrate)
	}
}

// The file is re-read on each lookup, which is what makes changes apply at the next
// connection without a server restart.
func TestSettingsAreReadOnEveryLookup(t *testing.T) {
	s := newSettings(t, `{"devices": {"x": {"scale": 0.5}}}`)
	if scale, _ := s.lookup("x"); scale != 0.5 {
		t.Fatalf("first lookup: got %v, want 0.5", scale)
	}
	if err := os.WriteFile(s.path, []byte(`{"devices": {"x": {"scale": 0.9}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if scale, _ := s.lookup("x"); scale != 0.9 {
		t.Fatalf("after edit: got %v, want 0.9", scale)
	}
}

// The touch mode is read from the file's defaults; anything missing or not a mode means
// the default (the cursor behaviour of every earlier version), never an error.
func TestSettingsTouchMode(t *testing.T) {
	for name, tc := range map[string]struct {
		contents string
		want     input.Mode
	}{
		"missing file":     {"", input.ModeCursor},
		"not set":          {`{"defaults": {"scale": 0.5}}`, input.ModeCursor},
		"background":       {`{"defaults": {"touchMode": "background"}}`, input.ModeBackground},
		"restore":          {`{"defaults": {"touchMode": "restore"}}`, input.ModeRestore},
		"unknown value":    {`{"defaults": {"touchMode": "telepathy"}}`, input.ModeCursor},
		"corrupt file":     {`{not json`, input.ModeCursor},
		"alongside others": {`{"defaults": {"scale": 0.5, "bitrate": 2000000, "touchMode": "background"}}`, input.ModeBackground},
	} {
		if got := newSettings(t, tc.contents).touchMode(); got != tc.want {
			t.Errorf("%s: touch mode %q, want %q", name, got, tc.want)
		}
	}
}
