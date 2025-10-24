package stream

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/cowby123/scrcpy-go/adb"
	"github.com/cowby123/scrcpy-go/internal/device"
	"github.com/cowby123/scrcpy-go/internal/video"
)

// Config 視訊流配置
type Config struct {
	WarnFrameMetaOver time.Duration
	WarnFrameReadOver time.Duration
	SlowThreshold     int
}

// Reader 視訊流讀取器
type Reader struct {
	deviceSession *device.DeviceSession
	config        Config

	slowMetaCount  int
	slowFrameCount int
}

// NewReader 創建視訊流讀取器
func NewReader(ds *device.DeviceSession, cfg Config) *Reader {
	return &Reader{
		deviceSession: ds,
		config:        cfg,
	}
}

// ReadVideoHeader 讀取視訊頭部資訊（設備名稱和編碼資訊）
func (r *Reader) ReadVideoHeader(conn *adb.ServerConn) (deviceName string, codecID uint32, w, h int, err error) {
	// 讀取設備名稱（固定 64 位元組）
	nameBuf := make([]byte, 64)
	if _, err = io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		return "", 0, 0, 0, fmt.Errorf("read device name: %w", err)
	}

	// 找到 NUL 結尾
	nameEnd := 0
	for i, b := range nameBuf {
		if b == 0 {
			nameEnd = i
			break
		}
	}
	deviceName = string(nameBuf[:nameEnd])

	// 讀取視訊頭部（12 位元組）
	vHeader := make([]byte, 12)
	if _, err = io.ReadFull(conn.VideoStream, vHeader); err != nil {
		return "", 0, 0, 0, fmt.Errorf("read video header: %w", err)
	}

	codecID = binary.BigEndian.Uint32(vHeader[0:4])
	w = int(binary.BigEndian.Uint32(vHeader[4:8]))
	h = int(binary.BigEndian.Uint32(vHeader[8:12]))

	return deviceName, codecID, w, h, nil
}

// FrameInfo 幀資訊
type FrameInfo struct {
	PTS       uint64
	FrameSize uint32
	Data      []byte
}

// ReadFrame 讀取單個視訊幀
func (r *Reader) ReadFrame(conn *adb.ServerConn) (*FrameInfo, error) {
	// 讀取幀資訊（12 bytes: PTS + FrameSize）
	meta := make([]byte, 12)

	t0 := time.Now()
	if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
		return nil, fmt.Errorf("read frame meta: %w", err)
	}
	metaElapsed := time.Since(t0)

	// 檢查是否連續慢
	if metaElapsed > r.config.WarnFrameMetaOver {
		r.slowMetaCount++
		if r.slowMetaCount >= r.config.SlowThreshold {
			log.Printf("[VIDEO][%s] 讀 meta 連續偏慢: %v（已連續 %d 次超過 %v）",
				r.deviceSession.DeviceIP, metaElapsed, r.slowMetaCount, r.config.WarnFrameMetaOver)
		}
	} else {
		r.slowMetaCount = 0
	}

	// 解析 PTS 和幀大小
	pts := binary.BigEndian.Uint64(meta[0:8])
	frameSize := binary.BigEndian.Uint32(meta[8:12])

	// 讀取幀資料
	frame := make([]byte, frameSize)
	t1 := time.Now()
	if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
		return nil, fmt.Errorf("read frame: %w", err)
	}
	readElapsed := time.Since(t1)

	// 檢查是否連續慢
	if readElapsed > r.config.WarnFrameReadOver {
		r.slowFrameCount++
		if r.slowFrameCount >= r.config.SlowThreshold {
			log.Printf("[VIDEO][%s] 讀 frame 連續偏慢: %v（已連續 %d 次，size=%d bytes, %.2f KB）",
				r.deviceSession.DeviceIP, readElapsed, r.slowFrameCount, frameSize, float64(frameSize)/1024)
		}
	} else {
		r.slowFrameCount = 0
	}

	return &FrameInfo{
		PTS:       pts,
		FrameSize: frameSize,
		Data:      frame,
	}, nil
}

// ProcessFrame 處理視訊幀（解析 NALU 並更新 SPS/PPS）
func (r *Reader) ProcessFrame(frame *FrameInfo) (nalus [][]byte, gotNewSPS bool, idrInThisAU bool) {
	nalus = video.SplitAnnexBNALUs(frame.Data)

	spsCnt, ppsCnt, idrCnt, othersCnt := video.CountByType(nalus)

	for _, n := range nalus {
		naluType := video.NALUType(n)
		switch naluType {
		case 7: // SPS
			r.deviceSession.StateMu.Lock()
			if !video.EqualNALU(r.deviceSession.LastSPS, n) {
				if w, h, ok := video.ParseH264SPSDimensions(n); ok {
					r.deviceSession.VideoW, r.deviceSession.VideoH = w, h
					gotNewSPS = true
					log.Printf("[AU][%s] 新的 SPS 並成功解析解析度 %dx%d", r.deviceSession.DeviceIP, w, h)
				}
			}
			r.deviceSession.LastSPS = append([]byte(nil), n...)
			r.deviceSession.StateMu.Unlock()

		case 8: // PPS
			r.deviceSession.StateMu.Lock()
			if !video.EqualNALU(r.deviceSession.LastPPS, n) {
				log.Printf("[AU][%s] 新的 PPS (len=%d)", r.deviceSession.DeviceIP, len(n))
			}
			r.deviceSession.LastPPS = append([]byte(nil), n...)
			r.deviceSession.StateMu.Unlock()

		case 5: // IDR
			idrInThisAU = true
		}
	}

	log.Printf("[NALU][%s] SPS=%d PPS=%d IDR=%d Others=%d",
		r.deviceSession.DeviceIP, spsCnt, ppsCnt, idrCnt, othersCnt)

	return nalus, gotNewSPS, idrInThisAU
}

// InitPTSMapping 初始化時間戳映射
func (r *Reader) InitPTSMapping(pts uint64) uint32 {
	r.deviceSession.StateMu.Lock()
	defer r.deviceSession.StateMu.Unlock()

	if !r.deviceSession.HavePTS0 {
		r.deviceSession.PTS0 = pts
		r.deviceSession.RTPTS0 = 0
		r.deviceSession.HavePTS0 = true
	}

	return r.deviceSession.RTPTS0 + video.RTPTSFromPTS(pts, r.deviceSession.PTS0)
}

// ShouldWaitKeyframe 檢查是否應該等待關鍵幀
func (r *Reader) ShouldWaitKeyframe() bool {
	r.deviceSession.StateMu.RLock()
	defer r.deviceSession.StateMu.RUnlock()
	return r.deviceSession.NeedKeyframe
}

// SetNeedKeyframe 設置需要關鍵幀標誌
func (r *Reader) SetNeedKeyframe(need bool) {
	r.deviceSession.StateMu.Lock()
	defer r.deviceSession.StateMu.Unlock()
	r.deviceSession.NeedKeyframe = need
}

// IncrementFramesSinceKF 增加距離關鍵幀的幀數
func (r *Reader) IncrementFramesSinceKF() int {
	r.deviceSession.StateMu.Lock()
	defer r.deviceSession.StateMu.Unlock()
	r.deviceSession.FramesSinceKF++
	return r.deviceSession.FramesSinceKF
}

// ResetFramesSinceKF 重置關鍵幀計數器
func (r *Reader) ResetFramesSinceKF() {
	r.deviceSession.StateMu.Lock()
	defer r.deviceSession.StateMu.Unlock()
	r.deviceSession.NeedKeyframe = false
	r.deviceSession.FramesSinceKF = 0
}

// GetSPSPPS 獲取當前的 SPS/PPS
func (r *Reader) GetSPSPPS() (sps, pps []byte) {
	r.deviceSession.StateMu.RLock()
	defer r.deviceSession.StateMu.RUnlock()
	return r.deviceSession.LastSPS, r.deviceSession.LastPPS
}
