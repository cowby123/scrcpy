package main

import (
	"log"

	"github.com/yourname/scrcpy-go/adb"
	"github.com/yourname/scrcpy-go/input"
	"github.com/yourname/scrcpy-go/protocol"
	"github.com/yourname/scrcpy-go/video"
)

const serverJar = "../server/scrcpy-server.jar"

func main() {
	dev, err := adb.NewDevice("")
	if err != nil {
		log.Fatal(err)
	}

	if err := dev.PushServer(serverJar); err != nil {
		log.Fatal("push server:", err)
	}

	stream, err := dev.StartServer()
	if err != nil {
		log.Fatal("start server:", err)
	}
	defer stream.Close()

	dec, err := video.NewDecoder()
	if err != nil {
		log.Fatal("init decoder:", err)
	}

	disp, err := video.NewDisplay("scrcpy-go", 720, 1280)
	if err != nil {
		log.Fatal(err)
	}
	defer disp.Close()

	for {
		pkt, err := protocol.Decode(stream)
		if err != nil {
			log.Println("read:", err)
			break
		}
		if pkt.Type == 0 { // video packet
			frame, ok, err := dec.Decode(pkt.Body)
			if err != nil {
				log.Println("decode:", err)
				continue
			}
			if ok {
				videoData := frame.Data()
				disp.Render(videoData)
			}
		}
		events := input.Capture()
		_ = events // future: encode/send using protocol encoder
		if !disp.Poll() {
			break
		}
	}
}
