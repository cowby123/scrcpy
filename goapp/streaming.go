package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/yourname/scrcpy-go/adb"
)

// registerADBFlags 註冊 ADB 相關的命令行參數並返回配置選項獲取函數
// 用途：配置 ADB 連接參數，包括設備序號、伺服器主機、端口等
func registerADBFlags(fs *flag.FlagSet, hardcodedDevice string) func() adb.Options {
	// ADB 伺服器設定（通常使用預設值即可）
	host := fs.String("adb-host", "127.0.0.1", "ADB 伺服器主機位址")
	port := fs.Int("adb-port", 5037, "ADB 伺服器端口")

	// scrcpy 本地端口設定
	scrcpyPort := fs.Int("scrcpy-port", adb.DefaultScrcpyPort, "scrcpy 反向連接使用的本地端口")

	return func() adb.Options {
		return adb.Options{
			Serial:     hardcodedDevice,
			ServerHost: *host,
			ServerPort: *port,
			ScrcpyPort: *scrcpyPort,
		}
	}
}

// runAndroidStreaming 運行 Android 設備的視訊串流處理
// 用途：建立與 Android 設備的連接，處理視訊串流讀取、NALU 解析和 RTP 封包發送
func runAndroidStreaming(deviceOpts adb.Options) {
	session, conn, err := StartScrcpyBoot(deviceOpts)
	if err != nil {
		log.Printf("[ADB][%s] setup 失敗: %v", deviceOpts.Serial, err)
		return
	}

	// 註冊設備到全域設備列表
	deviceIP := deviceOpts.Serial

	// 為該設備創建專用的 frame channel
	deviceFrameChannel := make(chan RtpPayload, rtpPayloadChannelSize)

	deviceSession := &DeviceSession{
		DeviceIP:     deviceIP,
		Session:      session,
		Conn:         conn,
		CreatedAt:    time.Now(),
		FrameChannel: deviceFrameChannel,
	}

	devicesMu.Lock()
	deviceSessions[deviceIP] = deviceSession
	devicesMu.Unlock()

	defer func() {
		session.Close()
		close(deviceFrameChannel) // 關閉該設備的 frame channel
		// 從設備列表移除
		devicesMu.Lock()
		delete(deviceSessions, deviceIP)
		devicesMu.Unlock()
		log.Printf("[ADB][%s] 連接已關閉", deviceIP)
	}()

	log.Printf("[ADB][%s] target serial=%q scrcpy_port=%d", deviceIP, deviceOpts.Serial, session.ScrcpyPort())
	log.Printf("[ADB][%s] scrcpy server 已啟動", deviceIP)

	// === 6. 啟動 RTP 發送器（該設備專用）===
	goSafe(fmt.Sprintf("rtp-sender-%s", deviceIP), func() {
		log.Printf("[RTP][%s] rtp-sender 啟動", deviceIP)
		for payload := range deviceFrameChannel {
			if payload.IsAccessUnit {
				sendNALUAccessUnitAtTS(deviceIP, payload.NALUs, payload.RTPTimestamp)
			} else {
				sendNALUsAtTS(deviceIP, payload.RTPTimestamp, payload.NALUs...)
			}
		}
		log.Printf("[RTP][%s] rtp-sender 結束", deviceIP)
	})

	// === 7. 開始視訊串流讀取 ===
	log.Printf("[VIDEO][%s] 開始接收視訊串流", deviceIP)

	// 讀取設備名稱
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		log.Fatal("[VIDEO] read device name:", err)
	}
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	log.Printf("[VIDEO] 裝置名稱: %s", deviceName)

	// 讀取視訊格式資訊
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Fatal("[VIDEO] read video header:", err)
	}
	codecID := binary.BigEndian.Uint32(vHeader[0:4])
	w0 := binary.BigEndian.Uint32(vHeader[4:8])
	h0 := binary.BigEndian.Uint32(vHeader[8:12])

	// 更新設備會話中的視訊解析度
	devicesMu.Lock()
	if ds, ok := deviceSessions[deviceIP]; ok {
		ds.VideoW = uint16(w0)
		ds.VideoH = uint16(h0)
	}
	devicesMu.Unlock()

	// 更新當前視訊解析度
	stateMu.Lock()
	videoW, videoH = uint16(w0), uint16(h0)
	stateMu.Unlock()
	evVideoW.Set(int64(videoW))
	evVideoH.Set(int64(videoH))

	log.Printf("[VIDEO] 編碼ID: %d, 初始解析度: %dx%d", codecID, w0, h0)

	// === 8. 主要視訊資料讀取與處理迴圈 ===
	meta := make([]byte, 12)
	startTime = time.Now()
	var frameCount int
	var totalBytes int64

	for {
		// 讀取幀資訊
		t0 := time.Now()
		if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
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

		// 初始化時間對齊
		deviceSession.StateMu.Lock()
		if !deviceSession.HavePTS0 {
			deviceSession.PTS0 = pts
			deviceSession.RTPTS0 = 0
			deviceSession.HavePTS0 = true
		}
		curTS := deviceSession.RTPTS0 + rtpTSFromPTS(pts, deviceSession.PTS0)
		deviceSession.StateMu.Unlock()

		// 讀取幀資料
		t1 := time.Now()
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
			log.Println("[VIDEO] read frame:", err)
			break
		}
		readElapsed := time.Since(t1)
		evLastFrameReadMS.Set(readElapsed.Milliseconds())
		if readElapsed > warnFrameReadOver {
			log.Printf("[VIDEO] 讀 frame 偏慢: %v (size=%d)", readElapsed, frameSize)
		}

		// 解析 NALUs
		nalus := splitAnnexBNALUs(frame)
		idrInThisAU := false
		var gotNewSPS bool
		var spsCnt, ppsCnt, idrCnt, othersCnt int

		for _, n := range nalus {
			switch naluType(n) {
			case 7: // SPS
				spsCnt++
				deviceSession.StateMu.Lock()
				if !bytes.Equal(deviceSession.LastSPS, n) {
					if w, h, ok := parseH264SPSDimensions(n); ok {
						deviceSession.VideoW, deviceSession.VideoH = w, h
						gotNewSPS = true
						evVideoW.Set(int64(w))
						evVideoH.Set(int64(h))
						log.Printf("[AU][%s] 新的 SPS 並成功解析解析度 %dx%d", deviceIP, w, h)
					}
				}
				deviceSession.LastSPS = append([]byte(nil), n...)
				deviceSession.StateMu.Unlock()
			case 8: // PPS
				ppsCnt++
				deviceSession.StateMu.Lock()
				if !bytes.Equal(deviceSession.LastPPS, n) {
					log.Printf("[AU][%s] 新的 PPS (len=%d)", deviceIP, len(n))
				}
				deviceSession.LastPPS = append([]byte(nil), n...)
				deviceSession.StateMu.Unlock()
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

		// 檢查並處理關鍵幀等待狀態
		deviceSession.StateMu.RLock()
		waitKF := deviceSession.NeedKeyframe
		deviceSession.StateMu.RUnlock()

		// 處理 SPS 變更情況
		if gotNewSPS {
			deviceSession.StateMu.RLock()
			sps := deviceSession.LastSPS
			pps := deviceSession.LastPPS
			deviceSession.StateMu.RUnlock()

			if len(sps) > 0 {
				pushToRTPChannel(deviceFrameChannel, deviceIP, RtpPayload{NALUs: [][]byte{sps}, RTPTimestamp: curTS, IsAccessUnit: false})
			}
			if len(pps) > 0 {
				pushToRTPChannel(deviceFrameChannel, deviceIP, RtpPayload{NALUs: [][]byte{pps}, RTPTimestamp: curTS, IsAccessUnit: false})
			}

			deviceSession.StateMu.Lock()
			deviceSession.NeedKeyframe = true
			waitKF = true
			deviceSession.StateMu.Unlock()

			if session != nil {
				session.RequestKeyframe()
			}
			evKeyframeRequests.Add(1)
			log.Printf("[KF][%s] 檢測到新 SPS，已請求關鍵幀", deviceIP)
		}

		// 處理關鍵幀等待邏輯
		if waitKF {
			deviceSession.StateMu.RLock()
			sps := deviceSession.LastSPS
			pps := deviceSession.LastPPS
			deviceSession.StateMu.RUnlock()

			if len(sps) > 0 && len(pps) > 0 {
				pushToRTPChannel(deviceFrameChannel, deviceIP, RtpPayload{NALUs: [][]byte{sps}, RTPTimestamp: curTS, IsAccessUnit: false})
				pushToRTPChannel(deviceFrameChannel, deviceIP, RtpPayload{NALUs: [][]byte{pps}, RTPTimestamp: curTS, IsAccessUnit: false})
			} else {
				if session != nil {
					session.RequestKeyframe()
				}
				evKeyframeRequests.Add(1)
			}

			deviceSession.StateMu.Lock()
			deviceSession.FramesSinceKF++
			evFramesSinceKF.Set(int64(deviceSession.FramesSinceKF))

			if deviceSession.FramesSinceKF%30 == 0 {
				log.Printf("[KF][%s] 等待 IDR 中... 已經 %d 幀；再次請求 IDR", deviceIP, deviceSession.FramesSinceKF)
				if session != nil {
					session.RequestKeyframe()
				}
				evKeyframeRequests.Add(1)
			}
			deviceSession.StateMu.Unlock()

			if !idrInThisAU {
				goto stats
			}

			log.Printf("[KF][%s] 收到 IDR，恢復正常串流", deviceIP)
			deviceSession.StateMu.Lock()
			deviceSession.NeedKeyframe = false
			deviceSession.FramesSinceKF = 0
			deviceSession.StateMu.Unlock()
			evFramesSinceKF.Set(0)

			pushToRTPChannel(deviceFrameChannel, deviceIP, RtpPayload{NALUs: nalus, RTPTimestamp: curTS, IsAccessUnit: true})
		} else {
			pushToRTPChannel(deviceFrameChannel, deviceIP, RtpPayload{NALUs: nalus, RTPTimestamp: curTS, IsAccessUnit: true})
		}

		// 更新統計
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

		// 更新串流序號
		stateMu.Lock()
		auSeq++
		stateMu.Unlock()
		evAuSeq.Set(int64(auSeq))
	}
}

// trimString 截斷字串到指定長度並添加省略號
func trimString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
