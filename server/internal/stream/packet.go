package stream

import (
	"encoding/binary"
	"errors"
)

const (
	// PacketHeaderSize is the fixed size of every packet header in bytes:
	// Sequence (4B) + Timestamp (8B) + FrameType (1B) + FragIndex (2B) + FragTotal (2B)
	PacketHeaderSize = 17

	// MaxPayloadSize is the maximum payload per UDP packet, sized to fit
	// within typical WiFi MTU (~1400 bytes) after subtracting the header.
	MaxPayloadSize = 1400 - PacketHeaderSize

	// Frame type constants for H.264/H.265 NAL unit classification.
	FrameTypeIDR byte = 0x01
	FrameTypeP   byte = 0x02
	FrameTypeB   byte = 0x03
	FrameTypeSPS byte = 0x10
	FrameTypePPS byte = 0x11
)

// PacketHeader is the 17-byte wire header prepended to every UDP packet.
type PacketHeader struct {
	Sequence  uint32 // Monotonically increasing packet counter
	Timestamp uint64 // Frame capture time (Unix microseconds, Mac clock)
	FrameType byte   // NAL unit type (IDR, P, B, SPS, PPS)
	FragIndex uint16 // Fragment index within this frame (0-based)
	FragTotal uint16 // Total number of fragments for this frame
}

// Packet is a single UDP datagram: header + payload bytes.
type Packet struct {
	Header  PacketHeader
	Payload []byte
}

// Marshal serializes the header into a PacketHeaderSize-byte slice
// using big-endian byte order for cross-platform compatibility.
func (h *PacketHeader) Marshal() []byte {
	buf := make([]byte, PacketHeaderSize)
	binary.BigEndian.PutUint32(buf[0:4], h.Sequence)
	binary.BigEndian.PutUint64(buf[4:12], h.Timestamp)
	buf[12] = h.FrameType
	binary.BigEndian.PutUint16(buf[13:15], h.FragIndex)
	binary.BigEndian.PutUint16(buf[15:17], h.FragTotal)
	return buf
}

// Unmarshal deserializes a PacketHeader from the given byte slice.
// Returns an error if the buffer is smaller than PacketHeaderSize.
func (h *PacketHeader) Unmarshal(buf []byte) error {
	if len(buf) < PacketHeaderSize {
		return errors.New("buffer too small for packet header")
	}
	h.Sequence = binary.BigEndian.Uint32(buf[0:4])
	h.Timestamp = binary.BigEndian.Uint64(buf[4:12])
	h.FrameType = buf[12]
	h.FragIndex = binary.BigEndian.Uint16(buf[13:15])
	h.FragTotal = binary.BigEndian.Uint16(buf[15:17])
	return nil
}

// MarshalPacket serializes the full packet (header + payload) into a
// single byte slice ready for UDP transmission.
// I7: Single allocation instead of Marshal() + append().
func (p *Packet) MarshalPacket() []byte {
	buf := make([]byte, PacketHeaderSize+len(p.Payload))
	binary.BigEndian.PutUint32(buf[0:4], p.Header.Sequence)
	binary.BigEndian.PutUint64(buf[4:12], p.Header.Timestamp)
	buf[12] = p.Header.FrameType
	binary.BigEndian.PutUint16(buf[13:15], p.Header.FragIndex)
	binary.BigEndian.PutUint16(buf[15:17], p.Header.FragTotal)
	copy(buf[PacketHeaderSize:], p.Payload)
	return buf
}

// UnmarshalPacket deserializes a full packet from raw UDP bytes.
// The returned Packet's Payload is a sub-slice of the input data.
func UnmarshalPacket(data []byte) (*Packet, error) {
	var hdr PacketHeader
	if err := hdr.Unmarshal(data); err != nil {
		return nil, err
	}
	return &Packet{
		Header:  hdr,
		Payload: data[PacketHeaderSize:],
	}, nil
}

// SplitIntoPackets fragments a NAL unit (or any payload) into one or more
// Packets, each with a payload no larger than MaxPayloadSize. All fragments
// share the same sequence number, timestamp, and frame type; they differ
// only in FragIndex. FragTotal is set to the total number of fragments.
func SplitIntoPackets(seq uint32, timestamp uint64, frameType byte, data []byte) []Packet {
	if len(data) <= MaxPayloadSize {
		return []Packet{{
			Header: PacketHeader{
				Sequence:  seq,
				Timestamp: timestamp,
				FrameType: frameType,
				FragIndex: 0,
				FragTotal: 1,
			},
			Payload: data,
		}}
	}

	total := (len(data) + MaxPayloadSize - 1) / MaxPayloadSize
	packets := make([]Packet, 0, total)

	for i := 0; i < total; i++ {
		start := i * MaxPayloadSize
		end := start + MaxPayloadSize
		if end > len(data) {
			end = len(data)
		}
		packets = append(packets, Packet{
			Header: PacketHeader{
				Sequence:  seq,
				Timestamp: timestamp,
				FrameType: frameType,
				FragIndex: uint16(i),
				FragTotal: uint16(total),
			},
			Payload: data[start:end],
		})
	}

	return packets
}
