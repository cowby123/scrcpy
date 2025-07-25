// 封裝對 adb 的操作，協助在裝置上啟動 scrcpy 伺服器
package adb

import (
        "fmt"
        "io"
        "os/exec"

        "github.com/zach-klippenstein/goadb"
)

// Device 代表一台 Android 裝置
type Device struct {
	adb    *adb.Adb    // adb 客戶端
	device *adb.Device // 特定序號的裝置
}

// NewDevice 連線至 adb，並回傳指定序號的 Device
func NewDevice(serial string) (*Device, error) {
	a, err := adb.NewWithConfig(adb.ServerConfig{PathToAdb: "adb"})
	if err != nil {
		return nil, fmt.Errorf("create adb client: %w", err)
	}
	d := a.Device(adb.DeviceWithSerial(serial))
	return &Device{adb: a, device: d}, nil
}

// PushServer 將 scrcpy-server.jar 推送到裝置的暫存目錄
func (d *Device) PushServer(localPath string) error {
        remotePath := "/data/local/tmp/scrcpy-server.jar"
        // goadb.Device does not provide a PushFile helper in the current
        // dependency version, so fallback to invoking `adb push` directly.
        cmd := exec.Command("adb", "push", localPath, remotePath)
        if out, err := cmd.CombinedOutput(); err != nil {
                return fmt.Errorf("push server: %w (%s)", err, string(out))
        }
        return nil
}

// StartServer 透過 adb shell 啟動 scrcpy 伺服器並回傳串流
func (d *Device) StartServer() (io.ReadCloser, error) {
	// The server outputs the video stream on stdout, so we use StartShell.
	// The command is simplified for demonstration purposes.
	cmd := "CLASSPATH=/data/local/tmp/scrcpy-server.jar app_process / com.genymobile.scrcpy.Server 1.25"
	return d.device.RunCommandWithByteOutput(cmd)
}

// Forward 在本地建立與 scrcpy 通道的連線轉發
func (d *Device) Forward(local string) error {
	return d.adb.Forward(local, "localabstract:scrcpy")
}
