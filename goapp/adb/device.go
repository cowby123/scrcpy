// Package adb wraps the minimal subset of adb interactions required to
// bootstrap the scrcpy server and channel the resulting connections.
package adb

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DefaultScrcpyPort is the TCP port used by scrcpy for both video and control
// channels. The Android server connects twice to this port: the first
// connection carries the H.264 stream, the second is the control socket for
// input events.
const DefaultScrcpyPort = 27183

// Options configure how adb is invoked and which local TCP port scrcpy should
// reach when it connects back to the host.
type Options struct {
	// Serial is the adb device identifier (e.g. from `adb devices` or `adb connect`).
	Serial string
	// ServerHost and ServerPort point to a specific adb server instance.
	// Leave empty/zero to use adb's defaults (local server on 127.0.0.1:5037).
	ServerHost string
	ServerPort int
	// ScrcpyPort is the local TCP port that the scrcpy server connects back to.
	// Set to 0 to use DefaultScrcpyPort.
	ScrcpyPort int
}

// Device encapsulates adb interactions with a specific target.
type Device struct {
	opts Options
}

type cmdReadCloser struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReadCloser) Close() error {
	err1 := c.ReadCloser.Close()
	err2 := c.cmd.Wait()
	if err1 != nil {
		return err1
	}
	return err2
}

func normalizeOptions(opts Options) Options {
	if opts.ScrcpyPort == 0 {
		opts.ScrcpyPort = DefaultScrcpyPort
	}
	return opts
}

func buildADBArgs(opts Options, includeSerial bool, extra ...string) []string {
	args := make([]string, 0, 4+len(extra))
	if opts.ServerHost != "" {
		args = append(args, "-H", opts.ServerHost)
	}
	if opts.ServerPort != 0 {
		args = append(args, "-P", strconv.Itoa(opts.ServerPort))
	}
	if includeSerial && opts.Serial != "" {
		args = append(args, "-s", opts.Serial)
	}
	args = append(args, extra...)
	return args
}

// NewDevice ensures the adb server is reachable and returns a configured Device.
func NewDevice(opts Options) (*Device, error) {
	opts = normalizeOptions(opts)
	cmd := exec.Command("adb", buildADBArgs(opts, false, "start-server")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start adb server: %w (%s)", err, string(out))
	}
	return &Device{opts: opts}, nil
}

// PushServer uploads scrcpy-server.jar into a temporary directory on device.
func (d *Device) PushServer(localPath string) error {
	remotePath := "/data/local/tmp/scrcpy-server.jar"
	args := buildADBArgs(d.opts, true, "push", localPath, remotePath)
	cmd := exec.Command("adb", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push server: %w (%s)", err, string(out))
	}
	return nil
}

// ServerConn holds both streams created by the scrcpy server.
type ServerConn struct {
	VideoStream io.ReadWriteCloser
	Control     io.ReadWriteCloser
}

// StartServer launches scrcpy through adb shell and waits for both channels.
func (d *Device) StartServer() (*ServerConn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", d.opts.ScrcpyPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	args := buildADBArgs(d.opts, true,
		"shell",
		"CLASSPATH=/data/local/tmp/scrcpy-server.jar",
		"app_process",
		"/",
		"com.genymobile.scrcpy.Server",
		"3.3.2",
		"audio=false",
	)
	cmd := exec.Command("adb", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	go cmd.Wait()

	videoConn, err := ln.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept video stream: %w", err)
	}
	// 啟用 TCP_NODELAY 以減少小封包延遲（禁用 Nagle 演算法）
	if tcpConn, ok := videoConn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			videoConn.Close()
			return nil, fmt.Errorf("set video TCP_NODELAY: %w", err)
		}
	}

	controlConn, err := ln.Accept()
	if err != nil {
		videoConn.Close()
		return nil, fmt.Errorf("accept control channel: %w", err)
	}
	// 啟用 TCP_NODELAY 以減少控制指令延遲
	if tcpConn, ok := controlConn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			videoConn.Close()
			controlConn.Close()
			return nil, fmt.Errorf("set control TCP_NODELAY: %w", err)
		}
	}

	return &ServerConn{
		VideoStream: videoConn,
		Control:     controlConn,
	}, nil
}

// ScrcpyPort returns the effective local port used for reverse connections.
func (d *Device) ScrcpyPort() int {
	return d.opts.ScrcpyPort
}

// Forward sets up classic adb forward (not used in current flow but kept for parity).
func (d *Device) Forward(local string) error {
	args := buildADBArgs(d.opts, true, "forward", local, "localabstract:scrcpy")
	cmd := exec.Command("adb", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("forward: %w (%s)", err, string(out))
	}
	return nil
}

// Reverse asks the device to connect back to the given local endpoint.
func (d *Device) Reverse(remote, local string) error {
	args := buildADBArgs(d.opts, true, "reverse", remote, local)
	cmd := exec.Command("adb", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reverse: %w (%s)", err, string(out))
	}
	return nil
}

// ADBDevice 代表一個 ADB 設備
type ADBDevice struct {
	Serial string // 設備序號或 IP:port
	State  string // device, offline, unauthorized 等
}

// ListDevices 列出所有 ADB 可見的設備
// 用途：執行 `adb devices` 並解析輸出
func ListDevices(opts Options) ([]ADBDevice, error) {
	args := buildADBArgs(opts, false, "devices")
	cmd := exec.Command("adb", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list devices: %w (%s)", err, string(out))
	}

	return parseDevicesOutput(string(out)), nil
}

// parseDevicesOutput 解析 `adb devices` 的輸出
// 格式範例：
// List of devices attached
// 192.168.66.102:5555	device
// emulator-5554	offline
func parseDevicesOutput(output string) []ADBDevice {
	devices := []ADBDevice{}

	// 分割行
	lines := strings.Split(strings.TrimSpace(output), "\n")

	for i, line := range lines {
		// 跳過第一行標題 "List of devices attached"
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}

		// 格式: <serial>\t<state>
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			devices = append(devices, ADBDevice{
				Serial: parts[0],
				State:  parts[1],
			})
		}
	}

	return devices
}
