package main

import (
	"encoding/binary"
	"log"
	"math"
	"sync"
)

// touchEvent 觸控事件結構體
// 用途：定義從前端 WebRTC DataChannel 接收的觸控事件資料格式，包含座標、壓力、按鍵狀態等
type touchEvent struct {
	Type        string  `json:"type"` // "down" | "up" | "move" | "cancel"
	ID          uint64  `json:"id"`   // pointer id（前端的）
	X           int32   `json:"x"`
	Y           int32   `json:"y"`
	ScreenW     uint16  `json:"screenW"`     // 原生寬
	ScreenH     uint16  `json:"screenH"`     // 原生高
	Pressure    float64 `json:"pressure"`    // 0..1
	Buttons     uint32  `json:"buttons"`     // mouse buttons bitmask；touch 一律 0
	PointerType string  `json:"pointerType"` // "mouse" | "touch" | "pen"
	DeviceIP    string  `json:"deviceIP"`    // 設備 IP（新增）
}

// ====== ★ 觸控 pointer 映射（限制活躍 ≤ 10；ID 0 給 mouse/pen）======
const maxPointers = 10

var (
	touchMu            sync.Mutex
	pointerMu          sync.Mutex
	pointerButtons     = make(map[uint64]uint32)
	touchLocalByRemote = map[uint64]uint16{}
	touchRemoteByLocal [maxPointers]uint64
	touchSlotUsed      [maxPointers]bool
)

// getLocalSlot 根據遠端觸控點 ID 獲取本地槽位編號
// 用途：查找已分配給特定遠端觸控點的本地槽位，用於多點觸控映射
func getLocalSlot(remote uint64) (uint16, bool) {
	if s, ok := touchLocalByRemote[remote]; ok {
		return s, true
	}
	return 0, false
}

// allocLocalSlot 為遠端觸控點 ID 分配本地槽位編號
// 用途：為新的觸控點分配可用的本地槽位，支持最多 10 個同時觸控點
func allocLocalSlot(remote uint64) (uint16, bool) {
	if s, ok := touchLocalByRemote[remote]; ok {
		return s, true
	}
	for i := 0; i < maxPointers; i++ {
		if !touchSlotUsed[i] {
			touchSlotUsed[i] = true
			touchLocalByRemote[remote] = uint16(i)
			touchRemoteByLocal[i] = remote
			return uint16(i), true
		}
	}
	return 0, false
}

// freeLocalSlot 釋放遠端觸控點 ID 對應的本地槽位
// 用途：當觸控點結束（up/cancel）時釋放占用的槽位，供後續觸控點使用
func freeLocalSlot(remote uint64) {
	if s, ok := touchLocalByRemote[remote]; ok {
		delete(touchLocalByRemote, remote)
		idx := int(s)
		touchSlotUsed[idx] = false
		touchRemoteByLocal[idx] = 0
	}
}

// handleTouchEvent 處理前端發送的觸控事件
// 用途：將 WebRTC DataChannel 收到的觸控事件轉換為 scrcpy 控制指令並發送給 Android 設備
func handleTouchEvent(ev touchEvent) {
	defer func() {
		pointerMu.Lock()
		evPendingPointers.Set(int64(len(pointerButtons)))
		pointerMu.Unlock()
	}()

	// 查找對應的設備會話
	devicesMu.RLock()
	deviceSession, exists := deviceSessions[ev.DeviceIP]
	devicesMu.RUnlock()

	if !exists || deviceSession == nil || deviceSession.Session == nil || deviceSession.Session.ControlConn() == nil {
		log.Printf("[CTRL] 設備 %s 不可用或未連接", ev.DeviceIP)
		return
	}

	// 取映射用的畫面寬高（從設備會話取得，前端沒帶就用設備的視訊解析度）
	sw := ev.ScreenW
	sh := ev.ScreenH
	if sw == 0 || sh == 0 {
		devicesMu.RLock()
		sw, sh = deviceSession.VideoW, deviceSession.VideoH
		devicesMu.RUnlock()
	}

	// 夾住座標
	if ev.X < 0 {
		ev.X = 0
	}
	if ev.Y < 0 {
		ev.Y = 0
	}
	if sw > 0 && sh > 0 {
		if ev.X > int32(sw)-1 {
			ev.X = int32(sw) - 1
		}
		if ev.Y > int32(sh)-1 {
			ev.Y = int32(sh) - 1
		}
	}

	// 轉 action
	var action uint8
	switch ev.Type {
	case "down":
		action = 0 // AMOTION_EVENT_ACTION_DOWN
	case "up":
		action = 1 // AMOTION_EVENT_ACTION_UP
	case "move":
		action = 2 // AMOTION_EVENT_ACTION_MOVE
	case "cancel":
		action = 3 // AMOTION_EVENT_ACTION_CANCEL
	default:
		action = 2
	}

	// ★ 計算送出的 pointerID
	var pointerID uint64
	if ev.PointerType != "touch" {
		// mouse/pen → 永遠使用 0，且忽略 hover move（無按鍵）
		pointerID = 0
		if action == 2 /*move*/ && ev.Buttons == 0 {
			return
		}
	} else {
		// touch → 對 remote ID 映射到 1..10（slot 0..9 對應 1..10；0 保留給滑鼠/pen）
		touchMu.Lock()
		switch action {
		case 0: // down
			if s, ok := allocLocalSlot(ev.ID); ok {
				pointerID = uint64(s + 1) // 1..10
			} else {
				touchMu.Unlock()
				log.Printf("[CTRL][TOUCH] 丟棄 down（超過 %d 指） id=%d", maxPointers, ev.ID)
				return
			}
		case 1, 3: // up/cancel
			if s, ok := getLocalSlot(ev.ID); ok {
				pointerID = uint64(s + 1)
				freeLocalSlot(ev.ID)
			} else {
				touchMu.Unlock()
				return
			}
		default: // move
			if s, ok := getLocalSlot(ev.ID); ok {
				pointerID = uint64(s + 1)
			} else {
				touchMu.Unlock()
				return
			}
		}
		touchMu.Unlock()
	}

	// 計算 action_button / buttons 狀態
	var actionButton uint32
	pointerMu.Lock()
	prevButtons := pointerButtons[pointerID]
	nowButtons := ev.Buttons
	if ev.PointerType == "touch" {
		nowButtons = 0 // 觸控不帶 mouse buttons
	}
	switch action {
	case 0: // down
		actionButton = nowButtons &^ prevButtons
	case 1: // up
		actionButton = prevButtons &^ nowButtons
	default:
		actionButton = 0
	}
	if action == 1 /*up*/ || action == 3 /*cancel*/ {
		delete(pointerButtons, pointerID)
	} else {
		pointerButtons[pointerID] = nowButtons
	}
	pointerMu.Unlock()

	// 壓力（UP 事件強制 0） → u16 fixed-point（官方）
	var p uint16
	if action != 1 {
		f := ev.Pressure
		if f < 0 {
			f = 0
		} else if f > 1 {
			f = 1
		}
		if f == 1 {
			p = 0xffff
		} else {
			p = uint16(math.Round(f * 65535))
		}
	}

	// ====== 官方線路格式（總 32 bytes）======
	// [0]    : type = 2 (INJECT_TOUCH_EVENT)
	// [1]    : action (u8)
	// [2:10] : pointerId (i64)
	// [10:14]: x (i32)
	// [14:18]: y (i32)
	// [18:20]: screenW (u16)
	// [20:22]: screenH (u16)
	// [22:24]: pressure (u16 fixed-point)
	// [24:28]: actionButton (i32)
	// [28:32]: buttons (i32)
	buf := make([]byte, 32)
	buf[0] = 2
	buf[1] = action
	binary.BigEndian.PutUint64(buf[2:], pointerID)
	binary.BigEndian.PutUint32(buf[10:], uint32(ev.X))
	binary.BigEndian.PutUint32(buf[14:], uint32(ev.Y))
	binary.BigEndian.PutUint16(buf[18:], sw)
	binary.BigEndian.PutUint16(buf[20:], sh)
	binary.BigEndian.PutUint16(buf[22:], p)
	binary.BigEndian.PutUint32(buf[24:], actionButton)
	binary.BigEndian.PutUint32(buf[28:], nowButtons)

	// 像官方：事件到就直接寫 socket（不合併、不延遲）
	// ★ 修改：使用設備會話的 writeFull 方法
	if deviceSession.Session != nil {
		_ = deviceSession.Session.writeFull(buf, criticalWriteTimeout, true)
	}
}
