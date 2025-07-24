package input

import (
	"github.com/go-vgo/robotgo"
	"github.com/veandco/go-sdl2/sdl"
)

// Event represents a simplified control event that will be encoded
// and sent to the scrcpy server.
type Event struct {
	Type   string
	Key    sdl.Keycode
	Button uint8
	X, Y   int32
}

// Capture polls SDL for keyboard/mouse events and converts them to Event.
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

// Example of sending a key via robotgo (used for OTG mode).
func SendKey(code string) {
	robotgo.KeyTap(code)
}
