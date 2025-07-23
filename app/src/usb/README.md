# usb 目錄導覽

處理 USB 及 OTG 模式下的資料傳輸與 HID 模擬。

## 主要檔案

- `usb.*`：列舉與開啟 USB 裝置。
- `scrcpy_otg.*`、`screen_otg.*`：OTG 模式下的畫面與資料通道。
- `keyboard_aoa.*`、`mouse_aoa.*`、`gamepad_aoa.*`：以 AOA 傳送 HID 輸入。
- `aoa_hid.*`：管理 Android Open Accessory HID 裝置。
