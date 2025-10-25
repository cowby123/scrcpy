package main

import (
	"expvar"
	"sync"
	"time"
)

// ====== 常量定義 ======
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
	stateMu sync.RWMutex

	startTime time.Time // 速率統計

	// 觀測 PLI/FIR（保留作為全域觀測）
	lastPLI  time.Time
	pliCount int
	auSeq    uint64

	// 目前「視訊解析度」（僅作後備；主要用前端傳入的 screenW/H）
	videoW uint16
	videoH uint16
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
	evFramesDropped      = expvar.NewInt("frames_dropped_on_send") // ★ 新增：觀測發送端丟幀
)
