package adb

import (
	"fmt"
	"io"

	"github.com/zach-klippenstein/goadb"
)

// Device wraps an adb Device and provides helper methods
// to start the scrcpy server on the Android device.
type Device struct {
	adb    *adb.Adb
	device *adb.Device
}

// NewDevice connects to adb and returns a Device for the given serial.
func NewDevice(serial string) (*Device, error) {
	a, err := adb.NewWithConfig(adb.ServerConfig{PathToAdb: "adb"})
	if err != nil {
		return nil, fmt.Errorf("create adb client: %w", err)
	}
	d := a.Device(adb.DeviceWithSerial(serial))
	return &Device{adb: a, device: d}, nil
}

// PushServer pushes scrcpy-server.jar to the device temporary directory.
func (d *Device) PushServer(localPath string) error {
	remotePath := "/data/local/tmp/scrcpy-server.jar"
	return d.device.PushFile(localPath, remotePath)
}

// StartServer starts the scrcpy server using adb shell.
func (d *Device) StartServer() (io.ReadCloser, error) {
	// The server outputs the video stream on stdout, so we use StartShell.
	// The command is simplified for demonstration purposes.
	cmd := "CLASSPATH=/data/local/tmp/scrcpy-server.jar app_process / com.genymobile.scrcpy.Server 1.25"
	return d.device.RunCommandWithByteOutput(cmd)
}

// Forward establishes a port forward from local to the scrcpy tunnel.
func (d *Device) Forward(local string) error {
	return d.adb.Forward(local, "localabstract:scrcpy")
}
