package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
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
const ptsPerSecond = uint64(1_000_000) // scrcpy PTS 單位：微秒

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

	// 觀測 PLI/FIR 與 AU 序號、RTP 詳細列印
	lastPLI       time.Time
	pliCount      int
	framesSinceKF int
	auSeq         uint64
	verboseRTP    = true

	// PTS → RTP Timestamp 映射
	havePTS0 bool
	pts0     uint64
	rtpTS0   uint32

	// 目前「視訊解析度」（給觸控封包的 screen_size）
	videoW uint16
	videoH uint16

	pointerMu      sync.Mutex
	pointerButtons = make(map[uint64]uint32)
)

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

	// 重要：把控制通道讀掉，避免堆積阻塞
	go func(r io.Reader) {
		_, _ = io.Copy(io.Discard, r)
	}(conn.Control)

	// 週期性請求關鍵幀（可依需求調整或移除）
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

	// 視訊標頭 (12 bytes)：[codec:4][w:4][h:4]
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Fatal("read video header:", err)
	}
	codec := string(vHeader[:4])
	w0 := binary.BigEndian.Uint32(vHeader[4:8])
	h0 := binary.BigEndian.Uint32(vHeader[8:12])

	stateMu.Lock()
	videoW, videoH = uint16(w0), uint16(h0) // 觸控封包用的 screen_size 初值
	stateMu.Unlock()

	log.Printf("編碼格式: %s, 解析度: %dx%d\n", codec, w0, h0)

	// 接收幀迴圈
	meta := make([]byte, 12) // [pts(8)] + [size(4)]
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
					// 嘗試從新 SPS 解析寬高
					if w, h, ok := parseH264SPSDimensions(n); ok {
						videoW, videoH = w, h
						gotNewSPS = true
						log.Printf("[AU] 更新 SPS 並套用解析度 %dx%d 給觸控映射", w, h)
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

		// 讀取目前狀態（列印時用）
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
				// 確保拿到 IDR 再開始送
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

type touchEvent struct {
	Type     string  `json:"type"` // "down" | "up" | "move" | "cancel"
	ID       uint64  `json:"id"`   // pointer id
	X        int32   `json:"x"`
	Y        int32   `json:"y"`
	Width    uint16  `json:"width"`  // 來源端可傳，但我們忽略
	Height   uint16  `json:"height"` // 來源端可傳，但我們忽略
	Pressure float64 `json:"pressure"`
	Buttons  uint32  `json:"buttons"`
}

func handleTouchEvent(ev touchEvent) {
	if controlConn == nil {
		return
	}

	var action uint8
	var actionButton uint32

	pointerMu.Lock()
	prev := pointerButtons[ev.ID]
	switch ev.Type {
	case "down":
		action = 0 // AMOTION_EVENT_ACTION_DOWN
		actionButton = ev.Buttons &^ prev
	case "up":
		action = 1 // AMOTION_EVENT_ACTION_UP
		actionButton = prev &^ ev.Buttons
	case "move":
		action = 2 // AMOTION_EVENT_ACTION_MOVE
		actionButton = 0
	case "cancel":
		action = 3 // AMOTION_EVENT_ACTION_CANCEL
		actionButton = 0
	default:
		action = 2
		actionButton = 0
	}
	pointerButtons[ev.ID] = ev.Buttons
	pointerMu.Unlock()

	// 讀當前視訊寬高（觸控映射空間）
	stateMu.RLock()
	sw, sh := videoW, videoH
	stateMu.RUnlock()

	// 夾住座標，避免越界（保守處理）
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

	// ====== 序列化（總長 32 bytes）======
	buf := make([]byte, 32)
	buf[0] = 2                                 // SC_CONTROL_MSG_TYPE_INJECT_TOUCH_EVENT
	buf[1] = action                            // action
	binary.BigEndian.PutUint64(buf[2:], ev.ID) // pointer_id (u64)

	// position: x(int32), y(int32), w(u16), h(u16) — 大端序
	binary.BigEndian.PutUint32(buf[10:], uint32(ev.X))
	binary.BigEndian.PutUint32(buf[14:], uint32(ev.Y))
	binary.BigEndian.PutUint16(buf[18:], sw)
	binary.BigEndian.PutUint16(buf[20:], sh)

	// pressure: 0..1 → u16 固定小數；UP 事件壓力歸 0
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
	binary.BigEndian.PutUint16(buf[22:], p)

	// action_button (u32) + buttons (u32)
	binary.BigEndian.PutUint32(buf[24:], actionButton)
	binary.BigEndian.PutUint32(buf[28:], ev.Buttons)

	controlMu.Lock()
	if _, err := controlConn.Write(buf); err != nil {
		log.Println("send touch event:", err)
	}
	controlMu.Unlock()
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
		for j, p := range pkts {
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
		pkts := pk.Packetize(n, ts)
		for _, p := range pkts {
			p.Marker = false // 參數集不當作獨立 AU
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
					// 略過 scaling_list 內容（簡化: 粗略跳過）
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
