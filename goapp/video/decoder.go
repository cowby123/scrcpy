// 利用 FFmpeg 將收到的 H264 資料解碼
package video

import (
	"fmt"
	"github.com/giorgisio/goav/avcodec"
	"github.com/giorgisio/goav/avutil"
)

// Decoder 包裝 FFmpeg 的 H264 解碼器
type Decoder struct {
	codecCtx *avcodec.Context       // 解碼器上下文
	parser   *avcodec.ParserContext // 用來解析 H264 串流
	frame    *avutil.Frame          // 暫存解碼後的影格
}

// NewDecoder 初始化 H264 解碼器
func NewDecoder() (*Decoder, error) {
	codec := avcodec.AvcodecFindDecoder(avcodec.AV_CODEC_ID_H264)
	if codec == nil {
		return nil, fmt.Errorf("H264 decoder not found")
	}
	c := codec.AvcodecAllocContext3()
	if c.AvcodecOpen2(codec, nil) < 0 {
		return nil, fmt.Errorf("could not open codec")
	}
	parser := avcodec.AvParserInit(int(avcodec.AV_CODEC_ID_H264))
	if parser == nil {
		return nil, fmt.Errorf("parser init failed")
	}
	return &Decoder{codecCtx: c, parser: parser, frame: avutil.AvFrameAlloc()}, nil
}

// Decode 解碼一個 H264 資料包，若成功取得影格則回傳
func (d *Decoder) Decode(data []byte) (*avutil.Frame, bool, error) {
	var gotFrame int
	avPacket := avcodec.AvPacketAlloc()
	avPacket.AvInitPacket()
	avPacket.SetData(data)
	avPacket.SetSize(len(data))
	ret := avcodec.AvcodecSendPacket(d.codecCtx, avPacket)
	if ret < 0 {
		return nil, false, fmt.Errorf("send packet failed")
	}
	ret = avcodec.AvcodecReceiveFrame(d.codecCtx, d.frame)
	if ret == 0 {
		gotFrame = 1
	}
	if gotFrame != 0 {
		return d.frame, true, nil
	}
	return nil, false, nil
}
