package video

import (
    "fmt"

    "github.com/3d0c/gmf"
)

// Decoder uses FFmpeg via gmf to decode H.264 frames sent from scrcpy.
type Decoder struct {
    ctx *gmf.CodecCtx
}

// NewDecoder creates and configures a new H.264 decoder.
func NewDecoder() (*Decoder, error) {
    codec, err := gmf.FindDecoder(gmf.AV_CODEC_ID_H264)
    if err != nil {
        return nil, fmt.Errorf("find decoder: %w", err)
    }
    ctx := gmf.NewCodecCtx(codec)
    if ctx == nil {
        return nil, fmt.Errorf("new codec context")
    }
    if err := ctx.Open(nil); err != nil {
        ctx.Free()
        return nil, fmt.Errorf("open codec: %w", err)
    }
    return &Decoder{ctx: ctx}, nil
}

// Close releases the decoder resources.
func (d *Decoder) Close() {
    if d.ctx != nil {
        d.ctx.Free()
    }
}

// Decode attempts to decode a single video packet.
// It returns a frame when a complete image is available.
func (d *Decoder) Decode(data []byte) (*gmf.Frame, bool, error) {
    pkt := gmf.NewPacket()
    if err := pkt.SetData(data); err != nil {
        return nil, false, fmt.Errorf("set packet data: %w", err)
    }
    pkt.SetSize(len(data))
    defer pkt.Free()

    frames, err := d.ctx.Decode(pkt)
    if err != nil {
        return nil, false, fmt.Errorf("decode: %w", err)
    }
    if len(frames) == 0 {
        return nil, false, nil
    }
    frame := frames[0]
    for _, f := range frames[1:] {
        f.Free()
    }
    return frame, true, nil
}
