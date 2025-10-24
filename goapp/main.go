// main.go — 線路格式對齊官方：觸控載荷 31B + type 1B = 總 32B；
// 觸控 ≤10 指映射；mouse/pen 固定用 ID 0；忽略滑鼠 hover move；
// DataChannel 收到控制訊號時印出原始資料並照官方順序編碼後直寫 control socket。
// ★ 新增：control socket 讀回解析（DeviceMessage：clipboard）、心跳 GET_CLIPBOARD、寫入 deadline/耗時告警。
// ★ 效能：解耦 ADB 讀取與 WebRTC 寫入，使用 channel 避免 I/O 阻塞；統一控制寫入 deadline。
// ★ v3: 縮小 channel 緩衝區以降低延遲。

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
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

// registerADBFlags 註冊 ADB 相關的命令行參數並返回配置選項獲取函數
// 用途：配置 ADB 連接參數，包括設備序號、伺服器主機、端口等
func registerADBFlags(fs *flag.FlagSet) func() adb.Options {
	// 設備序號或 IP:Port，例如：
	// - USB 設備：序號如 "ABCD1234"
	// - WiFi 設備：IP:Port 如 "192.168.1.100:5555"
	// - 留空則使用第一個可用設備
	serial := fs.String("device", "", "指定要連接的 Android 設備 (序號或 IP:Port，留空則自動選擇)")
	
	// 相容舊參數名稱
	serialAlt := fs.String("adb-serial", "", "同 -device (相容舊版)")
	
	// ADB 伺服器設定（通常使用預設值即可）
	host := fs.String("adb-host", "127.0.0.1", "ADB 伺服器主機位址")
	port := fs.Int("adb-port", 5037, "ADB 伺服器端口")
	
	// scrcpy 本地端口設定
	scrcpyPort := fs.Int("scrcpy-port", adb.DefaultScrcpyPort, "scrcpy 反向連接使用的本地端口")
	
	return func() adb.Options {
		// 優先使用 -device，若未設定則嘗試 -adb-serial（相容性）
		selectedSerial := *serial
		if selectedSerial == "" && *serialAlt != "" {
			selectedSerial = *serialAlt
		}
		
		return adb.Options{
			Serial:     selectedSerial,
			ServerHost: *host,
			ServerPort: *port,
			ScrcpyPort: *scrcpyPort,
		}
	}
}

func runAndroidStreaming(deviceOpts adb.Options) {
	session, conn, err := StartScrcpyBoot(deviceOpts)
	if err != nil {
		log.Fatal("[ADB] setup:", err)
	}
	scrcpySession = session
	defer scrcpySession.Close()
	log.Printf("[ADB] target serial=%q scrcpy_port=%d", deviceOpts.Serial, scrcpySession.ScrcpyPort())
	log.Println("[ADB] scrcpy server 已啟動")

	// === 6. 啟動 RTP 發送器 ===
	// 啟動專門的 goroutine 處理 RTP 封包發送，與視訊讀取解耦合
	goSafe("rtp-sender", func() {
		log.Println("[RTP] rtp-sender 啟動")
		// 不斷從通道取得 RTP 負載並發送
		for payload := range frameChannel {
			if payload.IsAccessUnit {
				// 發送整個存取單元 (AU)，Marker bit 會在最後一個 NALU 與最後一個 packet 上
				sendNALUAccessUnitAtTS(payload.NALUs, payload.RTPTimestamp)
			} else {
				// 發送單獨的 NALU (例如 SPS/PPS 參數)，不設定 Marker bit
				sendNALUsAtTS(payload.RTPTimestamp, payload.NALUs...)
			}
		}
		log.Println("[RTP] rtp-sender 結束")
	})

	// === 7. 開始視訊串流讀取 ===
	log.Println("[VIDEO] 開始接收視訊串流")

	// 讀取設備名稱（固定 64 位元組，以 NUL 結尾）
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		log.Fatal("[VIDEO] read device name:", err)
	}
	// 去除尾端的 NUL 字元並取得設備名稱
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	log.Printf("[VIDEO] 裝置名稱: %s", deviceName)

	// 讀取視訊格式資訊（12 位元組）：[codecID(u32)][width(u32)][height(u32)]
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Fatal("[VIDEO] read video header:", err)
	}
	// 解析編碼 ID 與初始解析度
	codecID := binary.BigEndian.Uint32(vHeader[0:4]) // 0=H264, 1=H265, 2=AV1（此版本主要支援 H264）
	w0 := binary.BigEndian.Uint32(vHeader[4:8])      // 視訊寬度
	h0 := binary.BigEndian.Uint32(vHeader[8:12])     // 視訊高度

	// 更新當前視訊解析度，用於觸控事件的座標換算
	stateMu.Lock()
	videoW, videoH = uint16(w0), uint16(h0)
	stateMu.Unlock()
	// 更新觀測指標
	evVideoW.Set(int64(videoW))
	evVideoH.Set(int64(videoH))

	log.Printf("[VIDEO] 編碼ID: %d, 初始解析度: %dx%d", codecID, w0, h0)

	// === 8. 主要視訊資料讀取與處理迴圈 ===
	// 初始化讀取與處理所需的變數
	meta := make([]byte, 12) // 幀資訊緩衝（PTS + 幀大小）
	startTime = time.Now()   // 紀錄開始時間，用於計算處理速率
	var frameCount int       // 已處理幀數統計
	var totalBytes int64     // 累積處理大小

	// 連續迴圈處理每一幀視訊
	for {
		// === 8.1 讀取幀資訊 ===
		// 測量讀取幀資訊耗時
		t0 := time.Now()
		if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
			log.Println("[VIDEO] read frame meta:", err)
			break // 讀取失敗時結束迴圈
		}
		metaElapsed := time.Since(t0)
		evLastFrameMetaMS.Set(metaElapsed.Milliseconds())
		// 若讀取時間過長，輸出警告
		if metaElapsed > warnFrameMetaOver {
			log.Printf("[VIDEO] 讀 meta 偏慢: %v", metaElapsed)
		}

		// 解析幀資訊：[PTS(u64)] + [frameSize(u32)]
		pts := binary.BigEndian.Uint64(meta[0:8])        // 呈現時間戳（微秒）
		frameSize := binary.BigEndian.Uint32(meta[8:12]) // 幀大小

		// === 8.2 初始化時間對齊 ===
		// 第一幀時建立 PTS 與 RTP 時間戳的對應關係
		if !havePTS0 {
			pts0 = pts // 記下第一幀的 PTS 作為基準
			rtpTS0 = 0 // RTP 時間戳從 0 開始
			havePTS0 = true
		}
		// 計算當前幀的 RTP 時間戳
		curTS := rtpTS0 + rtpTSFromPTS(pts, pts0)

		// === 8.3 讀取幀資料 ===
		// 測量讀取幀資料耗時
		t1 := time.Now()
		frame := make([]byte, frameSize) // 配置幀資料緩衝
		if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
			log.Println("[VIDEO] read frame:", err)
			break
		}
		readElapsed := time.Since(t1)
		evLastFrameReadMS.Set(readElapsed.Milliseconds())
		if readElapsed > warnFrameReadOver {
			log.Printf("[VIDEO] 讀 frame 偏慢: %v (size=%d)", readElapsed, frameSize)
		}

		// === 8.4 解析 NALUs ===
		// 解析 Annex-B 格式的 NALU，並檢測是否包含 IDR
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
						log.Printf("[AU] 新的 SPS 並成功解析解析度 %dx%d", w, h)
					}
				}
				lastSPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 8: // PPS
				ppsCnt++
				stateMu.Lock()
				if !bytes.Equal(lastPPS, n) {
					log.Printf("[AU] 新的 PPS (len=%d)", len(n))
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

		// === 8.5 檢查並處理關鍵幀等待狀態 ===
		stateMu.RLock()
		waitKF := needKeyframe // 使用全域狀態，而不是每次重設
		stateMu.RUnlock()

		// === 8.6 處理 SPS 變更情況 ===
		if gotNewSPS {
			// 推送最新的 SPS/PPS 參數
			stateMu.RLock()
			sps := lastSPS
			pps := lastPPS
			stateMu.RUnlock()

			if len(sps) > 0 {
				pushToRTPChannel(RtpPayload{NALUs: [][]byte{sps}, RTPTimestamp: curTS, IsAccessUnit: false})
			}
			if len(pps) > 0 {
				pushToRTPChannel(RtpPayload{NALUs: [][]byte{pps}, RTPTimestamp: curTS, IsAccessUnit: false})
			}

			// 設定等待關鍵幀狀態
			stateMu.Lock()
			needKeyframe = true
			waitKF = true // 更新本地副本
			stateMu.Unlock()

			// 請求關鍵幀（只在 SPS 變更時請求一次）
			if scrcpySession != nil {
				scrcpySession.RequestKeyframe()
			}
			evKeyframeRequests.Add(1)
			log.Println("[KF] 檢測到新 SPS，已請求關鍵幀")
		}

		// === 8.7 處理關鍵幀等待邏輯 ===
		if waitKF {
			stateMu.RLock()
			sps := lastSPS
			pps := lastPPS
			stateMu.RUnlock()

			if len(sps) > 0 && len(pps) > 0 {
				pushToRTPChannel(RtpPayload{NALUs: [][]byte{sps}, RTPTimestamp: curTS, IsAccessUnit: false})
				pushToRTPChannel(RtpPayload{NALUs: [][]byte{pps}, RTPTimestamp: curTS, IsAccessUnit: false})
			} else {
				if scrcpySession != nil {
					scrcpySession.RequestKeyframe()
				}
				evKeyframeRequests.Add(1)
			}

			framesSinceKF++
			evFramesSinceKF.Set(int64(framesSinceKF))

			if framesSinceKF%30 == 0 {
				log.Printf("[KF] 等待 IDR 中... 已經 %d 幀；再次請求 IDR", framesSinceKF)
				if scrcpySession != nil {
					scrcpySession.RequestKeyframe()
				}
				evKeyframeRequests.Add(1)
			}

			if !idrInThisAU {
				goto stats
			}

			log.Println("[KF] 收到 IDR，恢復正常串流")
			stateMu.Lock()
			needKeyframe = false
			framesSinceKF = 0
			stateMu.Unlock()
			evFramesSinceKF.Set(0)

			pushToRTPChannel(RtpPayload{NALUs: nalus, RTPTimestamp: curTS, IsAccessUnit: true})
		} else {
			pushToRTPChannel(RtpPayload{NALUs: nalus, RTPTimestamp: curTS, IsAccessUnit: true})
		}

		// === 8.7 更新統計 ===
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
			dropped := evFramesDropped.Value()
			stateMu.RUnlock()

			log.Printf("[STATS] 幀數: %d, 帶寬: %.2f MB/s, PLI 次數: %d (last=%s), AUseq=%d, 丟棄: %d",
				frameCount, bytesPerSecond/(1024*1024), pc, lp.Format(time.RFC3339), auSeq, dropped)
		}

		// === 8.8 更新串流序號 ===
		stateMu.Lock()
		auSeq++
		stateMu.Unlock()
		evAuSeq.Set(int64(auSeq))
	}
}

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

	// ★ [v3] 效能調校：使用一個非常小的緩衝區 (類似官方 scrcpy 的 1 幀緩衝)
	// 優先保證低延遲，而不是播放平順。這會更積極地丟幀。
	rtpPayloadChannelSize = 3

	controlWriteDefaultTimeout = 50 * time.Millisecond // ★ 控制請求 (PLI/HB) deadline

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

	startTime time.Time // 速率統計

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

var scrcpySession *ScrcpySession

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
	evFramesDropped      = expvar.NewInt("frames_dropped_on_send") // ★ 新增：觀測發送端丟幀
)

// ====== ★ 解耦 Video 讀/寫 ======
// RtpPayload RTP 載荷結構體
// 用途：封裝要透過 RTP 傳輸的視訊資料，包含 NALU 單元、時間戳和存取單元標記
type RtpPayload struct {
	NALUs        [][]byte
	RTPTimestamp uint32
	IsAccessUnit bool // 標記這是否是一個完整的 AU (Access Unit)，用於設定 RTP Marker bit
}

// ★ [v3] 使用了縮小後的 channel size
var frameChannel = make(chan RtpPayload, rtpPayloadChannelSize)

// ★ 推送至 RTP channel，若壅塞則丟棄
// pushToRTPChannel 將 RTP 載荷推送到處理通道
// 用途：非阻塞式推送視訊幀到 RTP 通道，當通道滿時主動丟幀以降低延遲
func pushToRTPChannel(payload RtpPayload) {
	select {
	case frameChannel <- payload:
		// 成功推入
	default:
		// Channel 滿了，代表 WebRTC 端處理不過來
		// 為了降低延遲，**主動丟棄**
		log.Printf("[RTP] 寫入壅塞 (channel 滿)，丟棄 %d NALUs (isAU=%v)", len(payload.NALUs), payload.IsAccessUnit)
		evFramesDropped.Add(1)
	}
}

// ★ 清空 RTP channel (WebRTC 斷線時)
// clearFrameChannel 清空 RTP 幀處理通道
// 用途：當 WebRTC 連接斷開時清空通道中的所有待處理幀，防止記憶體累積
func clearFrameChannel() {
	for {
		select {
		case <-frameChannel:
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

// ====== ★ 觸控 pointer 映射（限制活躍 ≤ 10；ID 0 給 mouse/pen）======
const maxPointers = 10

var touchMu sync.Mutex

// remoteID -> local slot (0..9)
var touchLocalByRemote = map[uint64]uint16{}

// local slot -> remoteID；slot 是否使用
var touchRemoteByLocal [maxPointers]uint64
var touchSlotUsed [maxPointers]bool

// getLocalSlot 根據遠端觸控點 ID 獲取本地槽位編號
// 用途：查找已分配給特定遠端觸控點的本地槽位，用於多點觸控映射
func getLocalSlot(remote uint64) (uint16, bool) {
	if s, ok := touchLocalByRemote[remote]; ok {
		return s, true
	}
	return 0, false
}

// allocLocalSlot 為遠端觸控點 ID 分配本地槽位編號
// 用途：為新的觸控點分配可用的本地槽位，支持最多 10 個同時觸控點
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

// freeLocalSlot 釋放遠端觸控點 ID 對應的本地槽位
// 用途：當觸控點結束（up/cancel）時釋放占用的槽位，供後續觸控點使用
func freeLocalSlot(remote uint64) {
	if s, ok := touchLocalByRemote[remote]; ok {
		delete(touchLocalByRemote, remote)
		idx := int(s)
		touchSlotUsed[idx] = false
		touchRemoteByLocal[idx] = 0
	}
}

// 寫入控制 socket：**一定寫完整個封包**，並可選設置 write deadline（避免長時間阻塞）
// ★ 修改：回傳 error
// ====== 前端事件（JSON）→ 官方線路格式（32 bytes）======
// touchEvent 觸控事件結構體
// 用途：定義從前端 WebRTC DataChannel 接收的觸控事件資料格式，包含座標、壓力、按鍵狀態等
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

// handleTouchEvent 處理前端發送的觸控事件
// 用途：將 WebRTC DataChannel 收到的觸控事件轉換為 scrcpy 控制指令並發送給 Android 設備
func handleTouchEvent(ev touchEvent) {
	defer func() {
		pointerMu.Lock()
		evPendingPointers.Set(int64(len(pointerButtons)))
		pointerMu.Unlock()
	}()

	if scrcpySession == nil || scrcpySession.ControlConn() == nil {
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
	// [0]    : type = 2 (INJECT_TOUCH_EVENT)
	// [1]    : action (u8)
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
	// ★ 修改：忽略 writeFull 的 error (日誌由 writeFull 內部處理)
	if scrcpySession != nil {
		_ = scrcpySession.writeFull(buf, criticalWriteTimeout, true)
	}
}

// ========= 伺服器入口 =========

// main 程式主入口函數
// 用途：初始化 HTTP 伺服器、建立 ADB 連接、處理視訊串流和 WebRTC 連接
func main() {
	// === 1. 解析命令行參數 ===
	// 註冊 ADB 相關的命令行標誌並獲取配置函數
	getADBOptions := registerADBFlags(flag.CommandLine)
	// 解析所有命令行參數
	flag.Parse()

	// === 2. 配置日誌格式 ===
	// 設定進階 log 格式（含毫秒與檔名:行號），方便除錯
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	// === 3. 設定 HTTP 路由處理器 ===
	// 根路由：提供靜態檔案服務，主要用於前端頁面
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// 直接提供 index.html 作為主頁
			http.ServeFile(w, r, "index.html")
			return
		}
		// 其他路徑使用檔案伺服器提供靜態資源
		http.FileServer(http.Dir(".")).ServeHTTP(w, r)
	})
	// WebRTC SDP 交換端點
	http.HandleFunc("/offer", handleOffer)
	// 除錯用的堆疊追蹤端點
	http.HandleFunc("/debug/stack", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(buf[:n])
	})

	// === 4. 啟動 HTTP 伺服器 ===
	// 使用安全的 goroutine 啟動 HTTP 伺服器（防止 panic 導致程式崩潰）
	goSafe("http-server", func() {
		addr := ":8080"
		log.Println("[HTTP] 服務啟動:", addr, "（/ , /offer , /debug/pprof , /debug/vars , /debug/stack）")
		srv := &http.Server{Addr: addr}
		log.Fatal(srv.ListenAndServe())
	})

	// === 5. 建立與 Android 設備的連接 ===
	deviceOpts := getADBOptions()
	
	// 顯示連接資訊
	if deviceOpts.Serial != "" {
		log.Printf("[ADB] 嘗試連接指定設備: %s", deviceOpts.Serial)
	} else {
		log.Println("[ADB] 未指定設備，將自動選擇第一個可用設備")
		log.Println("[ADB] 提示：使用 -device 參數指定設備，例如：")
		log.Println("[ADB]   USB 設備: -device ABCD1234")
		log.Println("[ADB]   WiFi 設備: -device 192.168.1.100:5555")
	}
	
	runAndroidStreaming(deviceOpts)
}

// === WebRTC: /offer handler ===
// handleOffer 處理 WebRTC 的 SDP offer 請求
// 用途：建立 WebRTC 連接，配置視訊軌道和數據通道，處理 RTCP 反饋和觸控事件
func handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid offer", http.StatusBadRequest)
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
					log.Println("[RTCP] 收到 PLI → needKeyframe = true")
					stateMu.Lock()
					needKeyframe = true
					lastPLI = time.Now()
					pliCount++
					stateMu.Unlock()
					evRTCP_PLI.Add(1)
					evPLICount.Set(int64(pliCount))
					if scrcpySession != nil {
						scrcpySession.RequestKeyframe()
					}
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
					if scrcpySession != nil {
						scrcpySession.RequestKeyframe()
					}
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

			// ★ 清空 channel，避免 main 迴圈阻塞並累積舊幀
			log.Println("[RTP] WebRTC 斷線，清空 frameChannel")
			clearFrameChannel()
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

	// 立刻請求關鍵幀
	if scrcpySession != nil {
		scrcpySession.RequestKeyframe()
	}
	evKeyframeRequests.Add(1)

	// 回傳 Answer（含 ICE）
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pc.LocalDescription())
}

// 要求 Android 重新送出關鍵幀
// ★ 修改：使用 writeFull 並設定 deadline
// 主動向 server 要求回傳剪貼簿（作為健康心跳）
// ★ 修改：使用 writeFull 並設定 deadline
// === 控制通道讀回（DeviceMessage）===
// 目前解析 TYPE_CLIPBOARD： [type(1)][len(4 BE)][utf8 bytes]
// trimString 截斷字串到指定長度並添加省略號
// 用途：限制日誌輸出中字串的長度，防止過長的文字影響日誌可讀性
func trimString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// === RTP 發送（以指定 TS）===
// ★ 注意：這些函式現在由 'rtp-sender' goroutine 唯一呼叫
// sendNALUAccessUnitAtTS 以指定時間戳發送完整的 NALU 存取單元
// 用途：將 H.264 存取單元（AU）打包成 RTP 封包並透過 WebRTC 發送，在最後一個封包設置 Marker bit
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
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1) // ★ Marker bit
			if err := vt.WriteRTP(p); err != nil {
				log.Println("[RTP] write error:", err)
			}
		}
	}
}

// sendNALUsAtTS 以指定時間戳發送單獨的 NALU 單元
// 用途：發送獨立的 NALU（如 SPS/PPS 參數集），不設置 Marker bit，用於參數集傳輸
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
			p.Marker = false // ★ 非 AU 結尾
			if err := vt.WriteRTP(p); err != nil {
				log.Println("[RTP] write error:", err)
			}
		}
	}
}

// === Annex-B 工具 ===
// splitAnnexBNALUs 將 Annex-B 格式的位元流分割為獨立的 NALU 單元
// 用途：解析 H.264 Annex-B 格式的視訊流，提取各個 NALU 單元供後續處理
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

// findStartCode 在位元組陣列中尋找 H.264 起始碼
// 用途：查找 Annex-B 格式中的 NALU 起始碼（00 00 01 或 00 00 00 01）
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

// naluType 取得 NALU 的類型
// 用途：從 NALU 標頭的低 5 位元提取 NALU 類型（SPS=7, PPS=8, IDR=5 等）
func naluType(n []byte) uint8 {
	if len(n) == 0 {
		return 0
	}
	return n[0] & 0x1F
}

// === PTS → RTP TS 轉換 ===
// rtpTSFromPTS 將 PTS（呈現時間戳）轉換為 RTP 時間戳
// 用途：將 scrcpy 的微秒 PTS 轉換為 RTP 標準的 90kHz 時間戳格式
func rtpTSFromPTS(pts, base uint64) uint32 {
	delta := pts - base
	return uint32((delta * 90000) / ptsPerSecond) // 90kHz * 秒數
}

// === H.264 SPS 解析寬高（極簡）===
// bitReader 位元流讀取器結構體
// 用途：用於逐位讀取 H.264 RBSP 資料，支援解析 SPS 中的各種編碼欄位
type bitReader struct {
	b []byte
	i int // bit index
}

// parseH264SPSDimensions 解析 H.264 SPS 中的視訊尺寸資訊
// 用途：從 SPS（序列參數集）中提取視訊的寬度和高度，用於觸控座標映射
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
// u 從位元流中讀取指定位數的無符號整數
// 用途：從 H.264 RBSP 位元流中按位讀取數據，用於解析 SPS 參數
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

// skip 跳過位元流中指定位數的資料
// 用途：在解析 SPS 時跳過不需要的欄位，簡化位元流解析邏輯
func (br *bitReader) skip(n int) bool { _, ok := br.u(n); return ok }

// ue 讀取 Exp-Golomb 無符號編碼值
// 用途：解析 H.264 標準中的 ue(v) 指數哥倫布編碼，用於讀取 SPS 中的各種參數
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

// se 讀取 Exp-Golomb 有符號編碼值
// 用途：解析 H.264 標準中的 se(v) 指數哥倫布有符號編碼，用於讀取可能為負數的 SPS 參數
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
