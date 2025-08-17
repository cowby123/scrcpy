package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

var (
	upgrader    = websocket.Upgrader{}
	tracksMu    sync.Mutex
	videoTracks []*webrtc.TrackLocalStaticSample
	whiteFrame  []byte
)

func init() {
	whiteFrame = []byte{
		0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0xC0, 0x0C, 0x8D, 0x8D, 0x40, 0x50, 0x1E, 0xD0, 0x0B, 0x80, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xF1, 0x83, 0x19, 0x60,
		0x00, 0x00, 0x00, 0x01, 0x68, 0xCE, 0x06, 0xE2,
		0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x84, 0x00,
	}
	for i := 0; i < 256; i++ {
		whiteFrame = append(whiteFrame, 0xFF)
	}
	for i := 0; i < 64; i++ {
		whiteFrame = append(whiteFrame, 0x80)
	}
	for i := 0; i < 64; i++ {
		whiteFrame = append(whiteFrame, 0x80)
	}
}

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
                       SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42c00c",
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
	http.HandleFunc("/ws", signalHandler)
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)
	go func() {
		log.Println("HTTP/WebSocket 伺服器啟動於 http://localhost:8080/webrtc_frontend.html")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatal("http server:", err)
		}
	}()

	ticker := time.NewTicker(time.Second / 30)
	for range ticker.C {
		tracksMu.Lock()
		for _, t := range videoTracks {
			if err := t.WriteSample(media.Sample{Data: whiteFrame, Duration: time.Second / 30}); err != nil {
				log.Println("write sample:", err)
			}
		}
		tracksMu.Unlock()
	}
}
