package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/luke/android-mac/server/internal/input"
)

// settingsFile is device-settings.json, written by the Mac app and read by the
// server on every handshake, so a change applies the next time a phone connects
// without restarting the server.
//
//	{
//	  "defaults": { "scale": 0.75, "bitrate": 4000000, "touchMode": "background" },
//	  "devices": {
//	    "<device id>": { "name": "moto g(9) plus", "scale": 1.0, "bitrate": 8000000 }
//	  }
//	}
//
// A missing or zero scale/bitrate means "not set": the next level applies. touchMode
// ("cursor", "restore" or "background", see input.Mode) is global, not per phone.
type settingsFile struct {
	Defaults defaultSettings        `json:"defaults"`
	Devices  map[string]deviceEntry `json:"devices"`
}

type defaultSettings struct {
	streamSettings
	TouchMode string `json:"touchMode"`
}

type streamSettings struct {
	Scale   float64 `json:"scale"`
	Bitrate int     `json:"bitrate"`
}

// deviceEntry holds one phone's own settings. The other fields the Mac app stores
// (name, native size) are only for its display and are ignored here.
type deviceEntry struct {
	Name string `json:"name"`
	streamSettings
}

// settingsStore resolves the stream settings for a phone. Precedence, highest first:
// the phone's own entry, the file's defaults, the -scale/-bitrate flags.
type settingsStore struct {
	path        string
	flagScale   float64
	flagBitrate int
}

func defaultSettingsStore(flagScale float64, flagBitrate int) (*settingsStore, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &settingsStore{
		path:        filepath.Join(dir, "AndroidDisplay", "device-settings.json"),
		flagScale:   flagScale,
		flagBitrate: flagBitrate,
	}, nil
}

func validScale(v float64) bool { return v >= 0.25 && v <= 1 }
func validBitrate(v int) bool   { return v > 0 }

// read loads the file. A missing or unreadable file gives empty settings, so a bad
// file never stops a phone from connecting.
func (s *settingsStore) read() settingsFile {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return settingsFile{}
	}
	if err != nil {
		log.Printf("read device settings: %v", err)
		return settingsFile{}
	}
	var f settingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("device settings ignored (invalid JSON): %v", err)
		return settingsFile{}
	}
	return f
}

// lookup returns the resolution scale and bitrate to stream to the given phone.
// Out-of-range values in the file are ignored rather than trusted.
func (s *settingsStore) lookup(deviceID string) (scale float64, bitrate int) {
	scale, bitrate = s.flagScale, s.flagBitrate
	f := s.read()

	apply := func(v streamSettings) {
		if validScale(v.Scale) {
			scale = v.Scale
		}
		if validBitrate(v.Bitrate) {
			bitrate = v.Bitrate
		}
	}
	apply(f.Defaults.streamSettings)
	if d, ok := f.Devices[deviceID]; ok {
		apply(d.streamSettings)
	}
	return scale, bitrate
}

// touchMode returns how touches from a phone reach macOS. An unknown value in the file
// is ignored (with a log line) rather than trusted.
func (s *settingsStore) touchMode() input.Mode {
	value := s.read().Defaults.TouchMode
	mode, ok := input.ParseMode(value)
	if !ok {
		log.Printf("touch mode %q in the settings is not valid, using %q", value, mode)
	}
	return mode
}
