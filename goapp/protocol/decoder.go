package protocol

import (
	"encoding/binary"
	"io"
)

// Packet represents a simplified packet header from scrcpy server.
type Packet struct {
	Type uint8
	Size uint32
	Body []byte
}

// Decode reads a packet from the given reader.
func Decode(r io.Reader) (*Packet, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	p := &Packet{Type: header[0], Size: binary.BigEndian.Uint32(header[1:])}
	p.Body = make([]byte, p.Size)
	if _, err := io.ReadFull(r, p.Body); err != nil {
		return nil, err
	}
	return p, nil
}
