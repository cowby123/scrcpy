// 這是一個以 Go 撰寫的簡易 scrcpy 客戶端範例，用於保存視訊串流
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/yourname/scrcpy-go/adb"
)

var upgrader = websocket.Upgrader{}

// signalHandler 提供簡易的 WebRTC 信令處理，透過 WebSocket 與瀏覽器交換 SDP/ICE
func signalHandler(w http.ResponseWriter, r *http.Request, peerConnection *webrtc.PeerConnection) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer conn.Close()

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand, _ := json.Marshal(c.ToJSON())
		conn.WriteJSON(map[string]interface{}{"candidate": json.RawMessage(cand)})
	})

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		log.Println("offer:", err)
		return
	}
	if err = peerConnection.SetLocalDescription(offer); err != nil {
		log.Println("set local desc:", err)
		return
	}
	conn.WriteJSON(map[string]interface{}{"sdp": offer})

	for {
		var msg map[string]json.RawMessage
		if err := conn.ReadJSON(&msg); err != nil {
			log.Println("ws read:", err)
			break
		}
		if sdp, ok := msg["sdp"]; ok {
			var desc webrtc.SessionDescription
			json.Unmarshal(sdp, &desc)
			peerConnection.SetRemoteDescription(desc)
		}
		if cand, ok := msg["candidate"]; ok {
			var ice webrtc.ICECandidateInit
			json.Unmarshal(cand, &ice)
			peerConnection.AddICECandidate(ice)
		}
	}
}

func main() {
	// 連線到第一台可用的裝置
	dev, err := adb.NewDevice("")
	if err != nil {
		log.Fatal(err)
	}

	// 透過 adb reverse 將裝置連線轉回本機，便於伺服器傳送資料
	if err := dev.Reverse("localabstract:scrcpy", "tcp:27183"); err != nil {
		log.Fatal("reverse:", err)
	}

	// 將伺服器檔案推送至裝置暫存目錄
	if err := dev.PushServer("./assets/scrcpy-server"); err != nil {
		log.Fatal("push server:", err)
	}

	// 啟動 scrcpy 伺服器並取得視訊串流
	conn, err := dev.StartServer()
	if err != nil {
		log.Fatal("start server:", err)
	}
	defer conn.VideoStream.Close()
	defer conn.Control.Close()

	// 建立輸出檔案
	outFile, err := os.Create("output.h264")
	if err != nil {
		log.Fatal("create output file:", err)
	}
	defer outFile.Close()

	log.Println("開始接收視訊串流，儲存至 output.h264")

	// 讀取並略過裝置名稱封包 (64 bytes)
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		log.Fatal("read device name:", err)
	}
	deviceName := string(bytes.TrimRight(nameBuf, "\x00"))
	log.Printf("裝置名稱: %s\n", deviceName)

	// 讀取視訊編碼格式與解析度資訊 (12 bytes)
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Fatal("read video header:", err)
	}
	codec := string(vHeader[:4])
	width := binary.BigEndian.Uint32(vHeader[4:8])
	height := binary.BigEndian.Uint32(vHeader[8:12])
	log.Printf("編碼格式: %s, 解析度: %dx%d\n", codec, width, height)

	// 設定 WebRTC，並透過 HTTP+WebSocket 提供信令
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		log.Fatal("webrtc:", err)
	}
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("Peer Connection State has changed: %s\n", s.String())
	})
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			Channels:    0,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		"video", "scrcpy",
	)
	if err != nil {
		log.Fatal("track:", err)
	}
	sender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		log.Fatal("add track:", err)
	}
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	// 啟動 WebSocket 信令伺服器與靜態檔案伺服器
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		signalHandler(w, r, peerConnection)
	})
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)
	go func() {
		log.Println("HTTP/WebSocket 伺服器啟動於 http://localhost:8080/webrtc_frontend.html")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatal("http server:", err)
		}
	}()

	// 推送假畫面到 WebRTC，暫以固定位元串做測試
	fakeFrame := []byte{0x00, 0x00, 0x00, 0x01, 0x09, 0xf0}
	go func() {
		ticker := time.NewTicker(time.Second / 30)
		defer ticker.Stop()
		for range ticker.C {
			videoTrack.WriteSample(media.Sample{Data: fakeFrame, Duration: time.Second / 30})
		}
	}()

	// 讀取視訊串流並保存（另起 goroutine）
	go func() {
		frameCount := 0
		totalBytes := int64(0)
		startTime := time.Now()
		meta := make([]byte, 12) // scrcpy: [pts(8)] + [size(4)]

		for {
			if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
				log.Println("read frame meta:", err)
				break
			}
			frameSize := binary.BigEndian.Uint32(meta[8:12])

			frame := make([]byte, frameSize)
			if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
				log.Println("read frame:", err)
				break
			}

			if _, err := outFile.Write(frame); err != nil {
				log.Println("write error:", err)
				break
			}

			frameCount++
			totalBytes += int64(frameSize)
			if frameCount%100 == 0 {
				elapsed := time.Since(startTime).Seconds()
				bytesPerSecond := float64(totalBytes) / elapsed
				log.Printf("接收影格: %d, 速率: %.2f MB/s\n",
					frameCount,
					bytesPerSecond/(1024*1024))
			}
		}
	}()

	select {} // 保持程式執行
}
