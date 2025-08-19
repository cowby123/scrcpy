package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/yourname/scrcpy-go/adb"
)

const controlMsgResetVideo = 17

// 假設 scrcpy 的 PTS 單位為「微秒」
const ptsPerSecond = uint64(1_000_000) // 1e6

// === 全域狀態 ===
var (
	videoTrack   *webrtc.TrackLocalStaticRTP
	peerConn     *webrtc.PeerConnection
	packetizer   rtp.Packetizer
	needKeyframe bool // 新用戶/PLI 時需要 SPS/PPS + IDR

	// H.264 參數集快取
	lastSPS []byte
	lastPPS []byte

	stateMu sync.RWMutex

	startTime   time.Time // 速率統計
	controlConn io.Writer // 與 Android 控制通道

	// 觀測 PLI/FIR 與 AU 序號、RTP 詳細列印
	lastPLI       time.Time
	pliCount      int
	framesSinceKF int
	auSeq         uint64
	verboseRTP    = true // 設 true 會列印每個 NALU 的 RTP 切片與 Marker

	// PTS → RTP Timestamp 映射
	havePTS0 bool
	pts0     uint64
	rtpTS0   uint32 // 可為 0；保留擴充空間（若要做 offset）
)

func main() {
	// 路由註冊
	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)
	http.HandleFunc("/offer", handleOffer)

	// 啟動 HTTP（簡化）
	go func() {
		log.Println("HTTP 伺服器: http://localhost:8080/  (SDP: POST /offer)")
		srv := &http.Server{Addr: ":8080"}
		log.Fatal(srv.ListenAndServe())
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

	// ===== 每 5 秒固定請求一次關鍵幀 =====
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			stateMu.Lock()
			needKeyframe = true // 讓送流路徑在下一個 IDR 前 prepend SPS/PPS
			stateMu.Unlock()

			log.Println("[KF] 週期性請求關鍵幀 (每 5 秒)")
			requestKeyframe()
		}
	}()
	// =================================

	log.Println("開始接收視訊串流（已停用磁碟寫入）")

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
		pts := binary.BigEndian.Uint64(meta[0:8])
		frameSize := binary.BigEndian.Uint32(meta[8:12])

		// 初始化 PTS 基準
		if !havePTS0 {
			pts0 = pts
			rtpTS0 = 0
			havePTS0 = true
		}
		curTS := rtpTS0 + rtpTSFromPTS(pts, pts0)

		// frame data
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
			log.Println("read frame:", err)
			break
		}

		// 解析 Annex-B → NALUs，並快取 SPS/PPS、偵測是否含 IDR
		nalus := splitAnnexBNALUs(frame)

		// 統計這個 AU 的 NALU 組成
		var cntSPS, cntPPS, cntIDR, cntNonIDR, cntSEI, cntAUD int
		idrInThisAU := false

		for _, n := range nalus {
			t := naluType(n)
			switch t {
			case 7: // SPS
				cntSPS++
				stateMu.Lock()
				if !bytes.Equal(lastSPS, n) {
					log.Printf("[AU] 更新 SPS (len=%d)", len(n))
				}
				lastSPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 8: // PPS
				cntPPS++
				stateMu.Lock()
				if !bytes.Equal(lastPPS, n) {
					log.Printf("[AU] 更新 PPS (len=%d)", len(n))
				}
				lastPPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 5: // IDR
				cntIDR++
				idrInThisAU = true
			case 1:
				cntNonIDR++
			case 6:
				cntSEI++
			case 9:
				cntAUD++
			}
		}

		// 讀取目前狀態（列印時用）
		stateMu.RLock()
		vt := videoTrack
		pk := packetizer
		waitKF := needKeyframe
		stateMu.RUnlock()

		// 推進 WebRTC
		if vt != nil && pk != nil {
			if waitKF {
				stateMu.RLock()
				sps := lastSPS
				pps := lastPPS
				stateMu.RUnlock()

				if len(sps) > 0 && len(pps) > 0 {
					log.Println("[KF] prepend SPS+PPS")
					sendNALUsAtTS(curTS, sps, pps) // 同一 timestamp
				} else {
					log.Println("[KF] 尚未緩存到 SPS/PPS，重新請求關鍵幀")
					requestKeyframe()
				}

				// 保險：每 30 幀再請一次（約 1 秒，假設 ~30fps）
				framesSinceKF++
				if framesSinceKF%30 == 0 {
					log.Printf("[KF] 等待 IDR 中... 已過 %d 幀；再次請求關鍵幀", framesSinceKF)
					requestKeyframe()
				}

				if !idrInThisAU {
					goto stats // 這幀不送
				}

				log.Println("[KF] 偵測到 IDR，開始送流")
				stateMu.Lock()
				needKeyframe = false
				framesSinceKF = 0
				stateMu.Unlock()

				sendNALUAccessUnitAtTS(nalus, curTS) // 送這個含 IDR 的 AU
			} else {
				// 正常持續送
				sendNALUAccessUnitAtTS(nalus, curTS)
			}
		}

	stats:
		frameCount++
		totalBytes += int64(frameSize)
		if frameCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			bytesPerSecond := float64(totalBytes) / elapsed
			stateMu.RLock()
			lp := lastPLI
			pc := pliCount
			stateMu.RUnlock()
			log.Printf("接收影格: %d, 速率: %.2f MB/s, PLI 累計: %d (last=%s)",
				frameCount, bytesPerSecond/(1024*1024), pc, lp.Format(time.RFC3339))
		}

		// 下一 AU 序號
		stateMu.Lock()
		auSeq++
		stateMu.Unlock()
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

	// 讀 RTCP：解析並印出 PLI / FIR，並請求關鍵幀
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(rtcpBuf)
			if err != nil {
				return
			}
			pkts, err := rtcp.Unmarshal(rtcpBuf[:n])
			if err != nil {
				log.Println("RTCP unmarshal error:", err)
				continue
			}
			for _, pkt := range pkts {
				switch p := pkt.(type) {
				case *rtcp.PictureLossIndication:
					log.Println("[RTCP] 收到 PLI → needKeyframe = true")
					stateMu.Lock()
					needKeyframe = true
					lastPLI = time.Now()
					pliCount++
					stateMu.Unlock()
					requestKeyframe()
				case *rtcp.FullIntraRequest:
					log.Printf("[RTCP] 收到 FIR → needKeyframe = true (SenderSSRC=%d, MediaSSRC=%d)", p.SenderSSRC, p.MediaSSRC)
					stateMu.Lock()
					needKeyframe = true
					lastPLI = time.Now()
					pliCount++
					stateMu.Unlock()
					requestKeyframe()
				default:
					// log.Printf("[RTCP] 其他封包: %T\n", pkt)
				}
			}
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
	packetizer = rtp.NewPacketizer(
		1200,                          // MTU
		96,                            // Payload type
		uint32(time.Now().UnixNano()), // SSRC（簡易）
		&codecs.H264Payloader{},
		rtp.NewRandomSequencer(),
		90000,
	)
	needKeyframe = true // 新用戶入房：先送 SPS/PPS，再等 IDR
	auSeq = 0
	havePTS0 = false
	pts0 = 0
	rtpTS0 = 0
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

// === RTP 發送（以指定 TS） ===

func sendNALUAccessUnitAtTS(nalus [][]byte, ts uint32) {
	stateMu.RLock()
	pk := packetizer
	vt := videoTrack
	stateMu.RUnlock()
	if pk == nil || vt == nil || len(nalus) == 0 {
		return
	}

	for i, n := range nalus {
		if len(n) == 0 {
			continue
		}
		pkts := pk.Packetize(n, ts)

		// if verboseRTP {
		// 	log.Printf("[AU #%d] RTPize NALU[%d/%d] %s len=%d → pkts=%d ts=%d",
		// 		auSeq, i+1, len(nalus), h264NaluName(naluType(n)), len(n), len(pkts), ts)
		// }

		for j, p := range pkts {
			// 僅最後一個 NALU 的最後一個 RTP 設 Marker=true
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)

			// if verboseRTP && p.Marker {
			// 	log.Printf("[AU #%d] Marker=true at seq=%d ts=%d", auSeq, p.SequenceNumber, p.Timestamp)
			// }

			if err := vt.WriteRTP(p); err != nil {
				log.Println("RTP write error:", err)
			}
		}
	}
}

func sendNALUsAtTS(ts uint32, nalus ...[]byte) {
	stateMu.RLock()
	pk := packetizer
	vt := videoTrack
	stateMu.RUnlock()
	if pk == nil || vt == nil {
		return
	}
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		pkts := pk.Packetize(n, ts)

		if verboseRTP {
			log.Printf("[AU #%d] PREPEND %s len=%d → pkts=%d ts=%d",
				auSeq, h264NaluName(naluType(n)), len(n), len(pkts), ts)
		}

		for _, p := range pkts {
			// 參數集不當作一個獨立 AU，統一不設 Marker
			p.Marker = false
			if err := vt.WriteRTP(p); err != nil {
				log.Println("RTP write error:", err)
			}
		}
	}
}

// === Annex-B 工具 ===

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

func h264NaluName(t uint8) string {
	switch t {
	// case 1:
	// 	return "Non-IDR (P/B)"
	case 5:
		return "IDR"
	case 6:
		return "SEI"
	case 7:
		return "SPS"
	case 8:
		return "PPS"
	case 9:
		return "AUD"
	default:
		return fmt.Sprintf("type=%d", t)
	}
}

// === PTS → RTP TS 轉換 ===

func rtpTSFromPTS(pts, base uint64) uint32 {
	delta := pts - base
	// 90kHz * 秒數；pts 單位為微秒
	return uint32((delta * 90000) / ptsPerSecond)
}
