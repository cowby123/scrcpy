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
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/yourname/scrcpy-go/adb"
)

var (
	upgrader    = websocket.Upgrader{}
	tracksMu    sync.Mutex
	videoTracks []*webrtc.TrackLocalStaticSample
)

// signalHandler 提供簡易的 WebRTC 信令處理，透過 WebSocket 與瀏覽器交換 SDP/ICE。
// 每次瀏覽器連線都建立新的 PeerConnection 與 Track，避免重整後狀態不一致。
func signalHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer ws.Close()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		log.Println("webrtc:", err)
		return
	}
	defer pc.Close()

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
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
		log.Println("track:", err)
		return
	}
	sender, err := pc.AddTrack(videoTrack)
	if err != nil {
		log.Println("add track:", err)
		return
	}
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	tracksMu.Lock()
	videoTracks = append(videoTracks, videoTrack)
	tracksMu.Unlock()
	defer func() {
		tracksMu.Lock()
		for i, t := range videoTracks {
			if t == videoTrack {
				videoTracks = append(videoTracks[:i], videoTracks[i+1:]...)
				break
			}
		}
		tracksMu.Unlock()
	}()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand, _ := json.Marshal(c.ToJSON())
		ws.WriteJSON(map[string]interface{}{"candidate": json.RawMessage(cand)})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Println("offer:", err)
		return
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		log.Println("set local desc:", err)
		return
	}
	ws.WriteJSON(map[string]interface{}{"sdp": offer})

	for {
		var msg map[string]json.RawMessage
		if err := ws.ReadJSON(&msg); err != nil {
			log.Println("ws read:", err)
			break
		}
		if sdp, ok := msg["sdp"]; ok {
			var desc webrtc.SessionDescription
			json.Unmarshal(sdp, &desc)
			pc.SetRemoteDescription(desc)
		}
		if cand, ok := msg["candidate"]; ok {
			var ice webrtc.ICECandidateInit
			json.Unmarshal(cand, &ice)
			pc.AddICECandidate(ice)
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

	// 啟動 WebSocket 信令伺服器與靜態檔案伺服器
	http.HandleFunc("/ws", signalHandler)
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)
	go func() {
		log.Println("HTTP/WebSocket 伺服器啟動於 http://localhost:8080/webrtc_frontend.html")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatal("http server:", err)
		}
	}()

	// 推送假畫面到所有 WebRTC 連線，暫以固定位元串做測試
	fakeFrame := []byte{0x00, 0x00, 0x00, 0x01, 0x09, 0xf0}
	go func() {
		ticker := time.NewTicker(time.Second / 30)
		defer ticker.Stop()
		for range ticker.C {
			tracksMu.Lock()
			for _, t := range videoTracks {
				t.WriteSample(media.Sample{Data: fakeFrame, Duration: time.Second / 30})
			}
			tracksMu.Unlock()
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
