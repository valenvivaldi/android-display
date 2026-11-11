package stream

import (
	"bytes"
	"testing"
)

func TestPacketHeaderSerialize(t *testing.T) {
	hdr := PacketHeader{
		Sequence:  42,
		Timestamp: 1234567890,
		FrameType: FrameTypeIDR,
		FragIndex: 0,
		FragTotal: 1,
	}

	buf := hdr.Marshal()
	if len(buf) != PacketHeaderSize {
		t.Fatalf("expected header size %d, got %d", PacketHeaderSize, len(buf))
	}

	var decoded PacketHeader
	err := decoded.Unmarshal(buf)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Sequence != hdr.Sequence {
		t.Errorf("sequence: got %d, want %d", decoded.Sequence, hdr.Sequence)
	}
	if decoded.Timestamp != hdr.Timestamp {
		t.Errorf("timestamp: got %d, want %d", decoded.Timestamp, hdr.Timestamp)
	}
	if decoded.FrameType != hdr.FrameType {
		t.Errorf("frameType: got %d, want %d", decoded.FrameType, hdr.FrameType)
	}
}

func TestSplitNALUnits(t *testing.T) {
	// Create test data larger than MaxPayloadSize
	data := bytes.Repeat([]byte{0xAB}, MaxPayloadSize*2+100)

	packets := SplitIntoPackets(1, 999, FrameTypeIDR, data)

	if len(packets) != 3 {
		t.Fatalf("expected 3 packets, got %d", len(packets))
	}

	// Check fragment indices
	for i, pkt := range packets {
		if pkt.Header.FragIndex != uint16(i) {
			t.Errorf("packet %d: fragIndex got %d, want %d", i, pkt.Header.FragIndex, i)
		}
		if pkt.Header.FragTotal != 3 {
			t.Errorf("packet %d: fragTotal got %d, want 3", i, pkt.Header.FragTotal)
		}
	}

	// Reassemble and verify
	var reassembled []byte
	for _, pkt := range packets {
		reassembled = append(reassembled, pkt.Payload...)
	}
	if !bytes.Equal(reassembled, data) {
		t.Error("reassembled data does not match original")
	}
}

func TestSmallPayloadSinglePacket(t *testing.T) {
	data := []byte{0x01, 0x02, 0x03}
	packets := SplitIntoPackets(1, 999, FrameTypeP, data)

	if len(packets) != 1 {
		t.Fatalf("expected 1 packet, got %d", len(packets))
	}
	if packets[0].Header.FragIndex != 0 || packets[0].Header.FragTotal != 1 {
		t.Error("single packet should have fragIndex=0, fragTotal=1")
	}
}
