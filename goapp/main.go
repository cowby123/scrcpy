// main.go — 線路格式對齊官方：觸控載荷 31B + type 1B = 總 32B；
// 觸控 ≤10 指映射；mouse/pen 固定用 ID 0；忽略滑鼠 hover move；
// DataChannel 收到控制訊號時印出原始資料並照官方順序編碼後直寫 control socket。
// ★ 新增：control socket 讀回解析（DeviceMessage：clipboard）、心跳 GET_CLIPBOARD、寫入 deadline/耗時告警。
// ★ 效能：解耦 ADB 讀取與 WebRTC 寫入，使用 channel 避免 I/O 阻塞；統一控制寫入 deadline。
// ★ v3: 縮小 channel 緩衝區以降低延遲。
// ★ v4: 模塊化重構 - 將代碼拆分為多個文件以提高可維護性

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" // 啟用 /debug/pprof
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yourname/scrcpy-go/adb"
)

// ========= 設備連接管理 =========

// connectAllDevices 掃描並連接所有可用的 ADB 設備
// 用途：自動發現並為每個在線的 Android 設備建立串流連接
func connectAllDevices() {
	getADBOptions := registerADBFlags(flag.CommandLine, "")
	baseOpts := getADBOptions()

	log.Println("[ADB] 掃描可用設備...")
	adbDevices, err := adb.ListDevices(baseOpts)
	if err != nil {
		log.Fatalf("[ADB] 列出設備失敗: %v", err)
	}

	if len(adbDevices) == 0 {
		log.Println("[ADB] 未發現任何設備，等待設備連接...")
		return
	}

	log.Printf("[ADB] 發現 %d 個設備", len(adbDevices))

	// 為每個在線設備啟動 streaming goroutine
	for _, dev := range adbDevices {
		if dev.State != "device" {
			log.Printf("[ADB] 跳過設備 %s (狀態: %s)", dev.Serial, dev.State)
			continue
		}

		deviceOpts := baseOpts
		deviceOpts.Serial = dev.Serial

		log.Printf("[ADB] 啟動設備連接: %s", dev.Serial)

		goSafe(fmt.Sprintf("device-%s", dev.Serial), func() {
			runAndroidStreaming(deviceOpts)
		})

		time.Sleep(500 * time.Millisecond)
	}
}

// setupHTTPServer 設定並啟動 HTTP/WebRTC 伺服器
// 用途：配置 Gin 路由、註冊端點、啟動 HTTP 服務
func setupHTTPServer() {
	// 設定 Gin 路由處理器
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		return fmt.Sprintf("[GIN] %s | %3d | %13v | %15s | %-7s %s\n",
			param.TimeStamp.Format("2006/01/02 15:04:05"),
			param.StatusCode,
			param.Latency,
			param.ClientIP,
			param.Method,
			param.Path,
		)
	}))
	router.Use(gin.Recovery())

	// 根路由：提供靜態檔案服務
	router.GET("/", func(c *gin.Context) {
		c.File("index.html")
	})

	// 靜態資源
	router.StaticFile("/web/index.html", "./web/index.html")
	router.Static("/assets", "./assets")

	// WebRTC SDP 交換端點
	router.POST("/offer", handleOfferGin)

	// 設備列表端點
	router.GET("/devices", handleDevicesGin)

	// 除錯用的堆疊追蹤端點
	router.GET("/debug/stack", func(c *gin.Context) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		c.Data(http.StatusOK, "text/plain; charset=utf-8", buf[:n])
	})

	// pprof 和 expvar 端點（需要 net/http 原生處理）
	router.GET("/debug/pprof/*any", gin.WrapH(http.DefaultServeMux))
	router.GET("/debug/vars", gin.WrapH(http.DefaultServeMux))

	// 啟動 HTTP 伺服器
	goSafe("http-server", func() {
		addr := ":8080"
		log.Println("[HTTP] 服務啟動:", addr, "（/ , /offer , /devices , /debug/pprof , /debug/vars , /debug/stack）")
		if err := router.Run(addr); err != nil {
			log.Fatal(err)
		}
	})
}

// ========= 伺服器入口 =========

// main 程式主入口函數
// 用途：初始化日誌、啟動 HTTP 伺服器、連接 ADB 設備
func main() {
	// 日誌級別參數
	logLevel := flag.String("log-level", "silent", "日誌級別: debug, info, error, silent")

	// 解析所有命令行參數
	flag.Parse()

	// 設定日誌級別
	switch *logLevel {
	case "debug":
		SetLogLevel(LogLevelDebug)
	case "info":
		SetLogLevel(LogLevelInfo)
	case "error":
		SetLogLevel(LogLevelError)
	case "silent":
		SetLogLevel(LogLevelSilent)
	default:
		SetLogLevel(LogLevelInfo)
	}

	// === 2. 配置日誌格式 ===
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	// === 3. 設定 HTTP 伺服器 ===
	setupHTTPServer()

	// === 4. 自動連接所有 ADB 設備 ===
	connectAllDevices()

	// === 5. 主程式保持運行 ===
	log.Println("[MAIN] 所有設備連接已啟動，主程式進入待命狀態")
	select {}
}
