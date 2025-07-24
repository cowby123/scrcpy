// 利用 SDL2 將解碼後的影像顯示在視窗中
package video

import (
	"github.com/veandco/go-sdl2/sdl"
)

// Display 使用 SDL2 顯示從影片串流解碼出的影格
type Display struct {
	window   *sdl.Window
	renderer *sdl.Renderer
	texture  *sdl.Texture
	width    int
	height   int
}

// NewDisplay 建立 SDL 視窗與繪圖器
func NewDisplay(title string, w, h int) (*Display, error) {
	if err := sdl.Init(sdl.INIT_VIDEO); err != nil {
		return nil, err
	}
	win, err := sdl.CreateWindow(title, sdl.WINDOWPOS_UNDEFINED, sdl.WINDOWPOS_UNDEFINED,
		int32(w), int32(h), sdl.WINDOW_SHOWN)
	if err != nil {
		return nil, err
	}
	rend, err := sdl.CreateRenderer(win, -1, sdl.RENDERER_ACCELERATED)
	if err != nil {
		return nil, err
	}
	tex, err := rend.CreateTexture(sdl.PIXELFORMAT_IYUV, sdl.TEXTUREACCESS_STREAMING, int32(w), int32(h))
	if err != nil {
		return nil, err
	}
	return &Display{window: win, renderer: rend, texture: tex, width: w, height: h}, nil
}

// Render 將 YUV 影格寫入紋理並更新畫面
func (d *Display) Render(yuv []byte) error {
	d.texture.Update(nil, yuv, d.width)
	d.renderer.Copy(d.texture, nil, nil)
	d.renderer.Present()
	return nil
}

// Poll 處理事件以維持視窗運作
func (d *Display) Poll() bool {
	for event := sdl.PollEvent(); event != nil; event = sdl.PollEvent() {
		switch event.(type) {
		case *sdl.QuitEvent:
			return false
		}
	}
	sdl.Delay(10)
	return true
}

// Close 釋放 SDL 資源
func (d *Display) Close() {
	d.texture.Destroy()
	d.renderer.Destroy()
	d.window.Destroy()
	sdl.Quit()
}
