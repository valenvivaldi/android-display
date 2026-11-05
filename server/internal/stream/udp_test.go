package stream

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestUDPStreamerSendAndReceive(t *testing.T) {
	receiverAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	receiverConn, err := net.ListenUDP("udp", receiverAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer receiverConn.Close()

	streamer, err := NewUDPStreamer(receiverConn.LocalAddr().String())
	if err != nil {
		t.Fatalf("failed to create streamer: %v", err)
	}
	defer streamer.Close()

	testData := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42} // fake SPS NAL
	err = streamer.SendFrame(testData, FrameTypeSPS, 1)
	if err != nil {
		t.Fatalf("send error: %v", err)
	}

	buf := make([]byte, 2048)
	receiverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := receiverConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("receive error: %v", err)
	}

	pkt, err := UnmarshalPacket(buf[:n])
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if pkt.Header.FrameType != FrameTypeSPS {
		t.Errorf("expected SPS frame type, got %d", pkt.Header.FrameType)
	}
	if !bytes.Equal(pkt.Payload, testData) {
		t.Error("payload mismatch")
	}
}

func TestUDPStreamerLargeFrameFragmentation(t *testing.T) {
	receiverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	receiverConn, _ := net.ListenUDP("udp", receiverAddr)
	defer receiverConn.Close()

	streamer, _ := NewUDPStreamer(receiverConn.LocalAddr().String())
	defer streamer.Close()

	largeData := bytes.Repeat([]byte{0xFF}, MaxPayloadSize*3)
	err := streamer.SendFrame(largeData, FrameTypeIDR, 1)
	if err != nil {
		t.Fatalf("send error: %v", err)
	}

	buf := make([]byte, 2048)
	var received []Packet

	for i := 0; i < 3; i++ {
		receiverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := receiverConn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("receive packet %d error: %v", i, err)
		}
		pkt, _ := UnmarshalPacket(buf[:n])
		received = append(received, *pkt)
	}

	if len(received) != 3 {
		t.Fatalf("expected 3 packets, got %d", len(received))
	}

	var reassembled []byte
	for _, pkt := range received {
		reassembled = append(reassembled, pkt.Payload...)
	}
	if !bytes.Equal(reassembled, largeData) {
		t.Error("reassembled data mismatch")
	}
}
