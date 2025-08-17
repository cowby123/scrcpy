package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// 可調整的參數
// 可調整的參數
var (
	listenAddr = ":8888" // HTTP 監聽埠
)

type Frame struct {
	Data     []byte
	Duration time.Duration
}

var frameCh = make(chan Frame, 128)

func RunRTC() {
	http.Handle("/", http.FileServer(http.Dir("./web")))
	http.HandleFunc("/offer", handleOffer)

	log.Printf("listening on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}

func handleOffer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// 1) 解析 SDP Offer
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 2) 建立 PeerConnection
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	closed := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Println("pc state:", s)
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateDisconnected {
			select {
			case <-closed:
			default:
				close(closed)
			}
			_ = pc.Close()
		}
	})

	// 3) 新增 H.264 Track（Baseline, packetization-mode=1）
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		"video", "pion",
	)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if _, err = pc.AddTrack(videoTrack); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 4) 設定 Remote/Local SDP 並回傳 Answer
	if err = pc.SetRemoteDescription(offer); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	<-gatherComplete

	// 5) 從 frameCh 讀取 H.264 影格並寫入 Track
	go func() {
		for {
			select {
			case <-closed:
				return
			case f := <-frameCh:
				if err := videoTrack.WriteSample(media.Sample{Data: f.Data, Duration: f.Duration}); err != nil {
					log.Println("write sample:", err)
				}
			}
		}
	}()

	// 6) 回傳 SDP Answer
	_ = json.NewEncoder(w).Encode(pc.LocalDescription())
}
