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

// === 全域狀態 ===
var (
	videoTrack    *webrtc.TrackLocalStaticRTP
	peerConn      *webrtc.PeerConnection
	packetizer    rtp.Packetizer
	rtpTS         uint32 // 90kHz 時基
	needKeyframe  bool   // 新用戶/PLI 時需要 SPS/PPS + IDR
	lastSPS       []byte
	lastPPS       []byte
	keyframeCache [][]byte // 最近一次完整關鍵幀 (SPS+PPS+IDR)

	stateMu   sync.RWMutex
	startTime time.Time
)

func main() {
	// 靜態檔案伺服器
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

	// debug 用：輸出 H264 檔
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
		if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
			log.Println("read frame meta:", err)
			break
		}
		frameSize := binary.BigEndian.Uint32(meta[8:12])

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

		// 解析 NALUs
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

		// 如果這幀是 IDR，把完整 AU 存成快取
		if idrInThisAU {
			stateMu.RLock()
			sps := lastSPS
			pps := lastPPS
			stateMu.RUnlock()
			if len(sps) > 0 && len(pps) > 0 {
				stateMu.Lock()
				keyframeCache = append([][]byte{}, sps, pps)
				keyframeCache = append(keyframeCache, nalus...)
				stateMu.Unlock()
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
				// 如果有快取，直接送出
				stateMu.RLock()
				cache := keyframeCache
				stateMu.RUnlock()
				if len(cache) > 0 {
					sendNALUAccessUnit(cache, false) // 不推進 TS
					stateMu.Lock()
					needKeyframe = false
					stateMu.Unlock()
				}
			} else {
				sendNALUAccessUnit(nalus, true) // 正常幀要推進 TS
			}
		}

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

	// 讀 RTCP
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(rtcpBuf); err != nil {
				return
			}
			// TODO: 可解析 PLI/FIR -> needKeyframe = true
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

	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, "set remote error", http.StatusInternalServerError)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "answer error", http.StatusInternalServerError)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, "set local error", http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	// 初始化發送端狀態
	stateMu.Lock()
	videoTrack = track
	packetizer = rtp.NewPacketizer(
		1200,
		96,
		uint32(time.Now().UnixNano()),
		&codecs.H264Payloader{},
		rtp.NewRandomSequencer(),
		90000,
	)
	rtpTS = 0

	// 如果有快取，馬上送一幀
	if len(keyframeCache) > 0 {
		sendNALUAccessUnit(keyframeCache, false)
		needKeyframe = false
	} else {
		needKeyframe = true
	}
	stateMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pc.LocalDescription())
}

// === Annex-B 工具 ===
func sendNALUAccessUnit(nalus [][]byte, advanceTS bool) {
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
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
			if err := vt.WriteRTP(p); err != nil {
				log.Println("RTP write error:", err)
			}
		}
	}
	if advanceTS {
		stateMu.Lock()
		rtpTS += 3000 // 假設 30fps
		stateMu.Unlock()
	}
}

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
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i, i + 3
		}
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
