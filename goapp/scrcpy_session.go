package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/yourname/scrcpy-go/adb"
)

// ScrcpySession encapsulates the lifecycle of an adb-backed scrcpy server.
type ScrcpySession struct {
	opts          adb.Options
	device        *adb.Device
	conn          *adb.ServerConn
	control       io.ReadWriter
	controlMu     sync.Mutex
	lastCtrlRead  time.Time
	lastCtrlWrite time.Time
}

// NewScrcpySession returns a session object configured with adb options.
func NewScrcpySession(opts adb.Options) *ScrcpySession {
	return &ScrcpySession{opts: opts}
}

// StartScrcpyBoot creates, starts, and initializes a ScrcpySession.
func StartScrcpyBoot(opts adb.Options) (*ScrcpySession, *adb.ServerConn, error) {
	session := NewScrcpySession(opts)
	if err := session.Start(); err != nil {
		return nil, nil, err
	}
	conn := session.Conn()
	if conn == nil {
		return nil, nil, fmt.Errorf("scrcpy connection not established")
	}
	session.StartControlLoops()
	return session, conn, nil
}

// Start establishes the adb connection, launches scrcpy on device, and wires streams.
func (s *ScrcpySession) Start() error {
	dev, err := adb.NewDevice(s.opts)
	if err != nil {
		return fmt.Errorf("new device: %w", err)
	}
	port := dev.ScrcpyPort()
	if err := dev.Reverse("localabstract:scrcpy", fmt.Sprintf("tcp:%d", port)); err != nil {
		return fmt.Errorf("reverse: %w", err)
	}
	if err := dev.PushServer("./assets/scrcpy-server"); err != nil {
		return fmt.Errorf("push server: %w", err)
	}
	conn, err := dev.StartServer()
	if err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	s.device = dev
	s.conn = conn
	s.control = conn.Control
	s.lastCtrlRead = time.Now()
	return nil
}

// Close closes active scrcpy streams.
func (s *ScrcpySession) Close() {
	if s == nil {
		return
	}
	if s.conn != nil {
		if s.conn.VideoStream != nil {
			_ = s.conn.VideoStream.Close()
		}
		if s.conn.Control != nil {
			_ = s.conn.Control.Close()
		}
	}
}

// Conn exposes the underlying scrcpy server connections.
func (s *ScrcpySession) Conn() *adb.ServerConn {
	return s.conn
}

// ControlConn exposes the control channel for read/write checks.
func (s *ScrcpySession) ControlConn() io.ReadWriter {
	return s.control
}

// ScrcpyPort reports the effective reverse port.
func (s *ScrcpySession) ScrcpyPort() int {
	if s.device == nil {
		return 0
	}
	return s.device.ScrcpyPort()
}

// StartControlLoops begins background goroutines for control channel I/O.
func (s *ScrcpySession) StartControlLoops() {
	if s == nil || s.control == nil {
		return
	}
	goSafe("control-reader", func() {
		s.readDeviceMessages()
	})
	goSafe("control-health", func() {
		s.monitorControlHealth()
	})
}

func (s *ScrcpySession) monitorControlHealth() {
	t := time.NewTicker(controlHealthTick)
	defer t.Stop()
	for range t.C {
		if s.control == nil {
			continue
		}
		ms := time.Since(s.lastCtrlRead).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		evLastCtrlReadMsAgo.Set(ms)

		if time.Since(s.lastCtrlRead) > controlStaleAfter {
			s.sendGetClipboard(0) // copyKey=COPY_KEY_NONE
			evHeartbeatSent.Add(1)
		}
	}
}

func (s *ScrcpySession) readDeviceMessages() {
	if s.control == nil {
		return
	}
	r := s.control
	buf := make([]byte, 0, 4096)

	readU8 := func() (byte, error) {
		var b [1]byte
		_, err := io.ReadFull(r, b[:])
		return b[0], err
	}

	readU32BE := func() (uint32, error) {
		var b [4]byte
		_, err := io.ReadFull(r, b[:])
		return binary.BigEndian.Uint32(b[:]), err
	}

	for {
		typ, err := readU8()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if err == io.EOF {
				log.Println("[CTRL][READ] EOF")
			} else {
				log.Println("[CTRL][READ] error:", err)
			}
			evCtrlReadsErr.Add(1)
			return
		}

		switch typ {
		case deviceMsgTypeClipboard:
			// len + data
			n, err := readU32BE()
			if err != nil {
				log.Println("[CTRL][READ] clipboard len err:", err)
				evCtrlReadsErr.Add(1)
				return
			}
			if n > controlReadBufMax {
				log.Printf("[CTRL][READ] clipboard 太大: %d > %d，捨棄", n, controlReadBufMax)
				if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
					log.Println("[CTRL][READ] discard err:", err)
					evCtrlReadsErr.Add(1)
					return
				}
				continue
			}
			if cap(buf) < int(n) {
				buf = make([]byte, n)
			} else {
				buf = buf[:n]
			}
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				log.Println("[CTRL][READ] clipboard data err:", err)
				evCtrlReadsErr.Add(1)
				return
			}
			s.lastCtrlRead = time.Now()
			evCtrlReadsOK.Add(1)
			evCtrlReadClipboardB.Add(int64(n))
			log.Printf("[CTRL][READ] DeviceMessage.CLIPBOARD %dB: %q", n, trimString(string(buf[:n]), 200))
		default:
			s.lastCtrlRead = time.Now()
			evCtrlReadsOK.Add(1)
			log.Printf("[CTRL][READ] 未知 DeviceMessage type=%d，跳過內容", typ)
		}
	}
}

func (s *ScrcpySession) writeFull(b []byte, deadline time.Duration, setDeadline bool) error {
	if s.control == nil || len(b) == 0 {
		return nil
	}
	start := time.Now()
	s.controlMu.Lock()
	defer s.controlMu.Unlock()

	c := s.control
	if setDeadline {
		if conn, ok := c.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = conn.SetWriteDeadline(time.Now().Add(deadline))
		}
	}
	var total int
	for total < len(b) {
		n, err := c.Write(b[total:])
		if err != nil {
			evCtrlWritesErr.Add(1)
			return err
		}
		total += n
	}
	if setDeadline {
		if conn, ok := c.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = conn.SetWriteDeadline(time.Time{})
		}
	}
	elapsed := time.Since(start)
	evCtrlWritesOK.Add(1)
	evLastCtrlWriteMS.Set(elapsed.Milliseconds())
	s.lastCtrlWrite = time.Now()
	return nil
}

// RequestKeyframe sends TYPE_RESET_VIDEO to prompt a keyframe.
func (s *ScrcpySession) RequestKeyframe() {
	if s == nil || s.control == nil {
		return
	}
	if err := s.writeFull([]byte{controlMsgResetVideo}, controlWriteDefaultTimeout, true); err != nil {
		log.Println("[CTRL] send RESET_VIDEO 錯誤:", err)
	} else {
		log.Println("[CTRL] 已送出 RESET_VIDEO")
	}
}

func (s *ScrcpySession) sendGetClipboard(copyKey byte) {
	if s == nil || s.control == nil {
		return
	}
	if err := s.writeFull([]byte{controlMsgGetClipboard, copyKey}, controlWriteDefaultTimeout, true); err != nil {
		log.Println("[CTRL] send GET_CLIPBOARD 錯誤:", err)
	} else {
		log.Println("[CTRL] 已送出 GET_CLIPBOARD (heartbeat)")
	}
}
