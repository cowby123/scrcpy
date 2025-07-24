// 與 scrcpy 伺服器溝通所用的封包格式解析
package protocol

import (
	"encoding/binary"
	"io"
)

// Packet 表示從 scrcpy 伺服器收到的簡化封包
type Packet struct {
	Type uint8
	Size uint32
	Body []byte
}

// Decode 從指定的 reader 讀取並解析封包
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
