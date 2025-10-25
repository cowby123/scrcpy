# log.Fatal / log.Printf 日誌級別設定

## 快速使用

### 命令行參數控制

```bash
# 1. Debug 模式 - 顯示所有日誌（包括 [DEBUG], [INFO], [ERROR]）
.\test.exe -log-level debug

# 2. Info 模式 - 顯示一般信息（包括 [INFO], [ERROR]）【預設】
.\test.exe -log-level info
.\test.exe  # 不加參數預設是 info

# 3. Error 模式 - 只顯示錯誤（只有 [ERROR]）
.\test.exe -log-level error

# 4. Silent 模式 - 完全靜默（不顯示任何日誌）
.\test.exe -log-level silent
```

### 配合其他參數使用

```bash
# 指定設備 + 靜默模式
.\test.exe -device 192.168.66.120:5555 -log-level silent

# 指定端口 + Debug 模式
.\test.exe -addr :8080 -log-level debug
```

## 日誌級別說明

| 級別 | 數值 | 顯示內容 | 用途 |
|------|------|----------|------|
| **debug** | 0 | 所有日誌 | 開發調試，查看詳細流程 |
| **info** | 1 | INFO + ERROR | 生產環境，正常運行日誌 |
| **error** | 2 | 只有 ERROR | 生產環境，只關注錯誤 |
| **silent** | 3 | 不顯示 | 完全靜默，適合後台運行 |

## 代碼中使用

### 舊的寫法（保持兼容）
```go
log.Printf("[VIDEO] 裝置名稱: %s", deviceName)
log.Fatal("[VIDEO] read device name:", err)
```

### 新的寫法（推薦）
```go
// 調試信息（只在 debug 模式顯示）
LogDebug("[VIDEO] 開始讀取幀數據")

// 一般信息（info 和 debug 模式顯示）
LogInfo("[VIDEO] 裝置名稱: %s", deviceName)

// 錯誤信息（error, info, debug 模式顯示）
LogError("[VIDEO] 讀取幀失敗: %v", err)

// 致命錯誤（總是顯示並退出程序）
LogFatal("[VIDEO] 無法連接設備: %v", err)
```

## 遷移示例

### 原始代碼
```go
func processVideo() {
    log.Printf("[VIDEO] 開始接收視訊串流")
    
    deviceName, err := readDeviceName()
    if err != nil {
        log.Fatal("[VIDEO] read device name:", err)
    }
    
    log.Printf("[VIDEO] 裝置名稱: %s", deviceName)
}
```

### 遷移後
```go
func processVideo() {
    LogInfo("[VIDEO] 開始接收視訊串流")  // 一般信息
    
    deviceName, err := readDeviceName()
    if err != nil {
        LogFatal("[VIDEO] read device name: %v", err)  // 致命錯誤
    }
    
    LogInfo("[VIDEO] 裝置名稱: %s", deviceName)  // 一般信息
}
```

## 實際輸出示例

### Debug 模式
```
$ .\test.exe -log-level debug

2025/10/26 15:30:45 [DEBUG] [ADB] 開始連接設備
2025/10/26 15:30:45 [INFO] [ADB][192.168.66.120:5555] scrcpy server 已啟動
2025/10/26 15:30:45 [DEBUG] [VIDEO] 讀取視訊頭
2025/10/26 15:30:45 [INFO] [VIDEO] 裝置名稱: Pixel 6
2025/10/26 15:30:45 [INFO] [VIDEO] 編碼ID: 4, 初始解析度: 1080x2400
2025/10/26 15:30:46 [DEBUG] [RTP] 發送幀 #1
```

### Info 模式（預設）
```
$ .\test.exe

2025/10/26 15:30:45 [INFO] [ADB][192.168.66.120:5555] scrcpy server 已啟動
2025/10/26 15:30:45 [INFO] [VIDEO] 裝置名稱: Pixel 6
2025/10/26 15:30:45 [INFO] [VIDEO] 編碼ID: 4, 初始解析度: 1080x2400
```

### Error 模式
```
$ .\test.exe -log-level error

（正常運行時不顯示任何日誌，只在出錯時顯示）
2025/10/26 15:30:50 [ERROR] [VIDEO] read frame: EOF
```

### Silent 模式
```
$ .\test.exe -log-level silent

（完全無輸出，除非遇到 LogFatal）
```

## 程序化控制

### 在代碼中動態修改級別
```go
// 在 main() 中
func main() {
    // 初始設定為 info
    SetLogLevel(LogLevelInfo)
    
    // 如果檢測到某個條件，切換到 debug
    if os.Getenv("DEBUG") == "1" {
        SetLogLevel(LogLevelDebug)
    }
    
    // 開始運行...
}
```

### 環境變數控制
```go
func main() {
    // 從環境變數讀取
    logLevel := os.Getenv("LOG_LEVEL")
    switch logLevel {
    case "debug":
        SetLogLevel(LogLevelDebug)
    case "error":
        SetLogLevel(LogLevelError)
    case "silent":
        SetLogLevel(LogLevelSilent)
    default:
        SetLogLevel(LogLevelInfo)
    }
}
```

使用方式：
```bash
# Windows PowerShell
$env:LOG_LEVEL="debug"; .\test.exe

# Linux/Mac
export LOG_LEVEL=debug
./test
```

## 最佳實踐

### 1. 根據重要性選擇級別

- `LogDebug()` - 調試信息、詳細流程、臨時變數值
- `LogInfo()` - 重要狀態變化、成功操作、關鍵指標
- `LogError()` - 錯誤但可恢復的情況
- `LogFatal()` - 致命錯誤，無法繼續運行

### 2. 開發階段
```bash
# 使用 debug 級別看到所有細節
.\test.exe -log-level debug
```

### 3. 生產部署
```bash
# 使用 error 級別減少日誌量
.\test.exe -log-level error

# 或使用 info 級別保留關鍵信息
.\test.exe -log-level info
```

### 4. 後台服務
```bash
# 使用 silent 級別完全靜默
.\test.exe -log-level silent > nul 2>&1
```

## 與 Gin 日誌的關係

**兩個獨立的系統**：

1. **Gin 日誌** - HTTP 請求日誌
   - 通過 `gin.SetMode()` 控制
   - 記錄 HTTP 請求/響應

2. **應用日誌** - 業務邏輯日誌
   - 通過 `-log-level` 參數控制
   - 記錄 ADB、視訊處理、WebRTC 等

### 組合使用示例
```bash
# Gin Release 模式 + 應用 Error 級別（最少日誌）
$env:GIN_MODE="release"
.\test.exe -log-level error

# Gin Debug 模式 + 應用 Debug 級別（最多日誌）
$env:GIN_MODE="debug"
.\test.exe -log-level debug
```

## 注意事項

1. **LogFatal 總是顯示** - 無論什麼級別，致命錯誤總會顯示並退出程序
2. **性能影響** - Debug 模式會產生大量日誌，生產環境建議使用 info 或 error
3. **舊代碼兼容** - 原有的 `log.Printf()` 和 `log.Fatal()` 仍然可用
4. **建議逐步遷移** - 將關鍵日誌改用新的 `LogXxx()` 函數，獲得更好的控制
