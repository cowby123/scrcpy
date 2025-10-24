package input

import (
	"encoding/binary"
	"log"
	"sync"
)

const maxPointers = 10

var (
	touchMu            sync.Mutex
	touchLocalByRemote = map[uint64]uint16{}
	touchRemoteByLocal [maxPointers]uint64
	touchSlotUsed      [maxPointers]bool
	pointerMu          sync.Mutex
	pointerButtons     = make(map[uint64]uint32)
)

// GetLocalSlot 根據遠端觸控點 ID 獲取本地槽位編號
func GetLocalSlot(remote uint64) (uint16, bool) {
	if s, ok := touchLocalByRemote[remote]; ok {
		return s, true
	}
	return 0, false
}

// AllocLocalSlot 分配本地槽位給遠端觸控點
func AllocLocalSlot(remote uint64) (uint16, bool) {
	for i := uint16(0); i < maxPointers; i++ {
		if !touchSlotUsed[i] {
			touchSlotUsed[i] = true
			touchLocalByRemote[remote] = i
			touchRemoteByLocal[i] = remote
			return i, true
		}
	}
	return 0, false
}

// FreeLocalSlot 釋放本地槽位
func FreeLocalSlot(remote uint64) {
	if s, ok := touchLocalByRemote[remote]; ok {
		touchSlotUsed[s] = false
		delete(touchLocalByRemote, remote)
		touchRemoteByLocal[s] = 0
	}
}

// TouchEvent 觸控事件
type TouchEvent struct {
	// 支持兩種字段名稱
	Action    string  `json:"action"`    // 後端 HTTP 使用
	Type      string  `json:"type"`      // 前端 DataChannel 使用
	PointerID uint64  `json:"pointerId"` // 後端 HTTP 使用
	ID        uint64  `json:"id"`        // 前端 DataChannel 使用
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Pressure  float64 `json:"pressure"`
	ScreenW   uint16  `json:"screenW"`
	ScreenH   uint16  `json:"screenH"`
	DeviceIP  string  `json:"deviceIP"`
}

// GetAction 獲取動作（兼容兩種命名）
func (e *TouchEvent) GetAction() string {
	if e.Action != "" {
		return e.Action
	}
	return e.Type
}

// GetPointerID 獲取指針 ID（兼容兩種命名）
func (e *TouchEvent) GetPointerID() uint64 {
	if e.PointerID != 0 {
		return e.PointerID
	}
	return e.ID
}

// HandleTouchEvent 處理觸控事件並編碼為 scrcpy 協議
func HandleTouchEvent(ev TouchEvent, controlWriter func([]byte) error) {
	touchMu.Lock()
	defer touchMu.Unlock()

	action := ev.GetAction()
	pointerID := ev.GetPointerID()

	var local uint16
	var found bool

	switch action {
	case "down":
		local, found = AllocLocalSlot(pointerID)
		if !found {
			log.Printf("[TOUCH] 無法分配槽位給 pointer=%d", pointerID)
			return
		}
		log.Printf("[TOUCH] down pointer=%d -> local=%d", pointerID, local)

	case "move":
		local, found = GetLocalSlot(pointerID)
		if !found {
			return
		}

	case "up":
		local, found = GetLocalSlot(pointerID)
		if !found {
			return
		}
		defer FreeLocalSlot(pointerID)
		log.Printf("[TOUCH] up pointer=%d (local=%d)", pointerID, local)
	}

	// 編碼觸控事件（31 bytes）
	payload := make([]byte, 31)

	// action: down=0, up=1, move=2
	var actionByte byte
	switch action {
	case "down":
		actionByte = 0
	case "up":
		actionByte = 1
	case "move":
		actionByte = 2
	}
	payload[0] = actionByte

	// pointerId (8 bytes)
	binary.BigEndian.PutUint64(payload[1:9], uint64(local))

	// position (4 bytes x, 4 bytes y) - 前端發送的是像素值，直接使用
	posX := uint32(ev.X)
	posY := uint32(ev.Y)
	binary.BigEndian.PutUint32(payload[9:13], posX)
	binary.BigEndian.PutUint32(payload[13:17], posY)

	// screenW, screenH (2 bytes each)
	binary.BigEndian.PutUint16(payload[17:19], ev.ScreenW)
	binary.BigEndian.PutUint16(payload[19:21], ev.ScreenH)

	// pressure (2 bytes, 0xFFFF = max)
	pressure := uint16(ev.Pressure * 0xFFFF)
	binary.BigEndian.PutUint16(payload[21:23], pressure)

	// buttons (4 bytes) - 保留 0

	// 發送：type(1) + payload(31) = 32 bytes
	msg := make([]byte, 32)
	msg[0] = 2 // INJECT_TOUCH_EVENT
	copy(msg[1:], payload)

	if err := controlWriter(msg); err != nil {
		log.Printf("[TOUCH] 寫入失敗: %v", err)
	}
}
