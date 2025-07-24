package video

import (
	"fmt"
	"github.com/giorgisio/goav/avcodec"
	"github.com/giorgisio/goav/avutil"
)

// Decoder wraps an FFmpeg H264 decoder.
type Decoder struct {
	codecCtx *avcodec.Context
	parser   *avcodec.ParserContext
	frame    *avutil.Frame
}

// NewDecoder initializes the FFmpeg decoder for H.264 stream.
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

// Decode decodes a packet of H264 data and returns whether a frame is ready.
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
