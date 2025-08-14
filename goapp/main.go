// 這是一個以 Go 撰寫的簡易 scrcpy 客戶端範例
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/yourname/scrcpy-go/adb"
)

var upgrader = websocket.Upgrader{}

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
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
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

// scrcpy 伺服器檔案的路徑
const serverJar = "./assets/scrcpy-server"

func main() {

	dev, err := adb.NewDevice("")
	if err != nil {
		log.Fatal(err)
	}

	// 將伺服器檔案推送至裝置暫存目錄
	if err := dev.PushServer(serverJar); err != nil {
		log.Fatal("push server:", err)
	}

	// 啟動 scrcpy 伺服器（tunnel_forward=false）
	stream, err := dev.StartServer()
	if err != nil {
		log.Fatal("start server:", err)
	}
	defer stream.Close()

	// WebRTC 部分
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Fatal("webrtc:", err)
	}
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "scrcpy",
	)
	if err != nil {
		log.Fatal("track:", err)
	}
	_, err = peerConnection.AddTrack(videoTrack)
	if err != nil {
		log.Fatal("add track:", err)
	}

	// 啟動 WebSocket 信令伺服器
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		signalHandler(w, r, peerConnection)
	})
	// 啟動靜態檔案伺服器，讓使用者可直接瀏覽 webrtc_frontend.html
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)
	go func() {
		log.Println("HTTP/WebSocket 伺服器啟動於 http://localhost:8080/webrtc_frontend.html")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatal("http server:", err)
		}
	}()

	// 讀取 scrcpy stream 並送到 WebRTC
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stream.Read(buf)
			if err != nil {
				log.Println("stream read:", err)
				break
			}
			videoTrack.WriteSample(media.Sample{Data: buf[:n], Duration: time.Second / 30})
		}
	}()

	select {} // 保持程式執行
}
