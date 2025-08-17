// 這是一個以 Go 撰寫的簡易 scrcpy 客戶端範例，用於保存視訊串流
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/yourname/scrcpy-go/adb"
)

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

	// 讀取視訊串流並保存
	frameCount := 0
	totalBytes := int64(0)
	startTime := time.Now()
	const (
		flagConfig   = uint64(1) << 63
		flagKeyFrame = uint64(1) << 62
	)
	meta := make([]byte, 12) // scrcpy: [pts+flags(8)] + [size(4)]
	var (
		prevPTS uint64
		conf    []byte
	)
	go RunRTC()
	for {
		// 讀取影格中繼資料
		if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
			log.Println("read frame meta:", err)
			break
		}
		ptsAndFlags := binary.BigEndian.Uint64(meta[:8])
		frameSize := binary.BigEndian.Uint32(meta[8:12])

		// 依照封包長度讀取完整影格
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
			log.Println("read frame:", err)
			break
		}

		// 寫入影格資料
		if _, err := outFile.Write(frame); err != nil {
			log.Println("write error:", err)
			break
		}

		isConfig := ptsAndFlags&flagConfig != 0
		isKey := ptsAndFlags&flagKeyFrame != 0
		pts := ptsAndFlags &^ (flagConfig | flagKeyFrame)

		annexb := avccToAnnexB(frame)
		if isConfig {
			// store SPS/PPS to prepend to next keyframe
			conf = append(conf[:0], annexb...)
			setProfileLevelID(extractProfileLevelID(annexb))
			continue
		}
		if isKey && len(conf) > 0 {
			annexb = append(conf, annexb...)
		}

		duration := time.Second / 30
		if prevPTS != 0 && pts > prevPTS {
			duration = time.Duration(pts-prevPTS) * time.Microsecond
		}
		prevPTS = pts
		frameCh <- Frame{Data: annexb, Duration: duration, Key: isKey}

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
}

// avccToAnnexB converts H.264 NAL units from AVCC (length-prefixed) to Annex-B.
// If the data already contains Annex-B start codes, it is returned unchanged.
func avccToAnnexB(data []byte) []byte {
	if bytes.HasPrefix(data, []byte{0x00, 0x00, 0x00, 0x01}) ||
		bytes.HasPrefix(data, []byte{0x00, 0x00, 0x01}) {
		return data
	}
	out := make([]byte, 0, len(data)+len(data)/4*4)
	for len(data) >= 4 {
		n := int(binary.BigEndian.Uint32(data[:4]))
		data = data[4:]
		if n <= 0 || len(data) < n {
			break
		}
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return out
}

// extractProfileLevelID returns profile-level-id from SPS NAL in Annex-B data.
func extractProfileLevelID(conf []byte) string {
	const def = "42e01f"
	start := []byte{0x00, 0x00, 0x00, 0x01}
	for i := 0; i+4 < len(conf); {
		if bytes.Equal(conf[i:i+4], start) {
			i += 4
			if i >= len(conf) {
				break
			}
			nalType := conf[i] & 0x1F
			if nalType == 7 && i+4 <= len(conf) {
				return fmt.Sprintf("%02x%02x%02x", conf[i+1], conf[i+2], conf[i+3])
			}
			for i+4 < len(conf) && !bytes.Equal(conf[i:i+4], start) {
				i++
			}
		} else {
			i++
		}
	}
	return def
}
