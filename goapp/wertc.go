package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// 可調整的參數
var (
	listenAddr  = ":8888" // HTTP 監聽埠
	videoWidth  = "1280"
	videoHeight = "720"
	videoFPS    = "30"     // 例如 "30"
	ffmpegPath  = "ffmpeg" // 若不在 PATH 請改絕對路徑
	ffmpegColor = "white"  // 支援 ffmpeg color 名稱或 #RRGGBB
)

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

	// 5) ffmpeg 產生 H.264 (Annex-B) 串流 → 以 h264reader 解析 → 聚合 AU 後寫入 Track
	go func() {
		fps := 30
		if v, err := strconv.Atoi(videoFPS); err == nil && v > 0 && v <= 240 {
			fps = v
		}
		frameDuration := time.Second / time.Duration(fps)

		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "color=c=" + ffmpegColor + ":s=" + videoWidth + "x" + videoHeight + ":r=" + videoFPS,
			"-vcodec", "libx264",
			"-pix_fmt", "yuv420p",
			"-tune", "zerolatency",
			"-preset", "veryfast",
			"-profile:v", "baseline",
			"-level", "3.1",
			"-x264-params", "repeat-headers=1:scenecut=0:open_gop=0:keyint=" + videoFPS + ":min-keyint=" + videoFPS,
			"-an",
			"-f", "h264", "pipe:1",
		}
		cmd := exec.Command(ffmpegPath, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Println("ffmpeg stdout:", err)
			return
		}
		cmd.Stderr = os.Stderr
		if err = cmd.Start(); err != nil {
			log.Println("ffmpeg start:", err)
			return
		}
		defer func() { _ = cmd.Process.Kill() }()

		// 當連線結束時，結束 ffmpeg 讓讀取 goroutine 收到 EOF
		go func() { <-closed; _ = cmd.Process.Kill() }()

		r, err := h264reader.NewReader(stdout)
		if err != nil {
			log.Println("h264 reader:", err)
			return
		}

		naluCh := make(chan *h264reader.NAL, 128)
		errCh := make(chan error, 1)

		go func() {
			defer close(naluCh)
			for {
				n, err := r.NextNAL()
				if err != nil {
					errCh <- err
					return
				}
				naluCh <- n
			}
		}()

		ticker := time.NewTicker(frameDuration)
		defer ticker.Stop()

		var (
			curFrame  []byte
			haveSlice bool
			startCode = []byte{0x00, 0x00, 0x00, 0x01}
			flush     = func() {
				if len(curFrame) == 0 {
					return
				}
				if err := videoTrack.WriteSample(media.Sample{Data: curFrame, Duration: frameDuration}); err != nil {
					log.Println("write sample:", err)
				}
				curFrame = curFrame[:0]
				haveSlice = false
			}
		)

		for {
			select {
			case <-closed:
				return
			case <-ticker.C:
				if haveSlice {
					flush()
				}
			case err := <-errCh:
				if err == io.EOF {
					return
				}
				log.Println("h264 read:", err)
				return
			case nalu, ok := <-naluCh:
				if !ok {
					return
				}
				if nalu == nil {
					continue
				}
				curFrame = append(curFrame, startCode...)
				curFrame = append(curFrame, nalu.Data...)

				switch nalu.UnitType {
				case h264reader.NalUnitTypeCodedSliceIdr, h264reader.NalUnitTypeCodedSliceNonIdr:
					haveSlice = true
				case h264reader.NalUnitTypeAUD:
					if haveSlice {
						flush()
					}
				}
			}
		}
	}()

	// 6) 回傳 SDP Answer
	_ = json.NewEncoder(w).Encode(pc.LocalDescription())
}
