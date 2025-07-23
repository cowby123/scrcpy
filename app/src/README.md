# src 目錄導覽

此目錄包含 scrcpy 客戶端主要的 C 原始碼。檔案依功能劃分多個子目錄，以下簡要說明：

## 子目錄

- `adb/`：與 Android Debug Bridge 溝通的實作，包含連線管理與指令處理。
- `android/`：Android 相關常數與結構定義，例如鍵盤按鍵碼與輸入事件格式。
- `hid/`：建立鍵盤、滑鼠及遊戲控制器等 HID 事件。
- `sys/`：與作業系統相關的程式碼，依 `unix/` 與 `win/` 區分實作。
- `trait/`：定義資料來源、接收端等抽象介面，供其他模組實作。
- `usb/`：處理 USB 與 OTG 模式下的輸入與畫面傳輸。
- `uhid/`：透過 Linux `uhid` 介面模擬虛擬輸入裝置。
- `util/`：通用工具函式庫，提供字串、執行緒、網路、記憶體等輔助功能。

## 主要檔案

- `main.c`：程式進入點，解析指令列並啟動 scrcpy。
- `scrcpy.c`／`scrcpy.h`：初始化各元件並執行主迴圈。
- `server.c`／`server.h`：與裝置端 server 建立連線並協調通訊。
- `controller.c`／`controller.h`：於獨立執行緒傳送控制訊息。
- `decoder.c`／`demuxer.c`：接收封包、解碼視訊與音訊資料。
- `display.c`／`screen.c`：將解碼後的畫面繪製到視窗。
- `recorder.c`：將視訊與音訊封存為檔案。
- 其他模組如 `audio_player.*`、`input_manager.*`、`file_pusher.*` 提供音訊播放、輸入事件處理及檔案傳送等功能。

此處僅列出核心檔案，更詳細的架構說明可參考 `doc/develop.md`。
