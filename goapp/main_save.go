// 這是一個以 Go 撰寫的簡易 scrcpy 客戶端範例，用於保存視訊串流
package main

import (
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

	// 讀取視訊串流並保存
	frameCount := 0
	startTime := time.Now()

	for {
		// 直接讀取並寫入所有收到的資料
		buffer := make([]byte, 4096)
		n, err := conn.VideoStream.Read(buffer)
		if err != nil {
			log.Println("read error:", err)
			break
		}

		// 直接寫入檔案
		_, err = outFile.Write(buffer[:n])
		if err != nil {
			log.Println("write error:", err)
			break
		}

		// 定期顯示已接收的資料量
		frameCount++
		if frameCount%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			bytesPerSecond := float64(frameCount*4096) / elapsed
			log.Printf("接收資料量: %.2f MB, 速率: %.2f MB/s\n",
				float64(frameCount*4096)/(1024*1024),
				bytesPerSecond/(1024*1024))
		}
	}
}
