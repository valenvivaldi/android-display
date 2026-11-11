package h264

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The SPS VideoToolbox produced for the Moto E's 896x540 stream (Baseline, level 3.2,
// one reference frame, cropped to 540 rows, no VUI), captured from a real session.
const realSPS = "0000000127420020ab407008bf68"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parse returns the SPS fields of an Annex B SPS NAL.
func parse(t *testing.T, nal []byte) SPSInfo {
	t.Helper()
	prefix, ok := startCodeLen(nal)
	if !ok {
		t.Fatal("no start code")
	}
	info, _, err := parseSPS(unescape(nal[prefix+1:]))
	if err != nil {
		t.Fatalf("parseSPS: %v", err)
	}
	return info
}

func TestParseRealSPS(t *testing.T) {
	info := parse(t, mustHex(t, realSPS))
	if info.ProfileIdc != 66 || info.LevelIdc != 32 {
		t.Errorf("profile/level = %d/%d, want 66/32 (Baseline 3.2)", info.ProfileIdc, info.LevelIdc)
	}
	if info.WidthMbs != 56 || info.HeightMapUnits != 34 {
		t.Errorf("size = %dx%d macroblocks, want 56x34 (896x544, cropped to 540)", info.WidthMbs, info.HeightMapUnits)
	}
	if info.MaxNumRefFrames != 1 {
		t.Errorf("max_num_ref_frames = %d, want 1", info.MaxNumRefFrames)
	}
	if info.HasVUI {
		t.Error("the encoder's SPS has no VUI")
	}
}

// The point of the rewrite: the decoder is told it never has to hold frames back.
func TestLimitDecoderBufferingAddsRestriction(t *testing.T) {
	in := mustHex(t, realSPS)
	out := LimitDecoderBuffering(in)

	info := parse(t, out)
	if !info.HasVUI || !info.HasBitstreamRestriction {
		t.Fatalf("rewritten SPS has VUI=%v bitstream_restriction=%v, want both", info.HasVUI, info.HasBitstreamRestriction)
	}
	if info.MaxNumReorderFrames != 0 {
		t.Errorf("max_num_reorder_frames = %d, want 0", info.MaxNumReorderFrames)
	}
	if info.MaxDecFrameBuffering != 1 {
		t.Errorf("max_dec_frame_buffering = %d, want 1 (= max_num_ref_frames)", info.MaxDecFrameBuffering)
	}

	// Everything the decoder needs to decode the existing frames must be untouched.
	before := parse(t, in)
	if info.ProfileIdc != before.ProfileIdc || info.LevelIdc != before.LevelIdc ||
		info.WidthMbs != before.WidthMbs || info.HeightMapUnits != before.HeightMapUnits ||
		info.MaxNumRefFrames != before.MaxNumRefFrames {
		t.Errorf("rewrite changed the stream parameters: before %+v, after %+v", before, info)
	}
	if !bytes.HasPrefix(out, in[:5]) {
		t.Errorf("start code and NAL header changed: % x", out[:5])
	}
}

// Rewriting twice must not stack a second VUI on top of the first.
func TestLimitDecoderBufferingIsIdempotent(t *testing.T) {
	once := LimitDecoderBuffering(mustHex(t, realSPS))
	twice := LimitDecoderBuffering(once)
	if !bytes.Equal(once, twice) {
		t.Errorf("second rewrite changed the SPS:\n once  %x\n twice %x", once, twice)
	}
}

// Only a plain SPS is touched; anything else goes through as it came.
func TestLimitDecoderBufferingLeavesOtherNALsAlone(t *testing.T) {
	for name, nal := range map[string][]byte{
		"PPS":                  mustHex(t, "0000000128ce3c80"),
		"IDR slice":            mustHex(t, "0000000165b8000000"),
		"no start code":        mustHex(t, "27420020ab407008bf68"),
		"truncated SPS":        mustHex(t, "000000012742"),
		"empty":                nil,
		"start code only":      mustHex(t, "00000001"),
		"3-byte start, no hdr": mustHex(t, "000001"),
	} {
		if got := LimitDecoderBuffering(nal); !bytes.Equal(got, nal) {
			t.Errorf("%s: changed to %x, want it untouched", name, got)
		}
	}
}

// Emulation prevention: payload bytes 00 00 0x must be escaped on the wire and
// unescaped when reading, so the rewrite never produces a false start code.
func TestEscapeRoundTrip(t *testing.T) {
	raw := []byte{0x42, 0, 0, 0, 0, 1, 0, 0, 2, 0, 0, 3, 0, 0, 4, 9}
	esc := escape(raw)
	for i := 0; i+2 < len(esc); i++ {
		if esc[i] == 0 && esc[i+1] == 0 && esc[i+2] <= 2 {
			t.Fatalf("escaped data still contains 00 00 %02x at %d: % x", esc[i+2], i, esc)
		}
	}
	if got := unescape(esc); !bytes.Equal(got, raw) {
		t.Errorf("unescape(escape(x)) = % x, want % x", got, raw)
	}
}

func TestExpGolombRoundTrip(t *testing.T) {
	values := []uint32{0, 1, 2, 3, 4, 7, 8, 16, 55, 255, 256, 65535}
	w := &bitWriter{}
	for _, v := range values {
		w.ue(v)
	}
	w.alignWithZeros()
	r := &bitReader{data: w.bytes()}
	for _, want := range values {
		if got, err := r.ue(); err != nil || got != want {
			t.Fatalf("ue() = %d, %v; want %d", got, err, want)
		}
	}
}
