// main_refactored.go - scrcpy WebRTC 伺服器（完全重構版）
// 使用模組化架構：internal/{device,video,stream,webrtc,input,utils}
// 支援多設備、多客戶端的 Android 螢幕串流

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
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"time"

	"github.com/cowby123/scrcpy-go/adb"
	"github.com/cowby123/scrcpy-go/internal/device"
	"github.com/cowby123/scrcpy-go/internal/input"
	"github.com/cowby123/scrcpy-go/internal/stream"
	"github.com/cowby123/scrcpy-go/internal/utils"
	"github.com/cowby123/scrcpy-go/internal/video"
	webrtcHandler "github.com/cowby123/scrcpy-go/internal/webrtc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// === 全域管理器 ===
var (
	deviceManager *device.Manager
	rtpSender     *video.RTPSender
	streamProc    *stream.Processor
	webrtcHdlr    *webrtcHandler.Handler
)

// === expvar 觀測指標 ===
var (
	evFramesRead         = expvar.NewInt("frames_read")
	evBytesRead          = expvar.NewInt("bytes_read")
	evPLICount           = expvar.NewInt("pli_count")
	evKeyframeRequests   = expvar.NewInt("keyframe_requests")
	evCtrlWritesOK       = expvar.NewInt("control_writes_ok")
	evCtrlWritesErr      = expvar.NewInt("control_writes_err")
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
	evLastCtrlReadMsAgo  = expvar.NewInt("last_control_read_ms_ago")
	evHeartbeatSent      = expvar.NewInt("heartbeat_sent")
	evCtrlReadsErr       = expvar.NewInt("control_reads_err")
	evLastFrameReadMS    = expvar.NewInt("last_frame_read_ms")
	evFramesSinceKF      = expvar.NewInt("frames_since_kf")
	evFramesDropped      = expvar.NewInt("frames_dropped")
	evCtrlReadsOK        = expvar.NewInt("control_reads_ok")
	evCtrlReadClipboardB = expvar.NewInt("control_read_clipboard_bytes")
)

// === 常數定義 ===
const (
	controlHealthTick          = 2 * time.Second
	controlStaleAfter          = 5 * time.Second
	controlReadBufMax          = 1 << 20 // 1MB
	deviceMsgTypeClipboard     = 0
	controlMsgResetVideo       = 17
	controlMsgGetClipboard     = 8
	controlWriteDefaultTimeout = 50 * time.Millisecond
)

// === 命令行參數 ===
var (
	listenAddr      = flag.String("listen", ":8080", "HTTP 伺服器監聽地址")
	hardcodedDevice = flag.String("device", "", "固定連接的設備 IP（留空則自動掃描）")
)

// runAndroidStreaming 啟動單個 Android 設備的串流
func runAndroidStreaming(deviceOpts adb.Options) {
	deviceIP := deviceOpts.Serial

	// 啟動 scrcpy
	session, conn, err := StartScrcpyBoot(deviceOpts)
	if err != nil {
		log.Printf("[ADB][%s] setup 失敗: %v", deviceIP, err)
		return
	}

	// 創建設備會話
	deviceFrameChannel := make(chan device.RtpPayload, 3)
	deviceSession := &device.DeviceSession{
		DeviceIP:     deviceIP,
		Session:      session,
		Conn:         conn,
		CreatedAt:    time.Now(),
		FrameChannel: deviceFrameChannel,
	}

	// 註冊到管理器
	deviceManager.AddDevice(deviceIP, deviceSession)
	defer func() {
		session.Close()
		close(deviceFrameChannel)
		deviceManager.RemoveDevice(deviceIP)
		log.Printf("[ADB][%s] 連接已關閉", deviceIP)
	}()

	log.Printf("[ADB][%s] scrcpy server 已啟動", deviceIP)

	// 啟動 RTP 發送器
	utils.GoSafe(fmt.Sprintf("rtp-sender-%s", deviceIP), func() {
		streamProc.StartRTPSender(deviceSession)
	})

	// 啟動控制通道監控
	utils.GoSafe(fmt.Sprintf("control-monitor-%s", deviceIP), func() {
		monitorControlChannel(deviceSession)
	})

	// 讀取設備名稱
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		log.Printf("[VIDEO][%s] read device name: %v", deviceIP, err)
		return
	}
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	log.Printf("[VIDEO][%s] 裝置名稱: %s", deviceIP, deviceName)

	// 讀取視訊格式資訊
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Printf("[VIDEO][%s] read video header: %v", deviceIP, err)
		return
	}

	codecID := binary.BigEndian.Uint32(vHeader[0:4])
	w0 := binary.BigEndian.Uint32(vHeader[4:8])
	h0 := binary.BigEndian.Uint32(vHeader[8:12])

	deviceSession.VideoW = uint16(w0)
	deviceSession.VideoH = uint16(h0)

	evVideoW.Set(int64(w0))
	evVideoH.Set(int64(h0))

	log.Printf("[VIDEO][%s] 編碼ID: %d, 解析度: %dx%d", deviceIP, codecID, w0, h0)

	// 處理視訊流（注意：頭部已在上方讀取，streamProc 直接處理幀）
	if err := streamProc.ProcessVideoStreamFrames(deviceSession); err != nil {
		log.Printf("[VIDEO][%s] 串流結束: %v", deviceIP, err)
	}
}

// monitorControlChannel 監控控制通道的訊息
func monitorControlChannel(ds *device.DeviceSession) {
	buf := make([]byte, 1<<20)
	for {
		if controlConn, ok := ds.Conn.Control.(net.Conn); ok {
			controlConn.SetReadDeadline(time.Now().Add(15 * time.Second))
		}

		n, err := ds.Conn.Control.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("[CTRL][%s] read error: %v", ds.DeviceIP, err)
			}
			return
		}

		if n == 0 {
			continue
		}

		msgType := buf[0]
		log.Printf("[CTRL][%s] 收到訊息: type=%d, len=%d", ds.DeviceIP, msgType, n)
	}
}

// handleTouchEvent 處理前端觸控事件
func handleTouchEvent(w http.ResponseWriter, r *http.Request) {
	var ev input.TouchEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// 獲取設備會話
	deviceSession, ok := deviceManager.GetDevice(ev.DeviceIP)
	if !ok {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	// 創建控制寫入函數
	controlWriter := func(data []byte) error {
		if controlConn, ok := deviceSession.Conn.Control.(net.Conn); ok {
			controlConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		}
		t0 := time.Now()
		_, err := deviceSession.Conn.Control.Write(data)
		elapsed := time.Since(t0)
		evLastCtrlWriteMS.Set(elapsed.Milliseconds())

		if err != nil {
			evCtrlWritesErr.Add(1)
			return err
		}
		evCtrlWritesOK.Add(1)
		return nil
	}

	// 處理觸控事件
	input.HandleTouchEvent(ev, controlWriter)

	w.WriteHeader(http.StatusOK)
}

// handleDevicesList 返回設備列表
func handleDevicesList(w http.ResponseWriter, r *http.Request) {
	adbDevices, err := adb.ListDevices(adb.Options{
		ServerHost: "127.0.0.1",
		ServerPort: 5037,
	})

	if err != nil {
		log.Printf("[ADB] 列出設備失敗: %v", err)
		http.Error(w, fmt.Sprintf("列出設備失敗: %v", err), http.StatusInternalServerError)
		return
	}

	devices := make([]map[string]interface{}, 0, len(adbDevices))
	for _, dev := range adbDevices {
		deviceInfo := map[string]interface{}{
			"ip":    dev.Serial,
			"state": dev.State,
		}

		if ds, ok := deviceManager.GetDevice(dev.Serial); ok {
			deviceInfo["connected"] = true
			deviceInfo["videoW"] = ds.VideoW
			deviceInfo["videoH"] = ds.VideoH
			deviceInfo["createdAt"] = ds.CreatedAt.Format(time.RFC3339)
		} else {
			deviceInfo["connected"] = false
		}

		devices = append(devices, deviceInfo)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"devices": devices,
	})
}

// handleWebRTCOffer 處理 WebRTC SDP offer
func handleWebRTCOffer(w http.ResponseWriter, r *http.Request) {
	// 支持兩種參數名稱：id（前端使用）和 deviceIP（向後兼容）
	deviceIP := r.URL.Query().Get("id")
	if deviceIP == "" {
		deviceIP = r.URL.Query().Get("deviceIP")
	}
	if deviceIP == "" {
		http.Error(w, "missing id or deviceIP parameter", http.StatusBadRequest)
		return
	}

	log.Printf("[WebRTC] 收到 Offer 請求，設備 IP: %s", deviceIP)

	deviceSession, ok := deviceManager.GetDevice(deviceIP)
	if !ok {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	sessionID := utils.GenerateSessionID()

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid offer", http.StatusBadRequest)
		return
	}

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		http.Error(w, "create pc error", http.StatusInternalServerError)
		return
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"scrcpy",
	)
	if err != nil {
		http.Error(w, "create track error", http.StatusInternalServerError)
		return
	}

	rtpSenderWRTC, err := pc.AddTrack(videoTrack)
	if err != nil {
		http.Error(w, "add track error", http.StatusInternalServerError)
		return
	}

	clientSession := &device.ClientSession{
		ID:         sessionID,
		DeviceIP:   deviceIP,
		PC:         pc,
		VideoTrack: videoTrack,
		Packetizer: rtp.NewPacketizer(
			1200,
			0,
			0,
			&codecs.H264Payloader{},
			rtp.NewRandomSequencer(),
			90000,
		),
		CreatedAt: time.Now(),
	}

	deviceManager.AddClient(sessionID, clientSession)

	// 處理 DataChannel 以接收觸控事件
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("[WebRTC][%s] DataChannel 已建立: %s", sessionID, dc.Label())
		
		dc.OnOpen(func() {
			log.Printf("[WebRTC][%s] DataChannel 已開啟: %s", sessionID, dc.Label())
		})
		
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			// 解析觸控事件
			var ev input.TouchEvent
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				log.Printf("[WebRTC][%s] 解析觸控事件失敗: %v", sessionID, err)
				return
			}
			
			// 設置設備 IP
			if ev.DeviceIP == "" {
				ev.DeviceIP = deviceIP
			}
			
			// 獲取設備會話
			ds, ok := deviceManager.GetDevice(ev.DeviceIP)
			if !ok {
				log.Printf("[WebRTC][%s] 找不到設備: %s", sessionID, ev.DeviceIP)
				return
			}
			
			// 創建控制寫入函數
			controlWriter := func(data []byte) error {
				if controlConn, ok := ds.Conn.Control.(net.Conn); ok {
					controlConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				}
				t0 := time.Now()
				_, err := ds.Conn.Control.Write(data)
				elapsed := time.Since(t0)
				evLastCtrlWriteMS.Set(elapsed.Milliseconds())

				if err != nil {
					evCtrlWritesErr.Add(1)
					return err
				}
				evCtrlWritesOK.Add(1)
				return nil
			}
			
			// 處理觸控事件
			input.HandleTouchEvent(ev, controlWriter)
		})
		
		dc.OnClose(func() {
			log.Printf("[WebRTC][%s] DataChannel 已關閉: %s", sessionID, dc.Label())
		})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[WebRTC][%s] 狀態: %s", sessionID, state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			deviceManager.RemoveClient(sessionID)
			pc.Close()
		}
	})

	utils.GoSafe(fmt.Sprintf("rtcp-%s", sessionID), func() {
		handleRTCP(rtpSenderWRTC, sessionID, deviceSession)
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

	<-webrtc.GatheringCompletePromise(pc)

	deviceSession.StateMu.Lock()
	deviceSession.NeedKeyframe = true
	deviceSession.StateMu.Unlock()

	if session, ok := deviceSession.Session.(*ScrcpySession); ok {
		session.RequestKeyframe()
	}
	evKeyframeRequests.Add(1)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pc.LocalDescription())
}

// handleRTCP 處理 RTCP 封包
func handleRTCP(rtpSender *webrtc.RTPSender, sessionID string, ds *device.DeviceSession) {
	rtcpBuf := make([]byte, 1500)
	for {
		n, _, err := rtpSender.Read(rtcpBuf)
		if err != nil {
			return
		}

		pkts, err := rtcp.Unmarshal(rtcpBuf[:n])
		if err != nil {
			continue
		}

		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication:
				log.Printf("[RTCP][%s] PLI → 請求關鍵幀", sessionID)
				requestKeyframe(ds)
				evRTCP_PLI.Add(1)
				evPLICount.Add(1)

			case *rtcp.FullIntraRequest:
				log.Printf("[RTCP][%s] FIR → 請求關鍵幀", sessionID)
				requestKeyframe(ds)
				evRTCP_FIR.Add(1)
			}
		}
	}
}

// requestKeyframe 請求關鍵幀
func requestKeyframe(ds *device.DeviceSession) {
	if ds == nil {
		return
	}

	ds.StateMu.Lock()
	ds.NeedKeyframe = true
	ds.StateMu.Unlock()

	if session, ok := ds.Session.(*ScrcpySession); ok {
		session.RequestKeyframe()
	}
	evKeyframeRequests.Add(1)
}

// main 主函數
func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	// 初始化管理器
	deviceManager = device.NewManager()
	rtpSender = video.NewRTPSender(deviceManager)
	streamProc = stream.NewProcessor(deviceManager, rtpSender, stream.Config{
		WarnFrameMetaOver: 100 * time.Millisecond,
		WarnFrameReadOver: 200 * time.Millisecond,
		SlowThreshold:     3,
	})

	webrtcConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	webrtcHdlr = webrtcHandler.NewHandler(deviceManager, webrtcConfig)

	// 設置路由
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
			return
		}
		http.FileServer(http.Dir(".")).ServeHTTP(w, r)
	})
	http.HandleFunc("/offer", handleWebRTCOffer)
	http.HandleFunc("/devices", handleDevicesList)
	http.HandleFunc("/touch", handleTouchEvent)

	// 啟動 HTTP 伺服器
	utils.GoSafe("http-server", func() {
		log.Printf("[HTTP] 伺服器啟動: %s", *listenAddr)
		if err := http.ListenAndServe(*listenAddr, nil); err != nil {
			log.Fatal("[HTTP] 伺服器錯誤:", err)
		}
	})

	// 啟動設備連接
	if *hardcodedDevice != "" {
		log.Printf("[ADB] 使用固定設備: %s", *hardcodedDevice)
		runAndroidStreaming(adb.Options{
			Serial:     *hardcodedDevice,
			ServerHost: "127.0.0.1",
			ServerPort: 5037,
			ScrcpyPort: adb.DefaultScrcpyPort,
		})
	} else {
		log.Println("[ADB] 掃描所有 ADB 設備...")
		devices, err := adb.ListDevices(adb.Options{
			ServerHost: "127.0.0.1",
			ServerPort: 5037,
		})

		if err != nil {
			log.Fatal("[ADB] 列出設備失敗:", err)
		}

		if len(devices) == 0 {
			log.Fatal("[ADB] 沒有找到任何設備")
		}

		log.Printf("[ADB] 找到 %d 個設備", len(devices))
		for _, dev := range devices {
			log.Printf("[ADB] 設備: %s (狀態: %s)", dev.Serial, dev.State)
			if dev.State == "device" {
				utils.GoSafe(fmt.Sprintf("device-%s", dev.Serial), func() {
					runAndroidStreaming(adb.Options{
						Serial:     dev.Serial,
						ServerHost: "127.0.0.1",
						ServerPort: 5037,
						ScrcpyPort: adb.DefaultScrcpyPort,
					})
				})
			}
		}
	}

	// 阻止主程式退出
	log.Println("[MAIN] 伺服器運行中...")
	log.Printf("[MAIN] 訪問 http://localhost%s", *listenAddr)
	log.Printf("[MAIN] 監控指標: http://localhost%s/debug/vars", *listenAddr)
	log.Printf("[MAIN] CPU profiling: http://localhost%s/debug/pprof/", *listenAddr)

	runtime.GC()
	select {}
}
