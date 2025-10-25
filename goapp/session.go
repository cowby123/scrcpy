package main

import (
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/yourname/scrcpy-go/adb"
)

// ClientSession 客戶端會話結構
// 用途：管理每個 WebRTC 客戶端的連接狀態和資源
type ClientSession struct {
	ID         string // 唯一的 session ID（UUID）
	DeviceIP   string // 連接的設備 IP
	PC         *webrtc.PeerConnection
	VideoTrack *webrtc.TrackLocalStaticRTP
	Packetizer rtp.Packetizer
	CreatedAt  time.Time
}

// DeviceSession 設備會話結構
// 用途：管理每個 Android 設備的 scrcpy 連接和視訊流
type DeviceSession struct {
	DeviceIP     string          // 設備 IP 地址（如 192.168.66.102:5555）
	Session      *ScrcpySession  // scrcpy 會話
	Conn         *adb.ServerConn // 視訊和控制連接
	VideoW       uint16          // 視訊寬度
	VideoH       uint16          // 視訊高度
	CreatedAt    time.Time       // 創建時間
	FrameChannel chan RtpPayload // 該設備專用的 RTP 幀通道

	// ★ 每個設備有自己的 SPS/PPS 參數（避免多設備互相干擾）
	LastSPS []byte       // 該設備最後的 SPS
	LastPPS []byte       // 該設備最後的 PPS
	StateMu sync.RWMutex // 保護 SPS/PPS 的讀寫

	// ★ 每個設備有自己的時間戳映射和狀態（避免多設備時間戳錯亂）
	HavePTS0 bool   // 是否已初始化 PTS 基準
	PTS0     uint64 // PTS 基準值
	RTPTS0   uint32 // RTP 時間戳基準值

	NeedKeyframe  bool   // 是否需要關鍵幀
	FramesSinceKF int    // 距離上次關鍵幀的幀數
	AuSeq         uint64 // Access Unit 序號
}

// === 全域設備和客戶端管理 ===
var (
	// ★ 支援多客戶端：改用 map 管理多個 WebRTC 連接
	clientsMu sync.RWMutex
	clients   = make(map[string]*ClientSession) // clientID -> session

	// ★ 支援多設備：管理多個 Android 設備連接
	devicesMu      sync.RWMutex
	deviceSessions = make(map[string]*DeviceSession) // deviceIP -> device session
)

// getDeviceSession 根據設備 IP 獲取設備會話
// 用途：從全域 map 中查找指定設備的連接會話
func getDeviceSession(deviceIP string) (*DeviceSession, bool) {
	devicesMu.RLock()
	defer devicesMu.RUnlock()
	ds, ok := deviceSessions[deviceIP]
	return ds, ok
}

// listDevices 列出所有已連接的設備
// 用途：返回當前所有活躍設備的 IP 列表
func listDevices() []string {
	devicesMu.RLock()
	defer devicesMu.RUnlock()
	devices := make([]string, 0, len(deviceSessions))
	for ip := range deviceSessions {
		devices = append(devices, ip)
	}
	return devices
}
