package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"runtime/debug"
	"time"
)

// RtpPayload RTP 載荷結構體
// 用途：封裝要透過 RTP 傳輸的視訊資料，包含 NALU 單元、時間戳和存取單元標記
type RtpPayload struct {
	NALUs        [][]byte
	RTPTimestamp uint32
	IsAccessUnit bool // 標記這是否是一個完整的 AU (Access Unit)，用於設定 RTP Marker bit
}

// ★ 推送至 RTP channel，若壅塞則丟棄
// pushToRTPChannel 將 RTP 載荷推送到該設備的處理通道
// 用途：非阻塞式推送視訊幀到 RTP 通道，當通道滿時主動丟幀以降低延遲
func pushToRTPChannel(deviceFrameChannel chan RtpPayload, deviceIP string, payload RtpPayload) {
	select {
	case deviceFrameChannel <- payload:
		// 成功推入
	default:
		// Channel 滿了，代表 WebRTC 端處理不過來
		// 為了降低延遲，**主動丟棄**
		log.Printf("[RTP][%s] 寫入壅塞 (channel 滿)，丟棄 %d NALUs (isAU=%v)", deviceIP, len(payload.NALUs), payload.IsAccessUnit)
		evFramesDropped.Add(1)
	}
}

// ★ 清空 RTP channel (WebRTC 斷線時)
// clearFrameChannel 清空該設備的 RTP 幀處理通道
// 用途：當 WebRTC 連接斷開時清空通道中的所有待處理幀，防止記憶體累積
func clearFrameChannel(deviceFrameChannel chan RtpPayload) {
	for {
		select {
		case <-deviceFrameChannel:
			// 丟棄
		default:
			// channel 空了
			return
		}
	}
}

// ====== 工具：安全啟動 goroutine，避免 panic 默默死掉 ======
// goSafe 安全啟動 goroutine，捕獲 panic 並記錄錯誤
// 用途：防止 goroutine 中的 panic 導致整個程式崩潰，提供錯誤追蹤和日誌記錄
func goSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC][%s] %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// generateSessionID 生成唯一的 session ID
// 用途：為每個 WebRTC 連接創建唯一標識符
func generateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 如果隨機數生成失敗，使用時間戳作為後備
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
