# app 目錄導覽

此目錄包含 scrcpy 桌面應用程式本身的原始碼、資源與建置腳本。以下簡要說明各子目錄與檔案用途。

## 目錄

### `data`
包含應用程式在不同平台上所需的各種資源：
- `icon.ico`、`icon.png`、`icon.svg`：應用程式圖示。
- `open_a_terminal_here.bat`：在 Windows 上於當前路徑開啟命令提示字元。
- `scrcpy-console.bat`、`scrcpy-noconsole.vbs`：於 Windows 執行 scrcpy 的批次與腳本檔。
- `scrcpy-console.desktop`、`scrcpy.desktop`：Linux 桌面環境的啟動檔。
- `bash-completion/`、`zsh-completion/`：提供 Bash 與 Zsh 的指令自動補完腳本。

### `deps`
收錄建置 scrcpy 依賴元件的腳本與說明文件：
- `adb_linux.sh`、`adb_macos.sh`、`adb_windows.sh`：下載並建置各平台的 ADB。
- `dav1d.sh`、`ffmpeg.sh`、`libusb.sh`、`sdl.sh`：建置其他第三方函式庫。
- `common`：被各腳本引用的共用函式。
- `README`：說明此目錄用途與腳本操作方式。

### `src`
scrcpy 主程式的 C 原始碼，依功能劃分多個子目錄：
- `adb/`：與 Android Debug Bridge 溝通的實作。
- `android/`：Android 相關的常數與定義。
- `hid/`：產生鍵盤、滑鼠與遊戲控制器的 HID 事件。
- `sys/`：與作業系統相關的程式碼，依 `unix/` 與 `win/` 區分。
- `trait/`：定義資料來源與接收端等抽象介面。
- `usb/`：USB 與 OTG 相關的處理。
- `uhid/`：透過 uhid 裝置模擬輸入。
- `util/`：通用工具函式庫。
除此之外，根目錄還包含 `main.c`、`scrcpy.c` 等核心程式檔案。

### `tests`
以 `meson test` 執行的單元測試程式。

## 檔案

- `meson.build`：定義如何以 Meson 建置此專案。
- `scrcpy-windows.rc`：Windows 版本的資源設定檔。
- `scrcpy-windows.manifest`：Windows 執行檔的 manifest。
- `scrcpy.1`：UNIX 手冊頁 (man page)。

