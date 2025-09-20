package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/yourname/scrcpy-go/adb"
)

const (
	controlMsgResetVideo = 17
	ptsPerSecond         = uint64(1_000_000) // scrcpy PTS 單位：微秒
)

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
	controlConn io.ReadWriter
	controlMu   sync.Mutex

	// 觀測 PLI/FIR 與 AU 序號
	lastPLI       time.Time
	pliCount      int
	framesSinceKF int
	auSeq         uint64

	// PTS → RTP Timestamp 映射
	havePTS0 bool
	pts0     uint64
	rtpTS0   uint32

	// 目前「視訊解析度」（僅作後備；主要用前端傳入的 screenW/H）
	videoW uint16
	videoH uint16

	// 指標按鍵狀態（用於 mouse action_button 計算）
	pointerMu      sync.Mutex
	pointerButtons = make(map[uint64]uint32)
)

// ====== 控制面（DataChannel → scrcpy socket）======

// 可靠關鍵事件隊列（down/up/cancel），決不丟；容量不要太大，避免長時間延後
var criticalQ = make(chan []byte, 512)

// 每指的最新 move（合併）；刷寫協程會以固定頻率送出
type moveKey struct{ id uint64 }

var moveMu sync.Mutex
var latestMove = map[moveKey][]byte{}

// move 刷新頻率（視網頁端 pointermove 頻率與網路情況可微調）
const moveFlushHz = 90

// 若 socket 堵塞，對「關鍵事件」的最長等待（避免永無止境卡死）
const criticalWriteTimeout = 120 * time.Millisecond

// move 寫入的最長等待（更短；無法寫就丟）
const moveWriteTimeout = 20 * time.Millisecond

func startControlWriters() {
	// 可靠事件寫入器
	go func() {
		for pkt := range criticalQ {
			writeWithDeadline(pkt, criticalWriteTimeout, true)
		}
	}()

	// move 合併刷寫器
	go func() {
		ticker := time.NewTicker(time.Second / moveFlushHz)
		defer ticker.Stop()
		for range ticker.C {
			moveMu.Lock()
			batches := make([][]byte, 0, len(latestMove))
			for _, b := range latestMove {
				batches = append(batches, b)
			}
			latestMove = make(map[moveKey][]byte)
			moveMu.Unlock()

			for _, pkt := range batches {
				writeWithDeadline(pkt, moveWriteTimeout, false)
			}
		}
	}()
}

// 寫入控制 socket（可設 deadline；對關鍵事件可稍長一點）
func writeWithDeadline(b []byte, d time.Duration, isCritical bool) {
	if controlConn == nil {
		return
	}
	controlMu.Lock()
	defer controlMu.Unlock()

	// 嘗試對 net.Conn 設定 deadline；不是 net.Conn 就照常寫
	if c, ok := controlConn.(net.Conn); ok {
		_ = c.SetWriteDeadline(time.Now().Add(d))
		defer c.SetWriteDeadline(time.Time{})
	}
	_, err := controlConn.Write(b)
	if err != nil {
		if isCritical {
			log.Println("[CTRL] write critical err:", err)
		}
		// move 錯誤可忽略
	}
}

// 把 JSON 事件轉成 scrcpy（你使用的）32 bytes 格式並入列
func handleTouchEvent(ev touchEvent) {
	if controlConn == nil {
		return
	}

	// 讀前端傳過來的畫面寬高，若無則退回伺服器解析到的寬高
	stateMu.RLock()
	fallbackW, fallbackH := videoW, videoH
	stateMu.RUnlock()
	sw := ev.ScreenW
	sh := ev.ScreenH
	if sw == 0 || sh == 0 {
		sw, sh = fallbackW, fallbackH
	}

	// 夾住座標（保守處理，避免越界）
	if ev.X < 0 {
		ev.X = 0
	}
	if ev.Y < 0 {
		ev.Y = 0
	}
	if sw > 0 && sh > 0 {
		if ev.X > int32(sw)-1 {
			ev.X = int32(sw) - 1
		}
		if ev.Y > int32(sh)-1 {
			ev.Y = int32(sh) - 1
		}
	}

	// 計算 action 與 action_button（只有 pointerType=mouse 才有意義）
	var action uint8
	switch ev.Type {
	case "down":
		action = 0 // AMOTION_EVENT_ACTION_DOWN
	case "up":
		action = 1 // AMOTION_EVENT_ACTION_UP
	case "move":
		action = 2 // AMOTION_EVENT_ACTION_MOVE
	case "cancel":
		action = 3 // AMOTION_EVENT_ACTION_CANCEL
	default:
		action = 2
	}

	var actionButton uint32
	pointerMu.Lock()
	prevButtons := pointerButtons[ev.ID]
	nowButtons := ev.Buttons

	// 對觸控（pointerType="touch"）按 Android 規範不送任何 mouse buttons
	// 前端已經將 touch 的 buttons 規一為 0，這裡保險起見再做一次
	if ev.PointerType == "touch" {
		nowButtons = 0
	}

	switch action {
	case 0: // down
		actionButton = nowButtons &^ prevButtons
	case 1: // up
		actionButton = prevButtons &^ nowButtons
	default:
		actionButton = 0
	}
	pointerButtons[ev.ID] = nowButtons
	pointerMu.Unlock()

	// 壓力轉 u16 固定小數（UP 事件強制 0）
	var p uint16
	if action != 1 {
		f := ev.Pressure
		if f < 0 {
			f = 0
		} else if f > 1 {
			f = 1
		}
		if f == 1 {
			p = 0xffff
		} else {
			p = uint16(math.Round(f * 65535))
		}
	}

	// ====== 32 bytes 格式（你的 scrcpy 變體）======
	// 0:  type=2 (INJECT_TOUCH_EVENT)
	// 1:  action (u8)
	// 2..9:  pointer_id (u64)
	// 10..13: x (i32)
	// 14..17: y (i32)
	// 18..19: screenW (u16)
	// 20..21: screenH (u16)
	// 22..23: pressure (u16, 0..65535，0xffff 代表 1.0)
	// 24..27: action_button (u32)  ← 你的 32bytes 版本特有
	// 28..31: buttons (u32)
	buf := make([]byte, 32)
	buf[0] = 2
	buf[1] = action
	binary.BigEndian.PutUint64(buf[2:], ev.ID)
	binary.BigEndian.PutUint32(buf[10:], uint32(ev.X))
	binary.BigEndian.PutUint32(buf[14:], uint32(ev.Y))
	binary.BigEndian.PutUint16(buf[18:], sw)
	binary.BigEndian.PutUint16(buf[20:], sh)
	binary.BigEndian.PutUint16(buf[22:], p)
	binary.BigEndian.PutUint32(buf[24:], actionButton)
	binary.BigEndian.PutUint32(buf[28:], nowButtons)

	// 入列：關鍵事件走 criticalQ、move 走合併池
	if action == 2 { // move
		moveMu.Lock()
		latestMove[moveKey{ev.ID}] = buf
		moveMu.Unlock()
	} else {
		select {
		case criticalQ <- buf:
			// ok
		default:
			// 若 criticalQ 滿了，仍然盡力寫（避免按鍵卡死）
			writeWithDeadline(buf, criticalWriteTimeout, true)
		}
	}
}

// ========= 伺服器入口 =========

func main() {
	// 靜態檔案 + SDP handler
	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)
	http.HandleFunc("/offer", handleOffer)

	// 啟動 HTTP
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
	if err := dev.Reverse("localabstract:scrcpy", fmt.Sprintf("tcp:%d", adb.ScrcpyPort)); err != nil {
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

	// 控制通道清空（避免裝置回傳的 clipboard 等把管道塞爆）
	go func(r io.Reader) {
		_, _ = io.Copy(io.Discard, r)
	}(conn.Control)

	// 啟動控制寫入協程
	startControlWriters()

	// 週期性請求關鍵幀（可依需求調整）
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			stateMu.Lock()
			needKeyframe = true
			stateMu.Unlock()
			log.Println("[KF] 週期性請求關鍵幀")
			requestKeyframe()
		}
	}()

	log.Println("開始接收視訊串流")

	// 跳過裝置名稱 (64 bytes, 以 NUL 結尾)
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		log.Fatal("read device name:", err)
	}
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	log.Printf("裝置名稱: %s\n", deviceName)

	// 視訊標頭 (12 bytes)：[codecID(u32)][w(u32)][h(u32)]
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Fatal("read video header:", err)
	}
	codecID := binary.BigEndian.Uint32(vHeader[0:4]) // 0=H264, 1=H265, 2=AV1（依版本可能不同）
	w0 := binary.BigEndian.Uint32(vHeader[4:8])
	h0 := binary.BigEndian.Uint32(vHeader[8:12])

	stateMu.Lock()
	videoW, videoH = uint16(w0), uint16(h0) // 後備觸控映射空間
	stateMu.Unlock()

	log.Printf("編碼ID: %d, 解析度: %dx%d\n", codecID, w0, h0)

	// 接收幀迴圈
	// 多數版本下 frame meta 是 12 bytes：[PTS(u64)] + [size(u32)]
	meta := make([]byte, 12)
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

		idrInThisAU := false
		var gotNewSPS bool

		for _, n := range nalus {
			switch naluType(n) {
			case 7: // SPS
				stateMu.Lock()
				if !bytes.Equal(lastSPS, n) {
					// 嘗試從新 SPS 解析寬高（常見 4:2:0）
					if w, h, ok := parseH264SPSDimensions(n); ok {
						videoW, videoH = w, h
						gotNewSPS = true
						log.Printf("[AU] 更新 SPS 並套用解析度 %dx%d 給觸控映射(後備)", w, h)
					}
				}
				lastSPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 8: // PPS
				stateMu.Lock()
				if !bytes.Equal(lastPPS, n) {
					log.Printf("[AU] 更新 PPS (len=%d)", len(n))
				}
				lastPPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 5: // IDR
				idrInThisAU = true
			}
		}

		// 讀取目前狀態
		stateMu.RLock()
		vt := videoTrack
		pk := packetizer
		waitKF := needKeyframe
		stateMu.RUnlock()

		// 推進 WebRTC
		if vt != nil && pk != nil {
			// 如果剛換解析度，先把 SPS/PPS prepend，等待 IDR
			if gotNewSPS {
				stateMu.RLock()
				sps := lastSPS
				pps := lastPPS
				stateMu.RUnlock()
				if len(sps) > 0 {
					sendNALUsAtTS(curTS, sps)
				}
				if len(pps) > 0 {
					sendNALUsAtTS(curTS, pps)
				}
				// 等待 IDR
				waitKF = true
				stateMu.Lock()
				needKeyframe = true
				stateMu.Unlock()
				requestKeyframe()
			}

			if waitKF {
				stateMu.RLock()
				sps := lastSPS
				pps := lastPPS
				stateMu.RUnlock()

				if len(sps) > 0 && len(pps) > 0 {
					sendNALUsAtTS(curTS, sps, pps)
				} else {
					requestKeyframe()
				}

				framesSinceKF++
				if framesSinceKF%30 == 0 {
					log.Printf("[KF] 等待 IDR 中... 已過 %d 幀；再次請求關鍵幀", framesSinceKF)
					requestKeyframe()
				}

				if !idrInThisAU {
					goto stats
				}

				log.Println("[KF] 偵測到 IDR，開始送流")
				stateMu.Lock()
				needKeyframe = false
				framesSinceKF = 0
				stateMu.Unlock()

				sendNALUAccessUnitAtTS(nalus, curTS)
			} else {
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

	// 讀 RTCP：解析 PLI / FIR，並請求關鍵幀
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
				}
			}
		}
	}()

	// 接前端兩條 DataChannel
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Println("DataChannel:", dc.Label())
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var ev touchEvent
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				log.Println("datachannel unmarshal:", err)
				return
			}
			handleTouchEvent(ev)
		})
	})

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

	// 設定 Remote SDP / 建立 Answer / 等待 ICE（非 trickle）
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
	<-webrtc.GatheringCompletePromise(pc)

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
	needKeyframe = true // 新用戶：先送 SPS/PPS，再等 IDR
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

// 要求 Android 重新送出關鍵幀
func requestKeyframe() {
	if controlConn == nil {
		return
	}
	controlMu.Lock()
	defer controlMu.Unlock()
	if _, err := controlConn.Write([]byte{controlMsgResetVideo}); err != nil {
		log.Println("send RESET_VIDEO:", err)
	}
}

// ===== 型別/工具 =====

type touchEvent struct {
	Type        string  `json:"type"` // "down" | "up" | "move" | "cancel"
	ID          uint64  `json:"id"`   // pointer id
	X           int32   `json:"x"`
	Y           int32   `json:"y"`
	ScreenW     uint16  `json:"screenW"`     // 前端傳入的原生寬
	ScreenH     uint16  `json:"screenH"`     // 前端傳入的原生高
	Pressure    float64 `json:"pressure"`    // 0..1
	Buttons     uint32  `json:"buttons"`     // 對 touch 會是 0
	PointerType string  `json:"pointerType"` // "mouse" | "touch" | "pen"
}

// === RTP 發送（以指定 TS） ===
// FIX: Packetizer 的 Packetize 第二參數是 samples（累加量），我們改為 samples=0，並手動覆寫 p.Timestamp
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
		pkts := pk.Packetize(n, 0)
		for j, p := range pkts {
			p.Timestamp = ts
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
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
		pkts := pk.Packetize(n, 0)
		for _, p := range pkts {
			p.Timestamp = ts
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

// === PTS → RTP TS 轉換 ===
func rtpTSFromPTS(pts, base uint64) uint32 {
	delta := pts - base
	return uint32((delta * 90000) / ptsPerSecond) // 90kHz * 秒數
}

// === H.264 SPS 解析寬高（極簡實作，足夠抓常見 4:2:0） ===
func parseH264SPSDimensions(nal []byte) (w, h uint16, ok bool) {
	if len(nal) < 4 || (nal[0]&0x1F) != 7 {
		return
	}
	// 去除 emulation prevention bytes（00 00 03 → 00 00）
	rbsp := make([]byte, 0, len(nal)-1)
	for i := 1; i < len(nal); i++ { // 跳過 NAL header
		if i+2 < len(nal) && nal[i] == 0 && nal[i+1] == 0 && nal[i+2] == 3 {
			rbsp = append(rbsp, 0, 0)
			i += 2
			continue
		}
		rbsp = append(rbsp, nal[i])
	}
	br := bitReader{b: rbsp}

	// profile_idc, constraint_flags, level_idc
	if !br.skip(8 + 8 + 8) {
		return
	}
	// seq_parameter_set_id
	if _, ok2 := br.ue(); !ok2 {
		return
	}

	// 一些 profile 會帶 chroma_format_idc
	var chromaFormatIDC uint = 1 // 預設 4:2:0
	profileIDC := rbsp[0]
	if profileIDC == 100 || profileIDC == 110 || profileIDC == 122 ||
		profileIDC == 244 || profileIDC == 44 || profileIDC == 83 ||
		profileIDC == 86 || profileIDC == 118 || profileIDC == 128 ||
		profileIDC == 138 || profileIDC == 139 || profileIDC == 134 {
		if v, ok2 := br.ue(); !ok2 {
			return
		} else {
			chromaFormatIDC = v
		}
		if chromaFormatIDC == 3 {
			if _, ok2 := br.u(1); !ok2 { // separate_colour_plane_flag
				return
			}
		}
		// bit_depth_luma_minus8, bit_depth_chroma_minus8, qpprime_y_zero_transform_bypass_flag
		if _, ok2 := br.ue(); !ok2 {
			return
		}
		if _, ok2 := br.ue(); !ok2 {
			return
		}
		if !br.skip(1) {
			return
		}
		// seq_scaling_matrix_present_flag
		if f, ok2 := br.u(1); !ok2 {
			return
		} else if f == 1 {
			// 略過 scaling_list
			n := 8
			if chromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if g, ok3 := br.u(1); !ok3 {
					return
				} else if g == 1 {
					// 粗略跳過
					size := 16
					if i >= 6 {
						size = 64
					}
					lastScale := 8
					nextScale := 8
					for j := 0; j < size; j++ {
						if nextScale != 0 {
							delta, ok4 := br.se()
							if !ok4 {
								return
							}
							nextScale = (lastScale + int(delta) + 256) % 256
						}
						if nextScale != 0 {
							lastScale = nextScale
						}
					}
				}
			}
		}
	}

	// log2_max_frame_num_minus4
	if _, ok2 := br.ue(); !ok2 {
		return
	}
	// pic_order_cnt_type
	pct, ok2 := br.ue()
	if !ok2 {
		return
	}
	if pct == 0 {
		if _, ok2 = br.ue(); !ok2 { // log2_max_pic_order_cnt_lsb_minus4
			return
		}
	} else if pct == 1 {
		if !br.skip(1) { // delta_pic_order_always_zero_flag
			return
		}
		if _, ok2 = br.se(); !ok2 {
			return
		}
		if _, ok2 = br.se(); !ok2 {
			return
		}
		var n uint
		if n, ok2 = br.ue(); !ok2 {
			return
		}
		for i := uint(0); i < n; i++ {
			if _, ok2 = br.se(); !ok2 {
				return
			}
		}
	}

	// num_ref_frames, gaps_in_frame_num_value_allowed_flag
	if _, ok2 = br.ue(); !ok2 {
		return
	}
	if !br.skip(1) {
		return
	}

	// 寬高
	pwMinus1, ok2 := br.ue()
	if !ok2 {
		return
	}
	phMinus1, ok2 := br.ue()
	if !ok2 {
		return
	}
	frameMbsOnlyFlag, ok2 := br.u(1)
	if !ok2 {
		return
	}
	if frameMbsOnlyFlag == 0 {
		if !br.skip(1) { // mb_adaptive_frame_field_flag
			return
		}
	}
	if !br.skip(1) { // direct_8x8_inference_flag
		return
	}

	// cropping
	cropLeft, cropRight, cropTop, cropBottom := uint(0), uint(0), uint(0), uint(0)
	fcrop, ok2 := br.u(1)
	if !ok2 {
		return
	}
	if fcrop == 1 {
		if cropLeft, ok2 = br.ue(); !ok2 {
			return
		}
		if cropRight, ok2 = br.ue(); !ok2 {
			return
		}
		if cropTop, ok2 = br.ue(); !ok2 {
			return
		}
		if cropBottom, ok2 = br.ue(); !ok2 {
			return
		}
	}

	mbWidth := (pwMinus1 + 1)
	mbHeight := (phMinus1 + 1) * (2 - frameMbsOnlyFlag)

	// 計算 crop 單位
	var subW, subH uint = 1, 1
	switch chromaFormatIDC {
	case 0: // monochrome
		subW, subH = 1, 1
	case 1: // 4:2:0
		subW, subH = 2, 2
	case 2: // 4:2:2
		subW, subH = 2, 1
	case 3: // 4:4:4
		subW, subH = 1, 1
	}
	cropUnitX := subW
	cropUnitY := subH * (2 - frameMbsOnlyFlag)

	width := int(mbWidth*16) - int((cropLeft+cropRight)*cropUnitX)
	height := int(mbHeight*16) - int((cropTop+cropBottom)*cropUnitY)

	if width <= 0 || height <= 0 || width > 65535 || height > 65535 {
		return
	}
	return uint16(width), uint16(height), true
}

// --- 極簡 bit reader ---
type bitReader struct {
	b []byte
	i int // bit index
}

func (br *bitReader) u(n int) (uint, bool) {
	if n <= 0 {
		return 0, true
	}
	var v uint
	for k := 0; k < n; k++ {
		byteIndex := br.i / 8
		if byteIndex >= len(br.b) {
			return 0, false
		}
		bitIndex := 7 - (br.i % 8)
		bit := (br.b[byteIndex] >> uint(bitIndex)) & 1
		v = (v << 1) | uint(bit)
		br.i++
	}
	return v, true
}
func (br *bitReader) skip(n int) bool {
	_, ok := br.u(n)
	return ok
}
func (br *bitReader) ue() (uint, bool) {
	var leadingZeros int
	for {
		b, ok := br.u(1)
		if !ok {
			return 0, false
		}
		if b == 0 {
			leadingZeros++
		} else {
			break
		}
	}
	if leadingZeros == 0 {
		return 0, true
	}
	val, ok := br.u(leadingZeros)
	if !ok {
		return 0, false
	}
	return (1 << leadingZeros) - 1 + val, true
}
func (br *bitReader) se() (int, bool) {
	uev, ok := br.ue()
	if !ok {
		return 0, false
	}
	k := int(uev)
	if k%2 == 0 {
		return -k / 2, true
	}
	return (k + 1) / 2, true
}
