# 快速使用範例

## 1. 查看連接的設備
```powershell
adb devices
```

輸出範例：
```
List of devices attached
ABCD1234        device
192.168.1.100:5555      device
```

## 2. 連接到 USB 設備
```powershell
go run . -device ABCD1234
```

## 3. 連接到 WiFi 設備

### 步驟 A：啟用 WiFi ADB（首次需要 USB 連接）
```powershell
# 1. 先用 USB 連接設備
adb devices

# 2. 啟用 TCP/IP 模式（端口 5555）
adb tcpip 5555

# 3. 查看設備 IP（在手機設定 → 關於手機 → 狀態資訊中查看）
# 或者使用：
adb shell ip addr show wlan0
```

### 步驟 B：無線連接
```powershell
# 1. 連接到設備（替換成你的設備 IP）
adb connect 192.168.1.100:5555

# 2. 確認連接成功
adb devices

# 3. 啟動程式
go run . -device 192.168.1.100:5555
```

## 4. 多設備情況

如果有多個設備連接，不指定 `-device` 參數會使用第一個設備：
```powershell
# 自動選擇（可能不是你想要的設備）
go run .

# 明確指定設備 1
go run . -device ABCD1234

# 明確指定設備 2
go run . -device 192.168.1.100:5555
```

## 5. 自訂端口
```powershell
# 如果預設端口被占用，可以更改
go run . -device ABCD1234 -scrcpy-port 28000
```

## 6. 存取 Web 介面
程式啟動後，開啟瀏覽器訪問：
```
http://localhost:8080
```

## 7. 除錯資訊
- 查看統計資訊：http://localhost:8080/debug/vars
- 查看效能分析：http://localhost:8080/debug/pprof
- 查看堆疊追蹤：http://localhost:8080/debug/stack

## 常見問題

### Q: 找不到設備？
```powershell
# 確認 ADB 服務正常
adb devices

# 重新啟動 ADB 服務
adb kill-server
adb start-server
```

### Q: WiFi 連接失敗？
1. 確認手機和電腦在同一網路
2. 確認已執行 `adb tcpip 5555`
3. 確認防火牆沒有阻擋 5555 端口
4. 嘗試重新連接：`adb disconnect` 然後 `adb connect IP:5555`

### Q: 端口被占用？
```powershell
# Windows 查看端口占用
netstat -ano | findstr :27183

# 使用其他端口
go run . -scrcpy-port 28000
```

## 編譯執行檔
```powershell
# 編譯
go build -o scrcpy-server.exe .

# 執行
.\scrcpy-server.exe -device ABCD1234
```
