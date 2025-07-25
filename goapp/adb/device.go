// 封裝對 adb 的操作，協助在裝置上啟動 scrcpy 伺服器
package adb

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// Device 代表一台 Android 裝置
type Device struct {
	serial string
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

// NewDevice 連線至 adb，並回傳指定序號的 Device
func NewDevice(serial string) (*Device, error) {
	cmd := exec.Command("adb", "start-server")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("start adb server: %w (%s)", err, string(out))
	}
	return &Device{serial: serial}, nil
}

// PushServer 將 scrcpy-server.jar 推送到裝置的暫存目錄
func (d *Device) PushServer(localPath string) error {
	remotePath := "/data/local/tmp/scrcpy-server.jar"
	args := []string{}
	if d.serial != "" {
		args = append(args, "-s", d.serial)
	}
	args = append(args, "push", localPath, remotePath)
	cmd := exec.Command("adb", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("push server: %w (%s)", err, string(out))
	}
	return nil
}

// StartServer 透過 adb shell 啟動 scrcpy 伺服器並回傳串流
func (d *Device) StartServer() (io.ReadCloser, error) {
	args := []string{}
	if d.serial != "" {
		args = append(args, "-s", d.serial)
	}
	args = append(args, "shell", "CLASSPATH=/data/local/tmp/scrcpy-server.jar", "app_process", "/", "com.genymobile.scrcpy.Server", "3.3.1")
	cmd := exec.Command("adb", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("start server pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	return &cmdReadCloser{ReadCloser: stdout, cmd: cmd}, nil
}

// Forward 在本地建立與 scrcpy 通道的連線轉發
func (d *Device) Forward(local string) error {
	args := []string{}
	if d.serial != "" {
		args = append(args, "-s", d.serial)
	}
	args = append(args, "forward", local, "localabstract:scrcpy")
	cmd := exec.Command("adb", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("forward: %w (%s)", err, string(out))
	}
	return nil
}

// Reverse 在裝置端建立連線，使其回連至本機指定的埠號
func (d *Device) Reverse(remote, local string) error {
	args := []string{}
	if d.serial != "" {
		args = append(args, "-s", d.serial)
	}
	args = append(args, "reverse", remote, local)
	cmd := exec.Command("adb", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reverse: %w (%s)", err, string(out))
	}
	return nil
}
