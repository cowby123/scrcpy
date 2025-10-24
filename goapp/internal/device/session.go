package device

import (
	"sync"
	"time"

	"github.com/cowby123/scrcpy-go/adb"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// RtpPayload 封裝要透過 RTP 傳輸的視訊資料
type RtpPayload struct {
	NALUs        [][]byte
	RTPTimestamp uint32
	IsAccessUnit bool // 標記這是否是一個完整的 AU (Access Unit)
}

// ClientSession 客戶端會話結構
type ClientSession struct {
	ID         string // 唯一的 session ID（UUID）
	DeviceIP   string // 連接的設備 IP
	PC         *webrtc.PeerConnection
	VideoTrack *webrtc.TrackLocalStaticRTP
	Packetizer rtp.Packetizer
	CreatedAt  time.Time
}

// DeviceSession 設備會話結構
type DeviceSession struct {
	DeviceIP     string          // 設備 IP 地址（如 192.168.66.102:5555）
	Session      interface{}     // scrcpy 會話 (避免循環依賴，使用 interface{})
	Conn         *adb.ServerConn // 視訊和控制連接
	VideoW       uint16          // 視訊寬度
	VideoH       uint16          // 視訊高度
	CreatedAt    time.Time       // 創建時間
	FrameChannel chan RtpPayload // 該設備專用的 RTP 幀通道

	// 每個設備有自己的 SPS/PPS 參數（避免多設備互相干擾）
	LastSPS []byte       // 該設備最後的 SPS
	LastPPS []byte       // 該設備最後的 PPS
	StateMu sync.RWMutex // 保護 SPS/PPS 的讀寫

	// 每個設備有自己的時間戳映射和狀態（避免多設備時間戳錯亂）
	HavePTS0 bool   // 是否已初始化 PTS 基準
	PTS0     uint64 // PTS 基準值
	RTPTS0   uint32 // RTP 時間戳基準值

	NeedKeyframe  bool   // 是否需要關鍵幀
	FramesSinceKF int    // 距離上次關鍵幀的幀數
	AuSeq         uint64 // Access Unit 序號
}
