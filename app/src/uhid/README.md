# uhid 目錄導覽

透過 Linux `uhid` 裝置模擬虛擬輸入，供系統將其視為一般 HID 裝置。

## 主要檔案

- `keyboard_uhid.*`、`mouse_uhid.*`、`gamepad_uhid.*`：各類虛擬輸入裝置的實作。
- `uhid_output.*`：管理 `uhid` 裝置的輸出事件。
