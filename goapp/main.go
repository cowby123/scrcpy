// 這是一個以 Go 撰寫的簡易 scrcpy 客戶端範例
package main

import (
	"log"

	"github.com/yourname/scrcpy-go/adb"
)

// scrcpy 伺服器檔案的路徑
const serverJar = "../assets/scrcpy-server"

func main() {
	// 連線到第一台可用的裝置
	dev, err := adb.NewDevice("")
	if err != nil {
		log.Fatal(err)
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

	// // 初始化影片解碼器
	// dec, err := video.NewDecoder()
	// if err != nil {
	// 	log.Fatal("init decoder:", err)
	// }

	// // 建立顯示視窗
	// disp, err := video.NewDisplay("scrcpy-go", 720, 1280)
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// defer disp.Close()

	// // 主迴圈：讀取封包、顯示畫面並處理輸入
	// for {
	// 	pkt, err := protocol.Decode(stream)
	// 	if err != nil {
	// 		log.Println("read:", err)
	// 		break
	// 	}
	// 	if pkt.Type == 0 { // 視訊封包
	// 		frame, ok, err := dec.Decode(pkt.Body)
	// 		if err != nil {
	// 			log.Println("decode:", err)
	// 			continue
	// 		}
	// 		if ok { // 取得完整畫面後進行渲染
	// 			videoData := frame.Data()
	// 			disp.Render(videoData)
	// 		}
	// 	}
	// 	// 取得鍵盤滑鼠事件（尚未傳送回裝置）
	// 	events := input.Capture()
	// 	_ = events // 未來可透過 encoder 發送控制訊息
	// 	if !disp.Poll() {
	// 		break
	// 	}
	// }
}
