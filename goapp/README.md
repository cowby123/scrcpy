# scrcpy-go 簡介

這個目錄包含以 Go 撰寫的 scrcpy 客戶端範例，示範如何透過 ADB 啟動
scrcpy 伺服器並在本機端顯示裝置畫面。

## 需求
- Go 1.20 以上
- 已安裝 `adb` 並可與 Android 裝置連線
- 系統需能安裝 SDL2、FFmpeg 以及 robotgo 相關依賴

## 執行方式

### 基本使用（自動選擇設備）
```bash
cd goapp
go run .
```

### 指定設備連接

#### USB 設備
```bash
# 先查看可用設備
adb devices

# 使用設備序號連接
go run . -device ABCD1234
```

#### WiFi 設備
```bash
# 先透過 USB 啟用 WiFi ADB
adb tcpip 5555

# 連接到設備 IP（確保設備與電腦在同一網路）
adb connect 192.168.1.100:5555

# 使用 IP:Port 連接
go run . -device 192.168.1.100:5555
```

### 其他參數
```bash
# 自訂 ADB 伺服器位址和端口
go run . -adb-host 127.0.0.1 -adb-port 5037

# 自訂 scrcpy 本地端口
go run . -scrcpy-port 27183

# 組合使用
go run . -device 192.168.1.100:5555 -scrcpy-port 28000
```

## 參數說明
- `-device <序號或IP:Port>`: 指定要連接的 Android 設備（留空則自動選擇第一個可用設備）
- `-adb-serial <序號或IP:Port>`: 同 `-device`（相容舊版參數）
- `-adb-host <位址>`: ADB 伺服器主機位址（預設：127.0.0.1）
- `-adb-port <端口>`: ADB 伺服器端口（預設：5037）
- `-scrcpy-port <端口>`: scrcpy 反向連接使用的本地端口（預設：27183）

## 運作原理
程式會自動推送 `../server/scrcpy-server.jar` 至裝置並透過 `adb reverse`
將裝置的 `localabstract:scrcpy` 轉發至本機 `tcp:27183`，接著啟動伺服器，
之後會開啟視窗顯示畫面，並於終端輸出錯誤訊息（若有）。

此範例僅提供影片顯示功能，輸入事件捕捉後並未送回裝置，可依需求在
`input` 與 `protocol` 套件中擴充。

## 系統依賴
```bash
sudo apt install libavcodec-dev libavformat-dev libavutil-dev libswscale-dev pkg-config
```
