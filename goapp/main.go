// main.go — 線路格式對齊官方：觸控載荷 31B + type 1B = 總 32B；
// 觸控 ≤10 指映射；mouse/pen 固定用 ID 0；忽略滑鼠 hover move；
// DataChannel 收到控制訊號時印出原始資料並照官方順序編碼後直寫 control socket。
// ★ 新增：control socket 讀回解析（DeviceMessage：clipboard）、心跳 GET_CLIPBOARD、寫入 deadline/耗時告警。

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"expvar"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	_ "net/http/pprof" // 啟用 /debug/pprof
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/yourname/scrcpy-go/adb"
)

const (
	controlMsgResetVideo   = 17                // TYPE_RESET_VIDEO
	controlMsgGetClipboard = 8                 // TYPE_GET_CLIPBOARD
	ptsPerSecond           = uint64(1_000_000) // scrcpy PTS 單位：微秒
)

// ---- 可調偵錯閾值 ----
const (
	criticalWriteTimeout = 120 * time.Millisecond
	warnCtrlWriteOver    = 30 * time.Millisecond // 控制通道單次寫入超過此值就告警
	warnFrameMetaOver    = 20 * time.Millisecond // 讀 frame meta >20ms
	warnFrameReadOver    = 50 * time.Millisecond // 讀 frame data >50ms
	statsLogEvery        = 100                   // 每 100 幀打印統計
	keyframeTick         = 5 * time.Second       // 週期性請求關鍵幀

	// control 心跳與讀回監控
	controlHealthTick      = 5 * time.Second  // 每 5s 檢查一次讀回
	controlStaleAfter      = 15 * time.Second // 超過 15s 無讀回就送 GET_CLIPBOARD 心跳
	controlReadBufMax      = 1 << 20          // 讀回緩衝上限（1MB，足夠容納剪貼簿）
	deviceMsgTypeClipboard = 0                // 目前僅解析 clipboard
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

	startTime     time.Time // 速率統計
	controlConn   io.ReadWriter
	controlMu     sync.Mutex
	lastCtrlRead  time.Time // 最近一次從 control socket 讀到裝置訊息
	lastCtrlWrite time.Time // 最近一次成功寫入 control

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

	// ADB 目標設備
	adbTarget string

	// 指標按鍵狀態（用於 mouse action_button 計算）
	pointerMu      sync.Mutex
	pointerButtons = make(map[uint64]uint32)
)

// ====== 指標（expvar）======
var (
	evFramesRead         = expvar.NewInt("frames_read")
	evBytesRead          = expvar.NewInt("bytes_read")
	evPLICount           = expvar.NewInt("pli_count")
	evKeyframeRequests   = expvar.NewInt("keyframe_requests")
	evCtrlWritesOK       = expvar.NewInt("control_writes_ok")
	evCtrlWritesErr      = expvar.NewInt("control_writes_err")
	evCtrlReadsOK        = expvar.NewInt("control_reads_ok")
	evCtrlReadsErr       = expvar.NewInt("control_reads_err")
	evCtrlReadClipboardB = expvar.NewInt("control_read_clipboard_bytes")
	evNALU_SPS           = expvar.NewInt("nalu_sps")
	evNALU_PPS           = expvar.NewInt("nalu_pps")
	evNALU_IDR           = expvar.NewInt("nalu_idr")
	evNALU_Others        = expvar.NewInt("nalu_others")
	evRTCP_PLI           = expvar.NewInt("rtcp_pli")
	evRTCP_FIR           = expvar.NewInt("rtcp_fir")
	evVideoW             = expvar.NewInt("video_w")
	evVideoH             = expvar.NewInt("video_h")
	evLastCtrlWriteMS    = expvar.NewInt("last_control_write_ms")
	evLastFrameMetaMS    = expvar.NewInt("last_frame_meta_ms")
	evLastFrameReadMS    = expvar.NewInt("last_frame_read_ms")
	evAuSeq              = expvar.NewInt("au_seq")
	evFramesSinceKF      = expvar.NewInt("frames_since_kf")
	evPendingPointers    = expvar.NewInt("pending_pointers")
	evActivePeer         = expvar.NewInt("active_peer") // 0/1
	evLastCtrlReadMsAgo  = expvar.NewInt("last_control_read_ms_ago")
	evHeartbeatSent      = expvar.NewInt("control_heartbeat_sent")
)

// ====== 工具：安全啟動 goroutine，避免 panic 默默死掉 ======
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

// ====== ★ 觸控 pointer 映射（限制活躍 ≤ 10；ID 0 給 mouse/pen）======
const maxPointers = 10

var touchMu sync.Mutex

// remoteID -> local slot (0..9)
var touchLocalByRemote = map[uint64]uint16{}

// local slot -> remoteID；slot 是否使用
var touchRemoteByLocal [maxPointers]uint64
var touchSlotUsed [maxPointers]bool

func getLocalSlot(remote uint64) (uint16, bool) {
	if s, ok := touchLocalByRemote[remote]; ok {
		return s, true
	}
	return 0, false
}
func allocLocalSlot(remote uint64) (uint16, bool) {
	if s, ok := touchLocalByRemote[remote]; ok {
		return s, true
	}
	for i := 0; i < maxPointers; i++ {
		if !touchSlotUsed[i] {
			touchSlotUsed[i] = true
			touchLocalByRemote[remote] = uint16(i)
			touchRemoteByLocal[i] = remote
			return uint16(i), true
		}
	}
	return 0, false
}
func freeLocalSlot(remote uint64) {
	if s, ok := touchLocalByRemote[remote]; ok {
		delete(touchLocalByRemote, remote)
		idx := int(s)
		touchSlotUsed[idx] = false
		touchRemoteByLocal[idx] = 0
	}
}

// 寫入控制 socket：**一定寫完整個封包**，並可選設置 write deadline（避免長時間阻塞）
func writeFull(b []byte, deadline time.Duration, setDeadline bool) {
	if controlConn == nil || len(b) == 0 {
		return
	}
	start := time.Now()
	controlMu.Lock()
	defer controlMu.Unlock()

	// 嘗試設置 write deadline（若底層支援）
	if setDeadline {
		if c, ok := controlConn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = c.SetWriteDeadline(time.Now().Add(deadline))
		}
	}

	total := 0
	for total < len(b) {
		n, err := controlConn.Write(b[total:])
		total += n
		if err != nil {
			evCtrlWritesErr.Add(1)
			log.Printf("[CTRL] write error after %d/%d bytes (elapsed=%v, deadline=%v): %v",
				total, len(b), time.Since(start), setDeadline, err)
			return
		}
	}
	elapsed := time.Since(start)
	lastCtrlWrite = time.Now()
	evLastCtrlWriteMS.Set(elapsed.Milliseconds())
	evCtrlWritesOK.Add(1)
	if elapsed > warnCtrlWriteOver {
		log.Printf("[CTRL] write 慢 (%v) deadline=%v size=%d", elapsed, setDeadline, len(b))
	}
	// 若曾設置 deadline，寫完後清掉（避免影響其他操作）
	if setDeadline {
		if c, ok := controlConn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = c.SetWriteDeadline(time.Time{})
		}
	}
}

// ====== 前端事件（JSON）→ 官方線路格式（32 bytes）======
type touchEvent struct {
	Type        string  `json:"type"` // "down" | "up" | "move" | "cancel"
	ID          uint64  `json:"id"`   // pointer id（前端的）
	X           int32   `json:"x"`
	Y           int32   `json:"y"`
	ScreenW     uint16  `json:"screenW"`     // 原生寬
	ScreenH     uint16  `json:"screenH"`     // 原生高
	Pressure    float64 `json:"pressure"`    // 0..1
	Buttons     uint32  `json:"buttons"`     // mouse buttons bitmask；touch 一律 0
	PointerType string  `json:"pointerType"` // "mouse" | "touch" | "pen"
}

func handleTouchEvent(ev touchEvent) {
	defer func() {
		pointerMu.Lock()
		evPendingPointers.Set(int64(len(pointerButtons)))
		pointerMu.Unlock()
	}()

	if controlConn == nil {
		return
	}

	// 取映射用的畫面寬高（前端沒帶就用後備）
	stateMu.RLock()
	fallbackW, fallbackH := videoW, videoH
	stateMu.RUnlock()
	sw := ev.ScreenW
	sh := ev.ScreenH
	if sw == 0 || sh == 0 {
		sw, sh = fallbackW, fallbackH
	}

	// 夾住座標
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

	// 轉 action
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

	// ★ 計算送出的 pointerID
	var pointerID uint64
	if ev.PointerType != "touch" {
		// mouse/pen → 永遠使用 0，且忽略 hover move（無按鍵）
		pointerID = 0
		if action == 2 /*move*/ && ev.Buttons == 0 {
			return
		}
	} else {
		// touch → 對 remote ID 映射到 1..10（slot 0..9 對應 1..10；0 保留給滑鼠/pen）
		touchMu.Lock()
		switch action {
		case 0: // down
			if s, ok := allocLocalSlot(ev.ID); ok {
				pointerID = uint64(s + 1) // 1..10
			} else {
				touchMu.Unlock()
				log.Printf("[CTRL][TOUCH] 丟棄 down（超過 %d 指） id=%d", maxPointers, ev.ID)
				return
			}
		case 1, 3: // up/cancel
			if s, ok := getLocalSlot(ev.ID); ok {
				pointerID = uint64(s + 1)
				freeLocalSlot(ev.ID)
			} else {
				touchMu.Unlock()
				return
			}
		default: // move
			if s, ok := getLocalSlot(ev.ID); ok {
				pointerID = uint64(s + 1)
			} else {
				touchMu.Unlock()
				return
			}
		}
		touchMu.Unlock()
	}

	// 計算 action_button / buttons 狀態
	var actionButton uint32
	pointerMu.Lock()
	prevButtons := pointerButtons[pointerID]
	nowButtons := ev.Buttons
	if ev.PointerType == "touch" {
		nowButtons = 0 // 觸控不帶 mouse buttons
	}
	switch action {
	case 0: // down
		actionButton = nowButtons &^ prevButtons
	case 1: // up
		actionButton = prevButtons &^ nowButtons
	default:
		actionButton = 0
	}
	if action == 1 /*up*/ || action == 3 /*cancel*/ {
		delete(pointerButtons, pointerID)
	} else {
		pointerButtons[pointerID] = nowButtons
	}
	pointerMu.Unlock()

	// 壓力（UP 事件強制 0） → u16 fixed-point（官方）
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

	// ====== 官方線路格式（總 32 bytes）======
	// [0]    : type = 2 (INJECT_TOUCH_EVENT)
	// [1]    : action (u8)
	// [2:10] : pointerId (i64)
	// [10:14]: x (i32)
	// [14:18]: y (i32)
	// [18:20]: screenW (u16)
	// [20:22]: screenH (u16)
	// [22:24]: pressure (u16 fixed-point)
	// [24:28]: actionButton (i32)
	// [28:32]: buttons (i32)
	buf := make([]byte, 32)
	buf[0] = 2
	buf[1] = action
	binary.BigEndian.PutUint64(buf[2:], pointerID)
	binary.BigEndian.PutUint32(buf[10:], uint32(ev.X))
	binary.BigEndian.PutUint32(buf[14:], uint32(ev.Y))
	binary.BigEndian.PutUint16(buf[18:], sw)
	binary.BigEndian.PutUint16(buf[20:], sh)
	binary.BigEndian.PutUint16(buf[22:], p)
	binary.BigEndian.PutUint32(buf[24:], actionButton)
	binary.BigEndian.PutUint32(buf[28:], nowButtons)

	// 像官方：事件到就直接寫 socket（不合併、不延遲）
	writeFull(buf, criticalWriteTimeout, true)
}

// ========= 伺服器入口 =========

func main() {
	// 進階 log 格式（含毫秒與檔名:行號）
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
	// 暫時開啟日誌以便偵錯
	// log.SetOutput(io.Discard)

	// 初始化 ADB 目標（空字串表示預設設備）
	adbTarget = ""

	log.Println("🚀 啟動 scrcpy WebRTC 服務...")

	// 初始化 HTTP 路由與服務
	initHTTP()

	log.Println("✅ HTTP 服務已啟動，請開啟瀏覽器訪問 http://127.0.0.1:8080")
	log.Println("💡 ADB 連線將在前端觸發時建立")

	// 保持程式運行
	select {}
}

// initHTTP 設定 HTTP 路由與啟動 server
func initHTTP() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
			return
		}
		http.FileServer(http.Dir(".")).ServeHTTP(w, r)
	})
	http.HandleFunc("/offer", handleOffer)
	http.HandleFunc("/set-adb-target", handleSetAdbTarget)
	http.HandleFunc("/debug/stack", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(buf[:n])
	})

	goSafe("http-server", func() {
		addr := ":8080"
		log.Println("[HTTP] 服務啟動:", addr, "（/ , /offer , /debug/pprof , /debug/vars , /debug/stack）")
		srv := &http.Server{Addr: addr}
		log.Fatal(srv.ListenAndServe())
	})
}

// connectToDevice 連線到 Android 裝置並啟動 scrcpy server，回傳 video/control streams
func connectToDevice() (io.ReadCloser, io.ReadWriter, error) {
	dev, err := adb.NewDevice(adbTarget)
	if err != nil {
		return nil, nil, fmt.Errorf("[ADB] NewDevice(%s): %w", adbTarget, err)
	}
	if err := dev.Reverse("localabstract:scrcpy", fmt.Sprintf("tcp:%d", adb.ScrcpyPort)); err != nil {
		return nil, nil, fmt.Errorf("[ADB] reverse: %w", err)
	}
	if err := dev.PushServer("./assets/scrcpy-server"); err != nil {
		return nil, nil, fmt.Errorf("[ADB] push server: %w", err)
	}
	conn, err := dev.StartServer()
	if err != nil {
		return nil, nil, fmt.Errorf("[ADB] start server: %w", err)
	}
	log.Println("[ADB] 已連上 scrcpy server")
	return conn.VideoStream, conn.Control, nil
}

// startControlHealthLoop 週期性檢查 control 讀回，必要時發送 GET_CLIPBOARD 心跳
func startControlHealthLoop() {
	t := time.NewTicker(controlHealthTick)
	defer t.Stop()
	for range t.C {
		if controlConn == nil {
			continue
		}
		ms := time.Since(lastCtrlRead).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		evLastCtrlReadMsAgo.Set(ms)

		if time.Since(lastCtrlRead) > controlStaleAfter {
			// 送一個 GET_CLIPBOARD 促使 server 回傳，確認雙向通暢
			sendGetClipboard(0) // copyKey=COPY_KEY_NONE
			evHeartbeatSent.Add(1)
		}
	}
}

// startVideoLoop 處理視訊 header 與接收幀迴圈
func startVideoLoop(videoStream io.ReadCloser) {
	// 跳過裝置名稱 (64 bytes, NUL 結尾)
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(videoStream, nameBuf); err != nil {
		log.Fatal("[VIDEO] read device name:", err)
	}
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	log.Printf("[VIDEO] 裝置名稱: %s", deviceName)

	// 視訊標頭 (12 bytes)：[codecID(u32)][w(u32)][h(u32)]
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(videoStream, vHeader); err != nil {
		log.Fatal("[VIDEO] read video header:", err)
	}
	codecID := binary.BigEndian.Uint32(vHeader[0:4]) // 0=H264, 1=H265, 2=AV1（依版本可能不同）
	w0 := binary.BigEndian.Uint32(vHeader[4:8])
	h0 := binary.BigEndian.Uint32(vHeader[8:12])

	stateMu.Lock()
	videoW, videoH = uint16(w0), uint16(h0) // 後備觸控映射空間
	stateMu.Unlock()
	evVideoW.Set(int64(videoW))
	evVideoH.Set(int64(videoH))

	log.Printf("[VIDEO] 編碼ID: %d, 初始解析度: %dx%d", codecID, w0, h0)

	// 視訊流已準備就緒，現在可以安全地請求關鍵幀
	log.Println("[VIDEO] 視訊流初始化完成，請求初始關鍵幀...")
	go func() {
		time.Sleep(500 * time.Millisecond) // 短暫延遲確保一切就緒
		requestKeyframe()
		evKeyframeRequests.Add(1)
	}()

	// 接收幀迴圈（多數版本：meta 12 bytes：[PTS(u64)] + [size(u32)]）
	meta := make([]byte, 12)
	startTime = time.Now()
	var frameCount int
	var totalBytes int64

	for {
		// frame meta
		t0 := time.Now()
		if _, err := io.ReadFull(videoStream, meta); err != nil {
			log.Println("[VIDEO] read frame meta:", err)
			break
		}
		metaElapsed := time.Since(t0)
		evLastFrameMetaMS.Set(metaElapsed.Milliseconds())
		if metaElapsed > warnFrameMetaOver {
			log.Printf("[VIDEO] 讀 meta 偏慢: %v", metaElapsed)
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
		t1 := time.Now()
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(videoStream, frame); err != nil {
			log.Println("[VIDEO] read frame:", err)
			break
		}
		readElapsed := time.Since(t1)
		evLastFrameReadMS.Set(readElapsed.Milliseconds())
		if readElapsed > warnFrameReadOver {
			log.Printf("[VIDEO] 讀 frame 偏慢: %v (size=%d)", readElapsed, frameSize)
		}

		// 解析 Annex-B → NALUs，並快取 SPS/PPS、偵測是否含 IDR
		nalus := splitAnnexBNALUs(frame)

		idrInThisAU := false
		var gotNewSPS bool
		var spsCnt, ppsCnt, idrCnt, othersCnt int

		for _, n := range nalus {
			switch naluType(n) {
			case 7: // SPS
				spsCnt++
				stateMu.Lock()
				if !bytes.Equal(lastSPS, n) {
					if w, h, ok := parseH264SPSDimensions(n); ok {
						videoW, videoH = w, h
						gotNewSPS = true
						evVideoW.Set(int64(videoW))
						evVideoH.Set(int64(videoH))
						log.Printf("[AU] 更新 SPS 並套用解析度 %dx%d 給觸控映射(後備)", w, h)
					}
				}
				lastSPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 8: // PPS
				ppsCnt++
				stateMu.Lock()
				if !bytes.Equal(lastPPS, n) {
					log.Printf("[AU] 更新 PPS (len=%d)", len(n))
				}
				lastPPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 5: // IDR
				idrCnt++
				idrInThisAU = true
			default:
				othersCnt++
			}
		}
		evNALU_SPS.Add(int64(spsCnt))
		evNALU_PPS.Add(int64(ppsCnt))
		evNALU_IDR.Add(int64(idrCnt))
		evNALU_Others.Add(int64(othersCnt))

		// 狀態
		stateMu.RLock()
		vt := videoTrack
		pk := packetizer
		waitKF := needKeyframe
		stateMu.RUnlock()

		// 推進 WebRTC
		if vt != nil && pk != nil {
			// 若剛換解析度，只是標記需要關鍵幀，不立即發送 SPS/PPS
			if gotNewSPS {
				log.Printf("[AU] 偵測到新 SPS，標記需要關鍵幀")
				stateMu.Lock()
				needKeyframe = true
				stateMu.Unlock()
				requestKeyframe()
				evKeyframeRequests.Add(1)
				waitKF = true // 更新本地狀態
			}

			if waitKF {
				framesSinceKF++
				evFramesSinceKF.Set(int64(framesSinceKF))

				if !idrInThisAU {
					// 等待 IDR 期間，每 30 幀重新請求一次關鍵幀
					if framesSinceKF%30 == 0 {
						log.Printf("[KF] 等待 IDR 中... 已過 %d 幀；再次請求關鍵幀", framesSinceKF)
						requestKeyframe()
						evKeyframeRequests.Add(1)
					}
					goto stats // 跳過發送，繼續等待 IDR
				}

				// 收到 IDR，發送完整的 Access Unit (SPS + PPS + IDR + ...)
				log.Println("[KF] 偵測到 IDR，發送完整 Access Unit")
				stateMu.Lock()
				needKeyframe = false
				framesSinceKF = 0
				stateMu.Unlock()
				evFramesSinceKF.Set(0)

				// 先發送 SPS/PPS，再發送整個 AU
				stateMu.RLock()
				sps := lastSPS
				pps := lastPPS
				stateMu.RUnlock()

				if len(sps) > 0 && len(pps) > 0 {
					// 將 SPS/PPS 與當前 AU 合併發送
					completeAU := make([][]byte, 0, len(nalus)+2)
					completeAU = append(completeAU, sps, pps)
					completeAU = append(completeAU, nalus...)
					sendNALUAccessUnitAtTS(completeAU, curTS)
				} else {
					sendNALUAccessUnitAtTS(nalus, curTS)
				}
			} else {
				sendNALUAccessUnitAtTS(nalus, curTS)
			}
		}

	stats:
		frameCount++
		totalBytes += int64(frameSize)
		evFramesRead.Add(1)
		evBytesRead.Add(int64(frameSize))

		if frameCount%statsLogEvery == 0 {
			elapsed := time.Since(startTime).Seconds()
			bytesPerSecond := float64(totalBytes) / elapsed
			stateMu.RLock()
			lp := lastPLI
			pc := pliCount
			stateMu.RUnlock()
			log.Printf("[STATS] 影格: %d, 速率: %.2f MB/s, PLI 累計: %d (last=%s), AUseq=%d",
				frameCount, bytesPerSecond/(1024*1024), pc, lp.Format(time.RFC3339), auSeq)
		}

		// 下一 AU 序號
		stateMu.Lock()
		auSeq++
		stateMu.Unlock()
		evAuSeq.Set(int64(auSeq))
	}
}

// === HTTP: /set-adb-target handler ===
func handleSetAdbTarget(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	stateMu.Lock()
	adbTarget = req.Target
	stateMu.Unlock()

	log.Printf("[ADB] 目標已設定為: %s", adbTarget)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"target": adbTarget,
	})
}

// === WebRTC: /offer handler ===
func handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid offer", http.StatusBadRequest)
		return
	}

	log.Printf("🔌 收到 WebRTC offer，開始建立 ADB 連線 (目標: %s)", adbTarget)

	// 建立 ADB 連線
	videoStream, controlStream, err := connectToDevice()
	if err != nil {
		log.Printf("❌ ADB 連線失敗: %v", err)
		http.Error(w, fmt.Sprintf("ADB connection failed: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("✅ ADB 連線成功，開始設定 WebRTC")

	// 設定全域控制連線
	controlConn = controlStream

	// 啟動控制通道處理
	goSafe("control-reader", func() {
		defer func() {
			if c, ok := controlStream.(io.Closer); ok {
				c.Close()
			}
		}()
		readDeviceMessages(controlConn)
	})

	// 啟動控制健康檢查
	goSafe("control-health", startControlHealthLoop)

	// 啟動視訊處理
	goSafe("video-loop", func() {
		defer videoStream.Close()
		startVideoLoop(videoStream)
	})

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
	evActivePeer.Set(1)

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

	// 讀 RTCP：PLI / FIR
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
					stateMu.Lock()
					needKeyframe = true
					lastPLI = time.Now()
					pliCount++
					stateMu.Unlock()
					evRTCP_PLI.Add(1)
					evPLICount.Set(int64(pliCount))
					requestKeyframe()
					evKeyframeRequests.Add(1)
				case *rtcp.FullIntraRequest:
					log.Printf("[RTCP] 收到 FIR → needKeyframe = true (SenderSSRC=%d, MediaSSRC=%d)", p.SenderSSRC, p.MediaSSRC)
					stateMu.Lock()
					needKeyframe = true
					lastPLI = time.Now()
					pliCount++
					stateMu.Unlock()
					evRTCP_FIR.Add(1)
					evPLICount.Set(int64(pliCount))
					requestKeyframe()
					evKeyframeRequests.Add(1)
				}
			}
		}
	})

	// 接前端 DataChannel（印原始資料 → 解析 → 注入）
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Println("[RTC] DataChannel:", dc.Label())

		dc.OnOpen(func() { log.Println("[RTC] DC open:", dc.Label()) })
		dc.OnClose(func() { log.Println("[RTC] DC close:", dc.Label()) })

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Printf("[RTC][DC:%s] recv isString=%v len=%d", dc.Label(), msg.IsString, len(msg.Data))
			if msg.IsString {
				s := string(msg.Data)
				if len(s) > 512 {
					log.Printf("[RTC][DC:%s] data: %s ...(剩餘 %d 字元)", dc.Label(), s[:512], len(s)-512)
				} else {
					log.Printf("[RTC][DC:%s] data: %s", dc.Label(), s)
				}
			} else {
				n := 128
				if len(msg.Data) < n {
					n = len(msg.Data)
				}
				hex := fmt.Sprintf("% x", msg.Data[:n])
				if n < len(msg.Data) {
					log.Printf("[RTC][DC:%s] data(hex %dB): %s ...(剩餘 %d bytes)", dc.Label(), n, hex, len(msg.Data)-n)
				} else {
					log.Printf("[RTC][DC:%s] data(hex): %s", dc.Label(), hex)
				}
			}

			var ev touchEvent
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				log.Printf("[RTC][DC:%s] json.Unmarshal 失敗：%v", dc.Label(), err)
				return
			}
			log.Printf("[CTRL] touch: type=%s id=%d x=%d y=%d pressure=%.3f buttons=%d pointerType=%s screen=%dx%d",
				ev.Type, ev.ID, ev.X, ev.Y, ev.Pressure, ev.Buttons, ev.PointerType, ev.ScreenW, ev.ScreenH)

			handleTouchEvent(ev)
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Println("[RTC] PeerConnection state:", s.String())
		if s == webrtc.PeerConnectionStateFailed ||
			s == webrtc.PeerConnectionStateClosed ||
			s == webrtc.PeerConnectionStateDisconnected {
			stateMu.Lock()
			videoTrack = nil
			packetizer = nil
			stateMu.Unlock()
			evActivePeer.Set(0)
		}
	})

	// 設定 Remote SDP / Answer / 等待 ICE（非 trickle）
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

	log.Println("[WebRTC] packetizer 初始化完成，等待視訊流請求關鍵幀...")

	// 回傳 Answer（含 ICE）
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pc.LocalDescription())
}

// 要求 Android 重新送出關鍵幀
func requestKeyframe() {
	if controlConn == nil {
		log.Println("[CTRL] requestKeyframe: controlConn is nil")
		return
	}
	controlMu.Lock()
	defer controlMu.Unlock()

	// 添加寫入超時以避免阻塞
	if conn, ok := controlConn.(net.Conn); ok {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer conn.SetWriteDeadline(time.Time{}) // 清除超時
	}

	// 控制訊息：TYPE_RESET_VIDEO 僅 1 byte
	if _, err := controlConn.Write([]byte{controlMsgResetVideo}); err != nil {
		log.Printf("[CTRL] send RESET_VIDEO failed: %v", err)
	} else {
		log.Println("[CTRL] 已送出 RESET_VIDEO")
	}
}

// 主動向 server 要求回傳剪貼簿（作為健康心跳）
func sendGetClipboard(copyKey byte) {
	if controlConn == nil {
		return
	}
	controlMu.Lock()
	defer controlMu.Unlock()
	// [type=8][copyKey=1B]
	if _, err := controlConn.Write([]byte{controlMsgGetClipboard, copyKey}); err != nil {
		log.Println("[CTRL] send GET_CLIPBOARD:", err)
	} else {
		log.Println("[CTRL] 已送出 GET_CLIPBOARD (heartbeat)")
	}
}

// === 控制通道讀回（DeviceMessage）===
// 目前解析 TYPE_CLIPBOARD： [type(1)][len(4 BE)][utf8 bytes]
func readDeviceMessages(r io.Reader) {
	buf := make([]byte, 0, 4096)
	readU8 := func() (byte, error) {
		var b [1]byte
		_, err := io.ReadFull(r, b[:])
		return b[0], err
	}
	readU32BE := func() (uint32, error) {
		var b [4]byte
		_, err := io.ReadFull(r, b[:])
		return binary.BigEndian.Uint32(b[:]), err
	}

	for {
		typ, err := readU8()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if err == io.EOF {
				log.Println("[CTRL][READ] EOF")
			} else {
				log.Println("[CTRL][READ] error:", err)
			}
			evCtrlReadsErr.Add(1)
			return
		}

		switch typ {
		case deviceMsgTypeClipboard:
			// len + data
			n, err := readU32BE()
			if err != nil {
				log.Println("[CTRL][READ] clipboard len err:", err)
				evCtrlReadsErr.Add(1)
				return
			}
			if n > controlReadBufMax {
				log.Printf("[CTRL][READ] clipboard 太大: %d > %d，截斷丟棄", n, controlReadBufMax)
				// 丟棄 n bytes
				if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
					log.Println("[CTRL][READ] discard err:", err)
					evCtrlReadsErr.Add(1)
					return
				}
				continue
			}
			if cap(buf) < int(n) {
				buf = make([]byte, n)
			} else {
				buf = buf[:n]
			}
			if _, err := io.ReadFull(r, buf[:n]); err != nil {
				log.Println("[CTRL][READ] clipboard data err:", err)
				evCtrlReadsErr.Add(1)
				return
			}
			lastCtrlRead = time.Now()
			evCtrlReadsOK.Add(1)
			evCtrlReadClipboardB.Add(int64(n))
			log.Printf("[CTRL][READ] DeviceMessage.CLIPBOARD %dB: %q", n, trimString(string(buf[:n]), 200))
		default:
			// 未知型別：無長度資訊 → 無法安全跳過，只記錄
			lastCtrlRead = time.Now()
			evCtrlReadsOK.Add(1)
			log.Printf("[CTRL][READ] 未知 DeviceMessage type=%d（未解析，可能為未來版本）", typ)
		}
	}
}

func trimString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// === RTP 發送（以指定 TS）===
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
		pkts := pk.Packetize(n, 0) // samples=0，手動覆寫 Timestamp
		for j, p := range pkts {
			p.Timestamp = ts
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
			if err := vt.WriteRTP(p); err != nil {
				log.Println("[RTP] write error:", err)
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
	for i, n := range nalus {
		if len(n) == 0 {
			continue
		}
		pkts := pk.Packetize(n, 0)
		for j, p := range pkts {
			p.Timestamp = ts
			// 最後一個 NALU 的最後一個封包才設 marker
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
			if err := vt.WriteRTP(p); err != nil {
				log.Println("[RTP] write error:", err)
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

// === H.264 SPS 解析寬高（極簡）===
type bitReader struct {
	b []byte
	i int // bit index
}

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
			// 粗略跳過 scaling_list
			n := 8
			if chromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if g, ok3 := br.u(1); !ok3 {
					return
				} else if g == 1 {
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

	// crop 單位
	var subW, subH uint = 1, 1
	switch chromaFormatIDC {
	case 0:
		subW, subH = 1, 1
	case 1:
		subW, subH = 2, 2
	case 2:
		subW, subH = 2, 1
	case 3:
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

// --- bitReader ---
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
func (br *bitReader) skip(n int) bool { _, ok := br.u(n); return ok }
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
