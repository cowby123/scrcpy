# adb 目錄導覽

此處實作與 Android Debug Bridge (ADB) 溝通的功能，包括連線建立、封包解析與通道管理等。

## 主要檔案

- `adb.c`／`adb.h`：提供 ADB 相關的高階函式。
- `adb_device.*`：管理與裝置端的連線。
- `adb_parser.*`：解析來自 ADB 的資料封包。
- `adb_tunnel.*`：在 USB 及 TCP 間建立轉接通道。
