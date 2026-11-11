package adb

import (
	"reflect"
	"testing"
)

// Only authorized devices can take `adb reverse`; unauthorized/offline ones and
// adb's own chatter must be ignored, and several devices must all be returned.
func TestParseDevices(t *testing.T) {
	out := "* daemon not running; starting now at tcp:5037\n" +
		"* daemon started successfully\n" +
		"List of devices attached\n" +
		"ZY227TN79V\tdevice usb:34881536X product:odessa_retail model:moto_g_9__plus device:odessa transport_id:3\n" +
		"ZX1B622LD8\tunauthorized usb:34877440X transport_id:4\n" +
		"emulator-5554\toffline\n" +
		"R58M12345AB\tdevice\n" +
		"\n"
	got := parseDevices(out)
	want := []string{"ZY227TN79V", "R58M12345AB"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDevices = %v, want %v", got, want)
	}
}

func TestParseDevicesNone(t *testing.T) {
	if got := parseDevices("List of devices attached\n\n"); len(got) != 0 {
		t.Fatalf("expected no devices, got %v", got)
	}
}
