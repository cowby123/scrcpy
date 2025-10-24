# scrcpy-go 快速啟動腳本
# 使用方式：
#   .\start.ps1                      # 自動選擇設備
#   .\start.ps1 ABCD1234             # 指定 USB 設備
#   .\start.ps1 192.168.1.100:5555   # 指定 WiFi 設備

param(
    [string]$Device = "",
    [int]$Port = 27183
)

Write-Host "=== scrcpy-go 啟動腳本 ===" -ForegroundColor Cyan
Write-Host ""

# 檢查 ADB 是否可用
try {
    $adbVersion = adb version 2>$null
    if ($LASTEXITCODE -ne 0) {
        throw "ADB not found"
    }
    Write-Host "✓ ADB 已安裝" -ForegroundColor Green
} catch {
    Write-Host "✗ 錯誤：找不到 ADB，請確保已安裝 Android SDK Platform Tools" -ForegroundColor Red
    Write-Host "  下載位址：https://developer.android.com/studio/releases/platform-tools" -ForegroundColor Yellow
    exit 1
}

# 列出可用設備
Write-Host ""
Write-Host "正在掃描 Android 設備..." -ForegroundColor Cyan
$devices = adb devices | Select-Object -Skip 1 | Where-Object { $_ -match '\t' }

if ($devices.Count -eq 0) {
    Write-Host "✗ 錯誤：未找到任何 Android 設備" -ForegroundColor Red
    Write-Host ""
    Write-Host "請確認：" -ForegroundColor Yellow
    Write-Host "  1. 設備已透過 USB 連接或已啟用 WiFi ADB" -ForegroundColor Yellow
    Write-Host "  2. 設備已啟用 USB 偵錯模式" -ForegroundColor Yellow
    Write-Host "  3. 已在設備上授權此電腦進行偵錯" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "WiFi 連接方式：" -ForegroundColor Cyan
    Write-Host "  adb tcpip 5555" -ForegroundColor White
    Write-Host "  adb connect <設備IP>:5555" -ForegroundColor White
    exit 1
}

Write-Host "✓ 找到 $($devices.Count) 個設備：" -ForegroundColor Green
$devices | ForEach-Object {
    $parts = $_ -split '\t'
    Write-Host "  - $($parts[0])" -ForegroundColor White
}

# 建立啟動參數
$args = @()
if ($Device -ne "") {
    Write-Host ""
    Write-Host "使用指定設備: $Device" -ForegroundColor Cyan
    $args += "-device", $Device
} else {
    Write-Host ""
    Write-Host "將自動選擇第一個可用設備" -ForegroundColor Yellow
}

if ($Port -ne 27183) {
    Write-Host "使用自訂端口: $Port" -ForegroundColor Cyan
    $args += "-scrcpy-port", $Port
}

# 啟動程式
Write-Host ""
Write-Host "正在啟動 scrcpy-go 伺服器..." -ForegroundColor Cyan
Write-Host "請在瀏覽器中開啟: http://localhost:8080" -ForegroundColor Green
Write-Host ""
Write-Host "按 Ctrl+C 停止伺服器" -ForegroundColor Yellow
Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

if ($args.Count -gt 0) {
    & go run . $args
} else {
    & go run .
}
