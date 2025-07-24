// 建立傳送至 scrcpy 伺服器的控制訊息封包
package protocol

import (
	"bytes"
	"encoding/binary"
)

// BuildKeyEvent 建立鍵盤事件封包
func BuildKeyEvent(code uint32, down bool) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(0) // 封包類型：鍵盤事件
	var action uint8
	if down {
		action = 1
	}
	binary.Write(buf, binary.BigEndian, action)
	binary.Write(buf, binary.BigEndian, code)
	return buf.Bytes()
}

// BuildMouseEvent 建立滑鼠事件封包
func BuildMouseEvent(x, y int32, button uint8, down bool) []byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(1) // 封包類型：滑鼠事件
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
