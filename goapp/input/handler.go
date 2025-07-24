// 處理鍵盤與滑鼠輸入事件
package input

import (
	"github.com/go-vgo/robotgo"
	"github.com/veandco/go-sdl2/sdl"
)

// Event 描述將來要傳送給 scrcpy 伺服器的控制事件
type Event struct {
	Type   string
	Key    sdl.Keycode
	Button uint8
	X, Y   int32
}

// Capture 從 SDL 取得鍵盤與滑鼠事件並轉換成 Event
func Capture() []Event {
	var events []Event
	for e := sdl.PollEvent(); e != nil; e = sdl.PollEvent() {
		switch ev := e.(type) {
		case *sdl.KeyboardEvent:
			if ev.Type == sdl.KEYDOWN || ev.Type == sdl.KEYUP {
				events = append(events, Event{Type: "key", Key: ev.Keysym.Sym})
			}
		case *sdl.MouseButtonEvent:
			events = append(events, Event{Type: "mouse", Button: ev.Button, X: ev.X, Y: ev.Y})
		}
	}
	return events
}

// 使用 robotgo 直接在主機端產生按鍵（OTG 模式範例）
func SendKey(code string) {
	robotgo.KeyTap(code)
}
