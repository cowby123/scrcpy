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

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/yourname/scrcpy-go/adb"
)

// === 全域狀態 ===
var (
	videoTrack   *webrtc.TrackLocalStaticRTP
	peerConn     *webrtc.PeerConnection
	packetizer   rtp.Packetizer
	rtpTS        uint32
	needKeyframe bool
	lastSPS      []byte
	lastPPS      []byte
	stateMu      sync.RWMutex

	startTime time.Time
	lastPTS   uint64
)

func main() {
	// 靜態檔案伺服器
	http.Handle("/", http.FileServer(http.Dir("./web")))
	http.HandleFunc("/offer", handleOffer)
	go func() {
		log.Println("HTTP 伺服器: http://localhost:8080/ (POST /offer)")
		log.Fatal(http.ListenAndServe(":8080", nil))
	}()

	// 連線到 Android
	dev, err := adb.NewDevice("")
	if err != nil {
		log.Fatal(err)
	}
	if err := dev.Reverse("localabstract:scrcpy", "tcp:27183"); err != nil {
		log.Fatal("reverse:", err)
	}
	if err := dev.PushServer("./assets/scrcpy-server"); err != nil {
		log.Fatal("push server:", err)
	}
	conn, err := dev.StartServer()
	if err != nil {
		log.Fatal("start server:", err)
	}
	defer conn.VideoStream.Close()
	defer conn.Control.Close()

	// debug 檔案
	outFile, _ := os.Create("output.h264")
	defer outFile.Close()

	// 跳過裝置名稱
	nameBuf := make([]byte, 64)
	if _, err := io.ReadFull(conn.VideoStream, nameBuf); err != nil {
		log.Fatal("read device name:", err)
	}
	log.Printf("裝置名稱: %s\n", string(bytes.TrimRight(nameBuf, "\x00")))

	// 視訊標頭
	vHeader := make([]byte, 12)
	if _, err := io.ReadFull(conn.VideoStream, vHeader); err != nil {
		log.Fatal("read video header:", err)
	}
	codec := string(vHeader[:4])
	width := binary.BigEndian.Uint32(vHeader[4:8])
	height := binary.BigEndian.Uint32(vHeader[8:12])
	log.Printf("編碼格式: %s, 解析度: %dx%d\n", codec, width, height)

	// frame loop
	meta := make([]byte, 12)
	startTime = time.Now()
	var frameCount int

	for {
		if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
			log.Println("read frame meta error:", err)
			continue // ⚠️ 不要 break
		}
		ptsAndFlags := binary.BigEndian.Uint64(meta[:8])
		isConfig := (ptsAndFlags & (1 << 63)) != 0
		pts := ptsAndFlags &^ (1<<63 | 1<<62)
		frameSize := binary.BigEndian.Uint32(meta[8:12])

		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
			log.Println("read frame data error:", err)
			continue
		}

		// 存檔
		_, _ = outFile.Write(frame)

		// 拆 NALUs
		nalus := splitAnnexBNALUs(frame)
		idr := false
		for _, n := range nalus {
			switch naluType(n) {
			case 7:
				stateMu.Lock()
				lastSPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 8:
				stateMu.Lock()
				lastPPS = append([]byte(nil), n...)
				stateMu.Unlock()
			case 5:
				idr = true
			}
		}

		// WebRTC 推送
		stateMu.RLock()
		vt, pk, waitKF := videoTrack, packetizer, needKeyframe
		stateMu.RUnlock()

		if vt != nil && pk != nil {
			if isConfig {
				sendNALUs(nalus...)
				goto stats
			}
			if waitKF {
				// 先送參數集
				stateMu.RLock()
				sps, pps := lastSPS, lastPPS
				stateMu.RUnlock()
				if len(sps) > 0 && len(pps) > 0 {
					sendNALUs(sps, pps)
				}
				if !idr {
					goto stats
				}
				// ✅ 收到 IDR → 清掉 needKeyframe
				log.Println("送出 IDR Access Unit (清掉 needKeyframe)")
				stateMu.Lock()
				needKeyframe = false
				stateMu.Unlock()
				if len(lastSPS) > 0 && len(lastPPS) > 0 {
					sendNALUs(lastSPS, lastPPS)
				}
				sendNALUAccessUnit(nalus, pts)
			} else {
				if idr {
					if len(lastSPS) > 0 && len(lastPPS) > 0 {
						sendNALUs(lastSPS, lastPPS)
					}
					log.Println("送出 IDR Access Unit")
				}
				sendNALUAccessUnit(nalus, pts)
			}
		}

	stats:
		frameCount++
		if frameCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			log.Printf("已接收 %d 幀 (%.2f fps)\n", frameCount, float64(frameCount)/elapsed)
		}
	}
}

func handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "invalid offer", http.StatusBadRequest)
		return
	}

	m := webrtc.MediaEngine{}
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}},
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo)

	ir := &interceptor.Registry{}
	_ = webrtc.RegisterDefaultInterceptors(&m, ir)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&m), webrtc.WithInterceptorRegistry(ir))

	pc, _ := api.NewPeerConnection(webrtc.Configuration{})
	peerConn = pc

	track, _ := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000},
		"video", "scrcpy",
	)
	sender, _ := pc.AddTrack(track)

	// RTCP 解析 (PLI/FIR)
	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				log.Println("RTCP read error:", err)
				return
			}
			for _, p := range pkts {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					log.Println("RTCP: PLI/FIR → needKeyframe=true")
					stateMu.Lock()
					needKeyframe = true
					stateMu.Unlock()
				}
			}
		}
	}()

	if err := pc.SetRemoteDescription(offer); err != nil {
		http.Error(w, "set remote error", http.StatusInternalServerError)
		return
	}
	answer, _ := pc.CreateAnswer(nil)
	_ = pc.SetLocalDescription(answer)
	<-webrtc.GatheringCompletePromise(pc)

	stateMu.Lock()
	videoTrack = track
	packetizer = rtp.NewPacketizer(
		1200, 96, uint32(time.Now().UnixNano()),
		&codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000,
	)
	rtpTS, lastPTS, needKeyframe = 0, 0, true
	stateMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pc.LocalDescription())
}

// === RTP 傳送 ===
func sendNALUAccessUnit(nalus [][]byte, pts uint64) {
	stateMu.RLock()
	pk, vt, ts, prevPts := packetizer, videoTrack, rtpTS, lastPTS
	stateMu.RUnlock()
	if pk == nil || vt == nil || len(nalus) == 0 {
		return
	}

	if prevPts != 0 {
		delta := pts - prevPts
		if delta == 0 {
			delta = 16666 // 保底值 ≈ 60fps
		}
		ts += uint32(delta * 90000 / 1000000)
	}

	for i, n := range nalus {
		pkts := pk.Packetize(n, ts)
		for j, p := range pkts {
			p.Marker = (i == len(nalus)-1) && (j == len(pkts)-1)
			if err := vt.WriteRTP(p); err != nil {
				log.Println("RTP write error:", err)
			}
		}
	}
	stateMu.Lock()
	rtpTS, lastPTS = ts, pts
	stateMu.Unlock()
}

func sendNALUs(nalus ...[]byte) {
	stateMu.RLock()
	pk, vt, ts := packetizer, videoTrack, rtpTS
	stateMu.RUnlock()
	if pk == nil || vt == nil {
		return
	}
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		pkts := pk.Packetize(n, ts)
		for _, p := range pkts {
			p.Marker = false
			_ = vt.WriteRTP(p)
		}
	}
}

// === Annex-B Parser ===
func splitAnnexBNALUs(b []byte) [][]byte {
	var nalus [][]byte
	i := 0
	for {
		s, e := findStartCode(b, i)
		if s < 0 {
			break
		}
		n, _ := findStartCode(b, e)
		if n < 0 {
			if len(b[e:]) > 0 {
				nalus = append(nalus, b[e:])
			}
			break
		}
		if len(b[e:n]) > 0 {
			nalus = append(nalus, b[e:n])
		}
		i = n
	}
	return nalus
}

func findStartCode(b []byte, from int) (int, int) {
	for i := from; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i, i + 3
		}
		if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return i, i + 4
		}
	}
	return -1, -1
}

func naluType(n []byte) uint8 {
	if len(n) == 0 {
		return 0
	}
	return n[0] & 0x1F
}
