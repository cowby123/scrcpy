// 這是一個以 Go 撰寫的簡易 scrcpy 客戶端範例
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/yourname/scrcpy-go/adb"
	"github.com/yourname/scrcpy-go/protocol"
)

// scrcpy 伺服器檔案的路徑
const serverJar = "./assets/scrcpy-server"

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
	if err := dev.PushServer(serverJar); err != nil {
		log.Fatal("push server:", err)
	}

	// 啟動 scrcpy 伺服器並取得資料串流
	stream, err := dev.StartServer()
	if err != nil {
		log.Fatal("start server:", err)
	}
	defer stream.Close()

	// 建立 HTTP 伺服器，透過 WebRTC 將影片串流給瀏覽器
	http.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		var offer webrtc.SessionDescription
		if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		m := &webrtc.MediaEngine{}
		m.RegisterDefaultCodecs()
		api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

		pc, err := api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		track, err := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
			"video", "scrcpy")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := pc.AddTrack(track); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := pc.SetRemoteDescription(offer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := pc.SetLocalDescription(answer); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		<-webrtc.GatheringCompletePromise(pc)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pc.LocalDescription())

		go func() {
			for {
				pkt, err := protocol.Decode(stream)
				if err != nil {
					log.Println("stream read:", err)
					return
				}
				if pkt.Type == 0 {
					track.WriteSample(media.Sample{Data: pkt.Body, Duration: time.Second / 60})
				}
			}
		}()
	})

	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)
	log.Println("open http://localhost:8080 to view stream")
	log.Fatal(http.ListenAndServe(":8081", nil))
}
