package stream

import (
	"net"
	"sync/atomic"
)

// UDPStreamer sends H.264 NAL units over UDP, automatically fragmenting
// large frames into multiple packets using the wire format defined in
// packet.go. Each call to SendFrame increments a monotonic sequence
// number shared across all fragments of that frame.
type UDPStreamer struct {
	conn     *net.UDPConn
	sequence atomic.Uint32
}

// NewUDPStreamer creates a UDPStreamer that sends packets to targetAddr.
// targetAddr must be a host:port string (e.g. "192.168.1.5:9000").
func NewUDPStreamer(targetAddr string) (*UDPStreamer, error) {
	addr, err := net.ResolveUDPAddr("udp", targetAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}
	return &UDPStreamer{conn: conn}, nil
}

// SendFrame serializes a NAL unit into one or more UDP packets and
// transmits them to the configured target address. Large NAL units are
// automatically split into MaxPayloadSize-byte fragments.
func (s *UDPStreamer) SendFrame(nalUnit []byte, frameType byte, captureMicros uint64) error {
	seq := s.sequence.Add(1)
	packets := SplitIntoPackets(seq, captureMicros, frameType, nalUnit)

	for _, pkt := range packets {
		data := pkt.MarshalPacket()
		if _, err := s.conn.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the underlying UDP connection.
func (s *UDPStreamer) Close() error {
	return s.conn.Close()
}

// LocalPort returns the local port number the streamer is sending from.
func (s *UDPStreamer) LocalPort() int {
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}
