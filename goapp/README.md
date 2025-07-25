# scrcpy-go 簡介

這個目錄包含以 Go 撰寫的 scrcpy 客戶端範例，示範如何透過 ADB 啟動
scrcpy 伺服器並在本機端顯示裝置畫面。

## 需求
- Go 1.20 以上
- 已安裝 `adb` 並可與 Android 裝置連線
- 系統需能安裝 SDL2、FFmpeg 以及 robotgo 相關依賴

## 執行方式
```bash
cd goapp
go run .
```
程式會自動推送 `../server/scrcpy-server.jar` 至裝置並透過 `adb reverse`
將裝置的 `localabstract:scrcpy` 轉發至本機 `tcp:27183`，接著啟動伺服器，
之後會開啟視窗顯示畫面，並於終端輸出錯誤訊息（若有）。

此範例僅提供影片顯示功能，輸入事件捕捉後並未送回裝置，可依需求在
`input` 與 `protocol` 套件中擴充。
