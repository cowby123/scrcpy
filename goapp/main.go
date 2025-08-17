// 這是一個以 Go 撰寫的簡易 scrcpy 客戶端範例，用於保存視訊串流
package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
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

	log.Println("開始接收視訊串流，透過瀏覽器播放")

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
	meta := make([]byte, 12) // scrcpy: [pts(8)] + [size(4)]
	go RunRTC()
	for {
		// 讀取影格中繼資料
		if _, err := io.ReadFull(conn.VideoStream, meta); err != nil {
			log.Println("read frame meta:", err)
			break
		}
		frameSize := binary.BigEndian.Uint32(meta[8:12])

		// 依照封包長度讀取完整影格
		frame := make([]byte, frameSize)
		if _, err := io.ReadFull(conn.VideoStream, frame); err != nil {
			log.Println("read frame:", err)
			break
		}

		// 將影格推送到 WebRTC 連線
		select {
		case videoCh <- frame:
		default:
			// 若沒有接收者或通道已滿，略過此影格
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
}
