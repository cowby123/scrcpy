package protocol

import (
	"bytes"
	"encoding/binary"
)

// BuildKeyEvent builds a keyboard control message packet.
func BuildKeyEvent(code uint32, down bool) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(0) // message type: key event
	var action uint8
	if down {
		action = 1
	}
	binary.Write(buf, binary.BigEndian, action)
	binary.Write(buf, binary.BigEndian, code)
	return buf.Bytes()
}

// BuildMouseEvent builds a mouse control message packet.
func BuildMouseEvent(x, y int32, button uint8, down bool) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(1) // message type: mouse event
	binary.Write(buf, binary.BigEndian, button)
	binary.Write(buf, binary.BigEndian, x)
	binary.Write(buf, binary.BigEndian, y)
	var action uint8
	if down {
		action = 1
	}
	binary.Write(buf, binary.BigEndian, action)
	return buf.Bytes()
}
