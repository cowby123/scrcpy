package stream

import (
	"fmt"
	"log"
	"time"

	"github.com/cowby123/scrcpy-go/internal/device"
	"github.com/cowby123/scrcpy-go/internal/video"
)

// Processor 視訊流處理器
type Processor struct {
	deviceManager *device.Manager
	rtpSender     *video.RTPSender
	config        Config
}

// NewProcessor 創建視訊流處理器
func NewProcessor(manager *device.Manager, sender *video.RTPSender, cfg Config) *Processor {
	return &Processor{
		deviceManager: manager,
		rtpSender:     sender,
		config:        cfg,
	}
}

// ProcessVideoStream 處理完整的視訊流（從連接到讀取幀）
func (p *Processor) ProcessVideoStream(deviceSession *device.DeviceSession) error {
	deviceIP := deviceSession.DeviceIP
	conn := deviceSession.Conn

	reader := NewReader(deviceSession, p.config)

	// 讀取視訊頭部
	deviceName, codecID, w0, h0, err := reader.ReadVideoHeader(conn)
	if err != nil {
		return fmt.Errorf("read video header: %w", err)
	}

	log.Printf("[VIDEO][%s] 設備名稱: %s", deviceIP, deviceName)
	log.Printf("[VIDEO][%s] 編碼ID: %d, 初始解析度: %dx%d", deviceIP, codecID, w0, h0)

	// 更新設備會話中的視訊解析度
	deviceSession.VideoW = uint16(w0)
	deviceSession.VideoH = uint16(h0)

	return p.processFrameLoop(deviceSession, reader)
}

// ProcessVideoStreamFrames 處理視訊幀（假設頭部已在外部讀取）
func (p *Processor) ProcessVideoStreamFrames(deviceSession *device.DeviceSession) error {
	reader := NewReader(deviceSession, p.config)
	return p.processFrameLoop(deviceSession, reader)
}

// processFrameLoop 處理視訊幀循環
func (p *Processor) processFrameLoop(deviceSession *device.DeviceSession, reader *Reader) error {
	deviceIP := deviceSession.DeviceIP
	conn := deviceSession.Conn

	// 幀計數和統計
	frameCount := 0
	var totalBytes int64
	startTime := time.Now()

	// 主視訊處理循環
	for {
		// 讀取幀
		frameInfo, err := reader.ReadFrame(conn)
		if err != nil {
			log.Printf("[VIDEO][%s] read frame: %v", deviceIP, err)
			break
		}

		// 初始化時間戳映射
		curTS := reader.InitPTSMapping(frameInfo.PTS)

		// 處理幀（解析 NALU）
		nalus, gotNewSPS, idrInThisAU := reader.ProcessFrame(frameInfo)

		// 檢查是否需要等待關鍵幀
		waitKF := reader.ShouldWaitKeyframe()

		// 處理 SPS 變更
		if gotNewSPS {
			sps, pps := reader.GetSPSPPS()

			if len(sps) > 0 {
				video.PushToChannel(deviceSession.FrameChannel, deviceIP, device.RtpPayload{
					NALUs:        [][]byte{sps},
					RTPTimestamp: curTS,
					IsAccessUnit: false,
				})
			}
			if len(pps) > 0 {
				video.PushToChannel(deviceSession.FrameChannel, deviceIP, device.RtpPayload{
					NALUs:        [][]byte{pps},
					RTPTimestamp: curTS,
					IsAccessUnit: false,
				})
			}

			reader.SetNeedKeyframe(true)
			waitKF = true

			// 請求關鍵幀
			if session, ok := deviceSession.Session.(interface{ RequestKeyframe() }); ok {
				session.RequestKeyframe()
			}
			log.Printf("[KF][%s] 檢測到新 SPS，已請求關鍵幀", deviceIP)
		}

		// 處理關鍵幀等待邏輯
		if waitKF {
			sps, pps := reader.GetSPSPPS()

			if len(sps) > 0 && len(pps) > 0 {
				video.PushToChannel(deviceSession.FrameChannel, deviceIP, device.RtpPayload{
					NALUs:        [][]byte{sps},
					RTPTimestamp: curTS,
					IsAccessUnit: false,
				})
				video.PushToChannel(deviceSession.FrameChannel, deviceIP, device.RtpPayload{
					NALUs:        [][]byte{pps},
					RTPTimestamp: curTS,
					IsAccessUnit: false,
				})
			} else {
				if session, ok := deviceSession.Session.(interface{ RequestKeyframe() }); ok {
					session.RequestKeyframe()
				}
			}

			framesSinceKF := reader.IncrementFramesSinceKF()

			if framesSinceKF%30 == 0 {
				log.Printf("[KF][%s] 等待 IDR 中... 已經 %d 幀；再次請求 IDR", deviceIP, framesSinceKF)
				if session, ok := deviceSession.Session.(interface{ RequestKeyframe() }); ok {
					session.RequestKeyframe()
				}
			}

			if !idrInThisAU {
				goto stats
			}

			log.Printf("[KF][%s] 收到 IDR，恢復正常串流", deviceIP)
			reader.ResetFramesSinceKF()

			video.PushToChannel(deviceSession.FrameChannel, deviceIP, device.RtpPayload{
				NALUs:        nalus,
				RTPTimestamp: curTS,
				IsAccessUnit: true,
			})
		} else {
			video.PushToChannel(deviceSession.FrameChannel, deviceIP, device.RtpPayload{
				NALUs:        nalus,
				RTPTimestamp: curTS,
				IsAccessUnit: true,
			})
		}

	stats:
		frameCount++
		totalBytes += int64(frameInfo.FrameSize)

		if frameCount%100 == 0 {
			elapsed := time.Since(startTime)
			fps := float64(frameCount) / elapsed.Seconds()
			mbps := float64(totalBytes) * 8 / elapsed.Seconds() / 1_000_000
			log.Printf("[STATS][%s] 幀數: %d, FPS: %.2f, 總計: %.2f MB, 速率: %.2f Mbps",
				deviceIP, frameCount, fps, float64(totalBytes)/1_000_000, mbps)
		}
	}

	return nil
}

// StartRTPSender 啟動 RTP 發送器 goroutine
func (p *Processor) StartRTPSender(deviceSession *device.DeviceSession) {
	deviceIP := deviceSession.DeviceIP
	log.Printf("[RTP][%s] rtp-sender 啟動", deviceIP)

	for payload := range deviceSession.FrameChannel {
		if payload.IsAccessUnit {
			p.rtpSender.SendNALUAccessUnit(deviceIP, payload.NALUs, payload.RTPTimestamp)
		} else {
			p.rtpSender.SendNALUs(deviceIP, payload.RTPTimestamp, payload.NALUs...)
		}
	}

	log.Printf("[RTP][%s] rtp-sender 結束", deviceIP)
}
