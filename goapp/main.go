package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/yourname/scrcpy-go/adb"
)

const controlMsgResetVideo = 17

// === 全域狀態 ===
var (
	videoTrack   *webrtc.TrackLocalStaticRTP
	peerConn     *webrtc.PeerConnection
	packetizer   rtp.Packetizer
	rtpTS        uint32 // 90kHz 時基
	needKeyframe bool   // 新用戶/PLI 時需要 SPS/PPS + IDR
	lastSPS      []byte
	lastPPS      []byte
	stateMu      sync.RWMutex

	startTime   time.Time // 只用來印速率統計
	controlConn io.Writer // 與 Android 控制通道
)

func main() {
	// 靜態檔案伺服器 (提供 web/index.html)
	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)

	// WebRTC SDP handler
	http.HandleFunc("/offer", handleOffer)
	go func() {
		log.Println("HTTP 伺服器: http://localhost:8080/  (SDP: POST /offer)")
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	// 連線到第一台 Android 裝置
	dev, err := adb.NewDevice("")
	if err != nil {
		log.Fatal(err)
	}
	if err := dev.Reverse("localabstract:scrcpy", "tcp:27183"); err != nil {
		log.Fatal("reverse:", err)
	}
	if err := dev.PushServer("./assets/scrcpy-server"); err != nil {
		log.Fatal("push server:", err)
	}
	conn, err := dev.StartServer()
	if err != nil {
		log.Fatal("start server:", err)
	}
	defer conn.VideoStream.Close()
	defer conn.Control.Close()
	controlConn = conn.Control

	// 建立輸出檔案 (debug 用)
	outFile, err := os.Create("output.h264")
	if err != nil {
		log.Fatal("create output file:", err)
	}
	defer outFile.Close()

	log.Println("開始接收視訊串流，並輸出到 output.h264")

	// 跳過裝置名稱 (64 bytes)
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		log.Fatal("read device name:", err)
	}
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	log.Printf("裝置名稱: %s\n", deviceName)

	// 視訊標頭 (12 bytes)
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Fatal("read video header:", err)
	}
	codec := string(vHeader[:4])
	width := binary.BigEndian.Uint32(vHeader[4:8])
	height := binary.BigEndian.Uint32(vHeader[8:12])
	log.Printf("編碼格式: %s, 解析度: %dx%d\n", codec, width, height)

	// 接收幀迴圈
	meta := make([]byte, 12) // scrcpy: [pts(8)] + [size(4)]
	startTime = time.Now()
	var frameCount int
	var totalBytes int64

	for {
		// frame meta
		if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
			log.Println("read frame meta:", err)
			break
		}
		frameSize := binary.BigEndian.Uint32(meta[8:12])

		// frame data
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
			log.Println("read frame:", err)
			break
		}

		// 存檔 (debug)
		if _, err := outFile.Write(frame); err != nil {
			log.Println("write file error:", err)
			break
		}

		// 解析 Annex-B → NALUs，並快取 SPS/PPS、偵測是否含 IDR
		nalus := splitAnnexBNALUs(frame)
		idrInThisAU := false
		for _, n := range nalus {
			switch naluType(n) {
			case 7: // SPS
				stateMu.Lock()
				lastSPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 8: // PPS
				stateMu.Lock()
				lastPPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 5: // IDR
				idrInThisAU = true
			}
		}

		// 推進 WebRTC
		stateMu.RLock()
		vt := videoTrack
		pk := packetizer
		waitKF := needKeyframe
		stateMu.RUnlock()

		if vt != nil && pk != nil {
			if waitKF {
				// 先送參數集 (不前進 timestamp)
				stateMu.RLock()
				sps := lastSPS
				pps := lastPPS
				stateMu.RUnlock()
				if len(sps) > 0 && len(pps) > 0 {
					sendNALUs(sps, pps) // 同一 timestamp
				}
				// 等 IDR 才開流
				if !idrInThisAU {
					goto stats // 這幀不送
				}
				stateMu.Lock()
				needKeyframe = false
				stateMu.Unlock()
				sendNALUAccessUnit(nalus) // 送這個含 IDR 的 AU，送完 +TS
			} else {
				// 正常持續送
				sendNALUAccessUnit(nalus)
			}
		}

	stats:
		frameCount++
		totalBytes += int64(frameSize)
		if frameCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			bytesPerSecond := float64(totalBytes) / elapsed
			log.Printf("接收影格: %d, 速率: %.2f MB/s\n",
				frameCount, bytesPerSecond/(1024*1024))
		}
	}
}

// === WebRTC: /offer handler ===
func handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid offer", http.StatusBadRequest)
		return
	}

	// 媒體編解碼設定：H.264，packetization-mode=1
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
		http.Error(w, "register codec error", http.StatusInternalServerError)
		return
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "pc error", http.StatusInternalServerError)
		return
	}
	peerConn = pc

	// 建立 H.264 RTP Track
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "scrcpy",
	)
	if err != nil {
		http.Error(w, "track error", http.StatusInternalServerError)
		return
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		http.Error(w, "add track error", http.StatusInternalServerError)
		return
	}

	// 讀 RTCP（可擴充：偵測 PLI/FIR → needKeyframe=true）
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(rtcpBuf); err != nil {
				return
			}
			// 簡化：不解析內容；你可改成解析 PLI/FIR 後：
			// stateMu.Lock(); needKeyframe = true; stateMu.Unlock()
		}
	}()

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Println("PeerConnection state:", s.String())
		if s == webrtc.PeerConnectionStateFailed ||
			s == webrtc.PeerConnectionStateClosed ||
			s == webrtc.PeerConnectionStateDisconnected {
			stateMu.Lock()
			videoTrack = nil
			packetizer = nil
			stateMu.Unlock()
		}
	})

	// 設定 Remote SDP
	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, "set remote error", http.StatusInternalServerError)
		return
	}

	// 建立 Answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "answer error", http.StatusInternalServerError)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, "set local error", http.StatusInternalServerError)
		return
	}

	// 等待 ICE 完成（不走 trickle，簡化）
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	// 初始化發送端狀態
	stateMu.Lock()
	videoTrack = track
	// 隨機 SSRC（簡化用時間來源），時基 90kHz
	packetizer = rtp.NewPacketizer(
		1200,                          // MTU
		96,                            // Payload type
		uint32(time.Now().UnixNano()), // SSRC
		&codecs.H264Payloader{},
		rtp.NewRandomSequencer(),
		90000,
	)
	rtpTS = 0
	needKeyframe = true // 新用戶入房：先送 SPS/PPS，再等 IDR
	stateMu.Unlock()

	// 請求 Android 立刻送出關鍵幀
	requestKeyframe()

	// 回傳 Answer（含 ICE）
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pc.LocalDescription())
}

// requestKeyframe 要求 Android 重新送出關鍵幀
func requestKeyframe() {
	if controlConn == nil {
		return
	}
	if _, err := controlConn.Write([]byte{controlMsgResetVideo}); err != nil {
		log.Println("send RESET_VIDEO:", err)
	}
}

// === Annex-B 工具與發送 ===

// 把一個 Access Unit 的 NALUs 一次送完，最後一個 RTP 設 marker，送完再 +TS
func sendNALUAccessUnit(nalus [][]byte) {
	stateMu.RLock()
	pk := packetizer
	vt := videoTrack
	ts := rtpTS
	stateMu.RUnlock()
	if pk == nil || vt == nil || len(nalus) == 0 {
		return
	}

	for i, n := range nalus {
		pkts := pk.Packetize(n, ts)
		for j, p := range pkts {
			// 僅最後一個 NALU 的最後一個 RTP 設 Marker=true
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
			if err := vt.WriteRTP(p); err != nil {
				log.Println("RTP write error:", err)
			}
		}
	}
	// 每幀 30fps → 90kHz/30 = 3000
	stateMu.Lock()
	rtpTS += 3000
	stateMu.Unlock()
}

// 只送參數集（同一 timestamp，不遞增）
func sendNALUs(nalus ...[]byte) {
	stateMu.RLock()
	pk := packetizer
	vt := videoTrack
	ts := rtpTS
	stateMu.RUnlock()
	if pk == nil || vt == nil {
		return
	}
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		pkts := pk.Packetize(n, ts)
		for _, p := range pkts {
			// 參數集不當作一個獨立 AU，統一不設 Marker
			p.Marker = false
			// 若你要把 SPS/PPS 視為一個 AU，也可在最後一包設 true：
			// p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
			if err := vt.WriteRTP(p); err != nil {
				log.Println("RTP write error:", err)
			}
		}
	}
}

// 解析 Annex-B → 去除 start code 的 NALUs
func splitAnnexBNALUs(b []byte) [][]byte {
	var nalus [][]byte
	i := 0
	for {
		scStart, scEnd := findStartCode(b, i)
		if scStart < 0 {
			break
		}
		nextStart, _ := findStartCode(b, scEnd)
		if nextStart < 0 {
			// 最後一個 NALU
			n := b[scEnd:]
			if len(n) > 0 {
				nalus = append(nalus, n)
			}
			break
		}
		n := b[scEnd:nextStart]
		if len(n) > 0 {
			nalus = append(nalus, n)
		}
		i = nextStart
	}
	return nalus
}

func findStartCode(b []byte, from int) (int, int) {
	for i := from; i+3 <= len(b); i++ {
		// 00 00 01
		if i+3 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i, i + 3
		}
		// 00 00 00 01
		if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return i, i + 4
		}
	}
	return -1, -1
}

func naluType(n []byte) uint8 {
	if len(n) == 0 {
		return 0
	}
	return n[0] & 0x1F
}
