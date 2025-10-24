package video

import (
	"log"

	"github.com/cowby123/scrcpy-go/internal/device"
)

// RTPSender 負責將視訊幀打包並發送到 WebRTC
type RTPSender struct {
	deviceManager *device.Manager
}

// NewRTPSender 創建 RTP 發送器
func NewRTPSender(manager *device.Manager) *RTPSender {
	return &RTPSender{
		deviceManager: manager,
	}
}

// SendNALUAccessUnit 發送完整的 NALU 存取單元
func (s *RTPSender) SendNALUAccessUnit(deviceIP string, nalus [][]byte, ts uint32) {
	if len(nalus) == 0 {
		return
	}

	// 獲取連接到該設備的所有客戶端
	clients := s.deviceManager.GetClientsByDevice(deviceIP)

	for _, session := range clients {
		pk := session.Packetizer
		vt := session.VideoTrack
		if pk == nil || vt == nil {
			continue
		}

		for i, n := range nalus {
			if len(n) == 0 {
				continue
			}
			pkts := pk.Packetize(n, 0) // samples=0，手動覆寫 Timestamp
			for j, p := range pkts {
				p.Timestamp = ts
				p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1) // 最後一個封包設置 Marker bit
				if err := vt.WriteRTP(p); err != nil {
					log.Printf("[RTP][%s][設備:%s] write error: %v", session.ID, deviceIP, err)
				}
			}
		}
	}
}

// SendNALUs 發送單獨的 NALU 單元（如 SPS/PPS）
func (s *RTPSender) SendNALUs(deviceIP string, ts uint32, nalus ...[]byte) {
	if len(nalus) == 0 {
		return
	}

	clients := s.deviceManager.GetClientsByDevice(deviceIP)

	for _, session := range clients {
		pk := session.Packetizer
		vt := session.VideoTrack
		if pk == nil || vt == nil {
			continue
		}

		for _, n := range nalus {
			if len(n) == 0 {
				continue
			}
			pkts := pk.Packetize(n, 0)
			for _, p := range pkts {
				p.Timestamp = ts
				// 注意：參數集（SPS/PPS）不設置 Marker bit
				if err := vt.WriteRTP(p); err != nil {
					log.Printf("[RTP][%s][設備:%s] write error: %v", session.ID, deviceIP, err)
				}
			}
		}
	}
}

// PushToChannel 將 RTP 載荷推送到設備的處理通道（非阻塞）
func PushToChannel(deviceFrameChannel chan device.RtpPayload, deviceIP string, payload device.RtpPayload) {
	select {
	case deviceFrameChannel <- payload:
		// 成功推入
	default:
		// Channel 滿了，主動丟棄以降低延遲
		log.Printf("[RTP][%s] 寫入壅塞 (channel 滿)，丟棄 %d NALUs (isAU=%v)", deviceIP, len(payload.NALUs), payload.IsAccessUnit)
	}
}

// ClearChannel 清空設備的 RTP 幀處理通道
func ClearChannel(deviceFrameChannel chan device.RtpPayload) {
	for {
		select {
		case <-deviceFrameChannel:
			// 丟棄
		default:
			return
		}
	}
}

// RTPTSFromPTS 將 PTS（微秒）轉換為 RTP 時間戳（90kHz）
func RTPTSFromPTS(pts, pts0 uint64) uint32 {
	delta := pts - pts0
	// RTP 時鐘頻率為 90kHz
	// 1 微秒 = 0.09 RTP ticks
	return uint32((delta * 9) / 100)
}
