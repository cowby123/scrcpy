# Gin 日誌層級設定說明

## 1. 基本模式設定

### 三種運行模式

```go
// 開發模式 (預設) - 顯示完整日誌
gin.SetMode(gin.DebugMode)

// 生產模式 - 減少日誌輸出
gin.SetMode(gin.ReleaseMode)

// 測試模式
gin.SetMode(gin.TestMode)
```

也可以通過環境變數設定：
```bash
# Windows PowerShell
$env:GIN_MODE="release"
.\test.exe

# Linux/Mac
export GIN_MODE=release
./test
```

## 2. 路由器創建方式

### Default() - 包含預設中間件
```go
// 自動包含 Logger 和 Recovery 中間件
router := gin.Default()
```

### New() - 完全自定義
```go
// 不包含任何中間件，可完全控制
router := gin.New()
router.Use(gin.Logger())    // 手動添加日誌
router.Use(gin.Recovery())  // 手動添加恢復
```

## 3. 自定義日誌格式

### 簡單格式
```go
router.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
    return fmt.Sprintf("[GIN] %s | %3d | %s\n",
        param.TimeStamp.Format("15:04:05"),
        param.StatusCode,
        param.Path,
    )
}))
```

### 詳細格式（當前使用）
```go
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
```

輸出範例：
```
[GIN] 2025/10/25 05:30:45 | 200 |      1.234ms |   192.168.1.10 | GET     /devices
[GIN] 2025/10/25 05:30:46 | 200 |     45.678ms |   192.168.1.10 | POST    /offer
```

## 4. 條件性日誌

### 跳過特定路徑
```go
router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
    // 不記錄這些路徑
    SkipPaths: []string{"/health", "/ping"},
}))
```

### 只記錄錯誤
```go
router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
    // 只記錄狀態碼 >= 400 的請求
    SkipPaths: []string{},
    Formatter: func(param gin.LogFormatterParams) string {
        if param.StatusCode < 400 {
            return ""
        }
        return fmt.Sprintf("[ERROR] %d %s %s\n",
            param.StatusCode,
            param.Method,
            param.Path,
        )
    },
}))
```

## 5. 輸出到文件

### 寫入文件
```go
import (
    "os"
    "io"
)

// 創建日誌文件
logFile, err := os.Create("gin.log")
if err != nil {
    log.Fatal(err)
}

// 設定輸出到文件和終端
gin.DefaultWriter = io.MultiWriter(logFile, os.Stdout)

router := gin.Default()
```

### 分離訪問日誌和錯誤日誌
```go
// 訪問日誌
accessLog, _ := os.Create("access.log")
gin.DefaultWriter = io.MultiWriter(accessLog, os.Stdout)

// 錯誤日誌
errorLog, _ := os.Create("error.log")
gin.DefaultErrorWriter = io.MultiWriter(errorLog, os.Stderr)
```

## 6. 關閉日誌

### 完全關閉 Gin 日誌
```go
import "io"

// 丟棄所有輸出
gin.DefaultWriter = io.Discard

router := gin.New()
router.Use(gin.Recovery()) // 只保留 panic 恢復
```

### 只在生產環境關閉
```go
if gin.Mode() == gin.ReleaseMode {
    gin.DefaultWriter = io.Discard
}
```

## 7. 與 logrus/zap 整合

### 使用 logrus
```go
import (
    "github.com/sirupsen/logrus"
    "github.com/gin-gonic/gin"
)

logger := logrus.New()
logger.SetFormatter(&logrus.JSONFormatter{})

router := gin.New()
router.Use(func(c *gin.Context) {
    start := time.Now()
    c.Next()
    
    logger.WithFields(logrus.Fields{
        "status":  c.Writer.Status(),
        "method":  c.Request.Method,
        "path":    c.Request.URL.Path,
        "latency": time.Since(start),
        "ip":      c.ClientIP(),
    }).Info("HTTP Request")
})
```

### 使用 zap
```go
import (
    "go.uber.org/zap"
    "github.com/gin-gonic/gin"
)

logger, _ := zap.NewProduction()
defer logger.Sync()

router := gin.New()
router.Use(func(c *gin.Context) {
    start := time.Now()
    c.Next()
    
    logger.Info("HTTP Request",
        zap.Int("status", c.Writer.Status()),
        zap.String("method", c.Request.Method),
        zap.String("path", c.Request.URL.Path),
        zap.Duration("latency", time.Since(start)),
        zap.String("ip", c.ClientIP()),
    )
})
```

## 8. 當前項目配置

目前使用的配置：
```go
// 生產模式（減少輸出）
gin.SetMode(gin.ReleaseMode)

// 自定義格式
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
```

## 9. 建議配置

### 開發環境
```go
gin.SetMode(gin.DebugMode)
router := gin.Default() // 顯示完整日誌
```

### 測試環境
```go
gin.SetMode(gin.TestMode)
router := gin.Default()
```

### 生產環境
```go
gin.SetMode(gin.ReleaseMode)
router := gin.New()
router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
    SkipPaths: []string{"/health"},  // 跳過健康檢查
}))
router.Use(gin.Recovery())
```

## 10. 動態調整

可以通過命令行參數動態調整：
```go
var logLevel = flag.String("log", "release", "日誌級別: debug, release, test")

func main() {
    flag.Parse()
    
    switch *logLevel {
    case "debug":
        gin.SetMode(gin.DebugMode)
    case "test":
        gin.SetMode(gin.TestMode)
    default:
        gin.SetMode(gin.ReleaseMode)
    }
    
    // ... 其他配置
}
```

使用方式：
```bash
# 開發模式
.\test.exe -log debug

# 生產模式
.\test.exe -log release
```
