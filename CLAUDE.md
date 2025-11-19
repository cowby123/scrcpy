# Claude 助手使用说明

## 语言设定
**请使用繁體中文与我交互。** 所有的对话、代码注释、文档说明等都将使用繁体中文进行。

---

## 開發限制

**重要**：在進行 scrcpy 客戶端（C 語言）開發時，**請勿參考 `goapp/` 目錄**的實現。

- `goapp/` 是獨立的 Go 語言示例應用，與主要的 C 客戶端開發方向不同
- 該目錄僅作為參考示例，不應作為 C 客戶端功能實現的依據
- 進行功能開發時，應專注於 `app/src/` 目錄下的 C 代碼架構

---

## scrcpy 专案概览

### 专案简介
**scrcpy** 是一个开源的 Android 设备屏幕镜像和控制工具（当前版本：v3.3.3）

**核心特点：**
- 通过 USB 或 TCP/IP 镜像 Android 设备的视频和音频
- 无需 root 权限或在设备上安装应用
- 跨平台支持：Linux、Windows、macOS
- 高性能（30-120 fps）、低延迟（35-70ms）
- 支持 1080p+ 分辨率

**主要功能：**
- 音频转发（Android 11+）
- 视频录制
- 虚拟显示
- 双向复制粘贴
- 摄像头镜像（Android 12+）
- V4L2 网络摄像头模拟（仅 Linux）
- 物理键盘和鼠标模拟（HID）
- 游戏手柄支持
- OTG 模式

---

## 专案架构

### 目录结构

```
scrcpy/
├── app/                    # 桌面客户端（C 语言）
│   ├── src/                # 主要源代码（158个C文件）
│   │   ├── adb/            # ADB 连接管理
│   │   ├── hid/            # HID 设备处理
│   │   ├── usb/            # USB 和 OTG 模式
│   │   ├── util/           # 工具库（30个文件）
│   │   ├── main.c          # 程序入口
│   │   ├── scrcpy.c/h      # 核心逻辑 (~900行)
│   │   ├── server.c/h      # 服务器通信 (~1000行)
│   │   ├── controller.c/h  # 控制消息发送
│   │   ├── decoder.c/h     # 视频/音频解码
│   │   ├── display.c/h     # 显示渲染 (~400行)
│   │   ├── screen.c/h      # 屏幕管理 (~900行)
│   │   └── ...
│   └── tests/              # 单元测试（11个文件）
│
├── server/                 # Android 服务器端（Java）
│   └── src/main/java/com/genymobile/scrcpy/
│       ├── audio/          # 音频捕获与编码（11个文件）
│       ├── video/          # 视频捕获与编码（13个文件）
│       ├── control/        # 控制命令处理（13个文件）
│       ├── device/         # 设备操作（6个文件）
│       ├── Server.java     # 主服务器
│       └── ...
│
├── goapp/                  # Go 客户端示例
│   ├── main.go             # Go 示例应用（~40KB）
│   ├── adb/                # ADB 相关包
│   └── protocol/           # 协议定义
│
├── doc/                    # 文档（21个文件）
│   ├── build.md            # 构建说明
│   ├── develop.md          # 开发文档
│   ├── connection.md       # 连接配置
│   ├── video.md            # 视频选项
│   ├── audio.md            # 音频转发
│   ├── control.md          # 控制选项
│   └── ...
│
├── release/                # 发布脚本
│   ├── build_linux.sh
│   ├── build_windows.sh
│   └── ...
│
├── meson.build             # Meson 构建配置
├── build.gradle            # Gradle 构建配置
└── README.md               # 项目说明
```

---

## 技术栈

### 桌面客户端（app/）
- **语言**: C（C11 标准）
- **构建系统**: Meson
- **主要依赖**:
  - FFmpeg（libavformat/libavcodec/libavutil/libswresample）- 音视频编解码
  - SDL2 (>= 2.0.5) - 图形窗口和输入
  - libusb-1.0 - USB 支持（可选）
  - libavdevice - V4L2 支持（Linux 可选）

### Android 服务器（server/）
- **语言**: Java
- **构建系统**: Gradle + Android Gradle Plugin
- **目标 SDK**: Android 35（API 35）
- **最小 SDK**: Android 21（API 21）
- **编译工具**: Android Build Tools 8.7.1

### Go 示例应用（goapp/）
- **语言**: Go 1.23+
- **主要依赖**: Pion WebRTC (v4.1.3)

---

## 核心模块说明

### 客户端核心文件（app/src/）

| 文件 | 功能 | 重要程度 |
|-----|------|---------|
| `main.c` | 程序入口，解析命令行参数 | ⭐⭐⭐ |
| `scrcpy.c/h` | 核心业务逻辑，初始化和主循环 | ⭐⭐⭐⭐⭐ |
| `server.c/h` | 与 Android 服务器通信 | ⭐⭐⭐⭐⭐ |
| `controller.c/h` | 发送控制消息 | ⭐⭐⭐⭐ |
| `decoder.c/h` | 视频解码 | ⭐⭐⭐⭐ |
| `display.c/h` | 窗口和显示管理 | ⭐⭐⭐⭐ |
| `screen.c/h` | 屏幕渲染 | ⭐⭐⭐⭐ |
| `recorder.c/h` | 视频/音频录制 | ⭐⭐⭐ |
| `audio_player.c/h` | 音频播放 | ⭐⭐⭐ |
| `input_manager.c/h` | 输入事件处理 | ⭐⭐⭐⭐ |
| `cli.c/h` | 命令行解析（~3000行） | ⭐⭐⭐⭐ |
| `adb/adb.c` | ADB 连接和设备管理 | ⭐⭐⭐⭐ |
| `hid/hid_*.c` | HID 事件生成 | ⭐⭐⭐ |
| `util/*.c` | 工具库（字符串、网络、日志等） | ⭐⭐⭐ |

### 服务器端核心模块（server/src/main/java/）

| 模块 | 功能 | 文件数 |
|-----|------|-------|
| `audio/` | 音频捕获、编码、格式处理 | 11 |
| `video/` | 屏幕/摄像头捕获、编码、虚拟显示 | 13 |
| `control/` | 控制命令解析、键盘/鼠标/游戏手柄处理 | 13 |
| `device/` | 设备信息获取、设备消息 | 6 |
| `opengl/` | OpenGL 渲染支持 | 3 |
| `wrappers/` | Android API 包装类 | 3 |
| `util/` | 工具和辅助函数 | 12 |
| 核心文件 | Server.java、Options.java、Workarounds.java 等 | - |

---

## 数据流架构

```
Android 设备端                     桌面客户端
┌──────────────────┐          ┌──────────────────┐
│   ScreenCapture  │          │   Display        │
│   (H.264/H.265)  │ ──────→  │   (SDL2 显示)    │
│                  │          │                  │
│   AudioCapture   │ ──────→  │   AudioPlayer    │
│   (AAC/Opus)     │          │   (FFmpeg 解码)  │
│                  │          │                  │
│   Controller     │ ←────── │   InputManager   │
│   (处理输入)     │          │   (键盘/鼠标)    │
└──────────────────┘          └──────────────────┘
        ↑↓                           ↑↓
    ADB Tunnel / TCP/IP 连接
```

---

## 构建系统

### Meson 构建选项
```bash
--compile_app=true/false           # 构建客户端
--compile_server=true/false        # 构建服务器
--portable=true/false              # 便携模式
--static=true/false                # 静态链接
--v4l2=true/false                  # V4L2 支持（Linux）
--usb=true/false                   # USB/OTG 支持
--prefix=/usr/local                # 安装前缀
```

### Windows (MSYS2) 環境編譯指令

#### 快速重新編譯（已經設置過 build 目錄）
```bash
cd /c/Users/test/Documents/GitHub/scrcpy/build
meson compile
```

#### 從頭完整編譯（首次或需要重新設置）
```bash
cd /c/Users/test/Documents/GitHub/scrcpy
meson setup build --buildtype=release
cd build
meson compile
```

#### 執行 scrcpy WebRTC 模式
```bash
# 設置環境變量（確保能找到 ADB 和依賴庫）
export PATH="/c/Users/test/Downloads/platform-tools:/c/msys64/mingw64/bin:/c/msys64/usr/bin:$PATH"

# 進入編譯輸出目錄
cd c:/Users/test/Documents/GitHub/scrcpy/build/app

# 啟動 WebRTC 串流模式
./scrcpy.exe -s 192.168.66.100:5555 --webrtc --webrtc-port=9100 --no-audio
```

#### 啟動參數說明
- `-s 192.168.66.100:5555` - 指定 TCP/IP 連接的 Android 設備 IP 和端口
- `--webrtc` - 啟用 WebRTC 模式（透過瀏覽器訪問）
- `--webrtc-port=9100` - HTTP 信令服務器端口（瀏覽器訪問 `http://localhost:9100`）
- `--no-audio` - 禁用音頻（可選，減少資源消耗）

#### 測試模式（藍色測試畫面）
```bash
# 設置環境變量啟用測試模式
export SCRCPY_WEBRTC_TEST=1

# 執行（無需連接 Android 設備）
./scrcpy.exe --webrtc --webrtc-port=9100
```

### 其他平台構建命令
```bash
# 標準構建（Linux/macOS）
meson setup builddir
ninja -C builddir

# Windows 交叉編譯
meson setup builddir --cross-file=cross_win64.txt
ninja -C builddir

# Android 服務器構建
cd server
./gradlew assembleRelease
```

---

## 常用命令示例

```bash
# 基本使用
scrcpy

# H.265 编码，限制尺寸和帧率
scrcpy --video-codec=h265 -m1920 --max-fps=60

# TCP/IP 无线连接
scrcpy --tcpip=192.168.1.100

# 虚拟显示（Android 13+）
scrcpy --new-display=1920x1080

# 摄像头镜像
scrcpy --video-source=camera --camera-size=1920x1080

# 录制屏幕
scrcpy --record=file.mp4

# 物理键盘模拟（HID）
scrcpy -K

# OTG 模式（仅输入控制）
scrcpy --otg

# 游戏手柄支持
scrcpy -G
```

---

## 重要文档参考

| 文档 | 用途 |
|-----|------|
| [build.md](doc/build.md) | 详细构建说明 |
| [develop.md](doc/develop.md) | 开发者指南、架构说明 |
| [connection.md](doc/connection.md) | USB/TCP/IP 连接配置 |
| [video.md](doc/video.md) | 视频选项（码率、尺寸、帧率） |
| [audio.md](doc/audio.md) | 音频转发和同步 |
| [control.md](doc/control.md) | 键盘、鼠标、触摸操作 |
| [camera.md](doc/camera.md) | 摄像头镜像功能 |
| [shortcuts.md](doc/shortcuts.md) | 快捷键列表 |
| [FAQ.md](FAQ.md) | 常见问题解答 |

---

## 开发注意事项

### 代码风格
- **C 代码**: 遵循项目现有风格，4 空格缩进
- **Java 代码**: 使用 Checkstyle 检查（见 `config/checkstyle/`）
- **命名规范**: 小写蛇形命名（C）、驼峰命名（Java）

### 调试技巧
```bash
# 启用服务器调试
meson configure builddir -Dserver_debugger=true

# 查看日志
scrcpy --verbosity=debug

# 本地运行脚本
./run
```

### 测试
```bash
# C 单元测试
ninja -C builddir test

# 完整测试脚本
./release/test_client.sh
./release/test_server.sh
```

---

## 项目统计

- **总源文件**: ~264 个
- **C 源文件**: ~158 个
- **Java 文件**: ~85 个
- **总提交数**: 2939+
- **许可证**: Apache License 2.0
- **维护者**: Genymobile / Romain Vimont (@rom1v)

---

## 常见任务快速参考

### 修改客户端逻辑
1. 找到相关 C 源文件（通常在 `app/src/`）
2. 修改代码
3. 重新构建：`ninja -C builddir`
4. 测试：`./builddir/app/scrcpy`

### 修改服务器逻辑
1. 找到相关 Java 源文件（`server/src/main/java/`）
2. 修改代码
3. 重新构建：`cd server && ./gradlew assembleRelease`
4. 测试：`./run`

### 添加新功能
1. 阅读 [develop.md](doc/develop.md)
2. 确定客户端/服务器端修改点
3. 更新通信协议（如需要）
4. 实现、测试、提交

### 调试问题
1. 启用详细日志：`scrcpy --verbosity=debug`
2. 检查 ADB 连接：`adb devices`
3. 查看服务器日志：`adb logcat`
4. 使用 GDB/LLDB 调试客户端

---

## 协助方向

我可以帮助您：
- 理解和修改 C/Java 代码
- 调试构建和运行时问题
- 添加新功能或优化现有功能
- 分析性能问题
- 编写测试代码
- 解释架构和数据流
- 跨平台兼容性问题
- 文档编写和翻译

有任何问题，随时用**中文**向我提问！
