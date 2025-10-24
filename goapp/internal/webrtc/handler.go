package webrtc

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/cowby123/scrcpy-go/internal/device"
	"github.com/cowby123/scrcpy-go/internal/utils"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// Handler WebRTC 連接處理器
type Handler struct {
	deviceManager *device.Manager
	config        webrtc.Configuration
}

// NewHandler 創建 WebRTC 處理器
func NewHandler(manager *device.Manager, config webrtc.Configuration) *Handler {
	return &Handler{
		deviceManager: manager,
		config:        config,
	}
}

// HandleOffer 處理 WebRTC SDP offer 請求
func (h *Handler) HandleOffer(w http.ResponseWriter, r *http.Request) {
	// 獲取設備 IP
	deviceIP := r.URL.Query().Get("deviceIP")
	if deviceIP == "" {
		http.Error(w, "missing deviceIP parameter", http.StatusBadRequest)
		return
	}

	log.Printf("[WebRTC] 收到 Offer 請求，設備 IP: %s", deviceIP)

	// 檢查設備是否存在
	deviceSession, ok := h.deviceManager.GetDevice(deviceIP)
	if !ok {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	// 生成唯一的 session ID
	sessionID := utils.GenerateSessionID()

	// 解析 offer
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid offer", http.StatusBadRequest)
		return
	}

	// 創建 PeerConnection
	pc, err := webrtc.NewPeerConnection(h.config)
	if err != nil {
		http.Error(w, "create pc error", http.StatusInternalServerError)
		return
	}

	// 創建 video track
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"scrcpy",
	)
	if err != nil {
		http.Error(w, "create track error", http.StatusInternalServerError)
		return
	}

	// 添加 track 到 PC
	rtpSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		http.Error(w, "add track error", http.StatusInternalServerError)
		return
	}

	// 創建客戶端會話
	clientSession := &device.ClientSession{
		ID:         sessionID,
		DeviceIP:   deviceIP,
		PC:         pc,
		VideoTrack: videoTrack,
		Packetizer: rtp.NewPacketizer(
			1200,
			0,
			0,
			&codecs.H264Payloader{},
			rtp.NewRandomSequencer(),
			90000,
		),
		CreatedAt: time.Now(),
	}

	// 註冊到管理器
	h.deviceManager.AddClient(sessionID, clientSession)

	// 處理連接狀態變化
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[WebRTC][%s] 連接狀態: %s", sessionID, state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			h.deviceManager.RemoveClient(sessionID)
			pc.Close()
		}
	})

	// 處理 RTCP（PLI/FIR）
	utils.GoSafe(fmt.Sprintf("rtcp-%s", sessionID), func() {
		rtcpBuf := make([]byte, 1500)
		for {
			n, _, err := rtpSender.Read(rtcpBuf)
			if err != nil {
				return
			}
			pkts, err := rtcp.Unmarshal(rtcpBuf[:n])
			if err != nil {
				continue
			}
			for _, pkt := range pkts {
				switch pkt.(type) {
				case *rtcp.PictureLossIndication:
					log.Printf("[RTCP][%s][設備:%s] 收到 PLI → 請求關鍵幀", sessionID, deviceIP)
					h.requestKeyframe(deviceSession)
				case *rtcp.FullIntraRequest:
					log.Printf("[RTCP][%s][設備:%s] 收到 FIR → 請求關鍵幀", sessionID, deviceIP)
					h.requestKeyframe(deviceSession)
				}
			}
		}
	})

	// 設置 Remote SDP
	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, "set remote error", http.StatusInternalServerError)
		return
	}

	// 創建 Answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "answer error", http.StatusInternalServerError)
		return
	}

	// 設置 Local Description
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, "set local error", http.StatusInternalServerError)
		return
	}

	// 等待 ICE gathering 完成
	<-webrtc.GatheringCompletePromise(pc)

	// 為該設備設置關鍵幀請求
	h.requestKeyframe(deviceSession)

	// 回傳 Answer
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())
}

// requestKeyframe 請求關鍵幀
func (h *Handler) requestKeyframe(ds *device.DeviceSession) {
	if ds == nil {
		return
	}

	ds.StateMu.Lock()
	ds.NeedKeyframe = true
	ds.StateMu.Unlock()

	// 請求 Android 設備發送關鍵幀
	if session, ok := ds.Session.(interface{ RequestKeyframe() }); ok {
		session.RequestKeyframe()
	}
}
