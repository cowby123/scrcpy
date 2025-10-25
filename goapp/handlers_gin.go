package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/yourname/scrcpy-go/adb"
)

// handleDevicesGin 處理設備列表請求 (Gin 版本)
func handleDevicesGin(c *gin.Context) {
	// 使用 ADB 列出設備
	adbDevices, err := adb.ListDevices(adb.Options{
		ServerHost: "127.0.0.1",
		ServerPort: 5037,
	})

	if err != nil {
		log.Printf("[ADB] 列出設備失敗: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("列出設備失敗: %v", err),
		})
		return
	}

	// 構建回應，包含設備狀態和連接資訊
	devices := make([]map[string]interface{}, 0, len(adbDevices))
	for _, dev := range adbDevices {
		deviceInfo := map[string]interface{}{
			"ip":    dev.Serial,
			"state": dev.State,
		}

		// 如果設備已連接，添加視訊資訊
		devicesMu.RLock()
		if ds, ok := deviceSessions[dev.Serial]; ok {
			deviceInfo["connected"] = true
			deviceInfo["videoW"] = ds.VideoW
			deviceInfo["videoH"] = ds.VideoH
			deviceInfo["createdAt"] = ds.CreatedAt.Format(time.RFC3339)
		} else {
			deviceInfo["connected"] = false
		}
		devicesMu.RUnlock()

		devices = append(devices, deviceInfo)
	}

	c.JSON(http.StatusOK, gin.H{
		"devices": devices,
		"count":   len(devices),
	})
}

// handleOfferGin 處理 WebRTC offer 請求 (Gin 版本)
func handleOfferGin(c *gin.Context) {
	// 從 URL 參數取得客戶端 ID（設備 IP）
	deviceIP := c.Query("id")
	if deviceIP == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "缺少設備 ID (id 參數)",
		})
		return
	}

	log.Printf("[WebRTC] 收到 Offer 請求，設備 IP: %s", deviceIP)

	// 查找對應的設備會話
	devicesMu.RLock()
	deviceSession, exists := deviceSessions[deviceIP]
	devicesMu.RUnlock()

	if !exists {
		log.Printf("[WebRTC] 設備 %s 未連接", deviceIP)
		c.JSON(http.StatusNotFound, gin.H{
			"error": fmt.Sprintf("設備 %s 未連接", deviceIP),
		})
		return
	}

	var offer webrtc.SessionDescription
	if err := c.ShouldBindJSON(&offer); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid offer",
		})
		return
	}

	// 媒體編解碼：H.264 packetization-mode=1
	m := webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}},
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "register codec error",
		})
		return
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "pc error",
		})
		return
	}

	// 建立 H.264 RTP Track
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "scrcpy",
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "track error",
		})
		return
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "add track error",
		})
		return
	}

	// 生成唯一的 session ID
	sessionID := generateSessionID()

	// 建立客戶端會話
	session := &ClientSession{
		ID:         sessionID,
		DeviceIP:   deviceIP,
		PC:         pc,
		VideoTrack: track,
		Packetizer: rtp.NewPacketizer(
			1200,
			96,
			uint32(time.Now().UnixNano()),
			&codecs.H264Payloader{},
			rtp.NewRandomSequencer(),
			90000,
		),
		CreatedAt: time.Now(),
	}

	// 註冊到全域客戶端列表
	clientsMu.Lock()
	clients[sessionID] = session
	clientCount := len(clients)
	clientsMu.Unlock()
	evActivePeer.Set(int64(clientCount))

	log.Printf("[WebRTC][%s] 客戶端已註冊（設備: %s），當前連接數: %d", sessionID, deviceIP, clientCount)

	// 處理 RTCP
	goSafe("rtcp-reader", func() {
		rtcpBuf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(rtcpBuf)
			if err != nil {
				return
			}
			pkts, err := rtcp.Unmarshal(rtcpBuf[:n])
			if err != nil {
				continue
			}
			for _, pkt := range pkts {
				switch p := pkt.(type) {
				case *rtcp.PictureLossIndication:
					log.Printf("[RTCP][%s][設備:%s] 收到 PLI", sessionID, deviceIP)
					deviceSession.StateMu.Lock()
					deviceSession.NeedKeyframe = true
					deviceSession.StateMu.Unlock()
					if deviceSession.Session != nil {
						deviceSession.Session.RequestKeyframe()
					}
					stateMu.Lock()
					lastPLI = time.Now()
					pliCount++
					stateMu.Unlock()
					evRTCP_PLI.Add(1)
					evPLICount.Set(int64(pliCount))
					evKeyframeRequests.Add(1)
				case *rtcp.FullIntraRequest:
					log.Printf("[RTCP][%s][設備:%s] 收到 FIR (SenderSSRC=%d, MediaSSRC=%d)", sessionID, deviceIP, p.SenderSSRC, p.MediaSSRC)
					deviceSession.StateMu.Lock()
					deviceSession.NeedKeyframe = true
					deviceSession.StateMu.Unlock()
					if deviceSession.Session != nil {
						deviceSession.Session.RequestKeyframe()
					}
					stateMu.Lock()
					lastPLI = time.Now()
					pliCount++
					stateMu.Unlock()
					evRTCP_FIR.Add(1)
					evPLICount.Set(int64(pliCount))
					evKeyframeRequests.Add(1)
				}
			}
		}
	})

	// 處理 DataChannel
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("[RTC][%s][設備:%s] DataChannel: %s", sessionID, deviceIP, dc.Label())
		dc.OnOpen(func() {
			log.Printf("[RTC][%s][設備:%s] DC open: %s", sessionID, deviceIP, dc.Label())
		})
		dc.OnClose(func() {
			log.Printf("[RTC][%s][設備:%s] DC close: %s", sessionID, deviceIP, dc.Label())
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Printf("[RTC][%s][設備:%s][%s] 收到訊息: %s", sessionID, deviceIP, dc.Label(), string(msg.Data))
			var ev touchEvent
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				log.Printf("[RTC][%s][設備:%s] JSON 解析失敗: %v", sessionID, deviceIP, err)
				return
			}
			ev.DeviceIP = deviceIP
			handleTouchEvent(ev)
		})
	})

	// 處理連接狀態變化
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[RTC][%s][設備:%s] 連接狀態: %s", sessionID, deviceIP, state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			clientsMu.Lock()
			delete(clients, sessionID)
			remaining := len(clients)
			clientsMu.Unlock()
			evActivePeer.Set(int64(remaining))
			log.Printf("[RTC][%s][設備:%s] 客戶端已移除，剩餘: %d", sessionID, deviceIP, remaining)
		}
	})

	// 設置遠端描述
	if err := pc.SetRemoteDescription(offer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "set remote error",
		})
		return
	}

	// 創建 answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "answer error",
		})
		return
	}

	// 設置本地描述
	if err := pc.SetLocalDescription(answer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "set local error",
		})
		return
	}

	// 等待 ICE gathering 完成
	<-webrtc.GatheringCompletePromise(pc)

	// 設置需要關鍵幀
	deviceSession.StateMu.Lock()
	deviceSession.NeedKeyframe = true
	deviceSession.StateMu.Unlock()
	if deviceSession.Session != nil {
		deviceSession.Session.RequestKeyframe()
	}
	evKeyframeRequests.Add(1)

	// 返回 answer
	c.JSON(http.StatusOK, pc.LocalDescription())
}
