package main

// === Annex-B 工具 ===
// splitAnnexBNALUs 將 Annex-B 格式的位元流分割為獨立的 NALU 單元
func splitAnnexBNALUs(b []byte) [][]byte {
	var nalus [][]byte
	i := 0
	for {
		scStart, scEnd := findStartCode(b, i)
		if scStart < 0 {
			break
		}
		nextStart, _ := findStartCode(b, scEnd)
		if nextStart < 0 {
			n := b[scEnd:]
			if len(n) > 0 {
				nalus = append(nalus, n)
			}
			break
		}
		n := b[scEnd:nextStart]
		if len(n) > 0 {
			nalus = append(nalus, n)
		}
		i = nextStart
	}
	return nalus
}

// findStartCode 在位元組陣列中尋找 H.264 起始碼
func findStartCode(b []byte, from int) (int, int) {
	for i := from; i+3 <= len(b); i++ {
		// 00 00 01
		if i+3 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return i, i + 3
		}
		// 00 00 00 01
		if i+4 <= len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return i, i + 4
		}
	}
	return -1, -1
}

// naluType 取得 NALU 的類型
func naluType(n []byte) uint8 {
	if len(n) == 0 {
		return 0
	}
	return n[0] & 0x1F
}

// === PTS → RTP TS 轉換 ===
// rtpTSFromPTS 將 PTS（呈現時間戳）轉換為 RTP 時間戳
func rtpTSFromPTS(pts, base uint64) uint32 {
	delta := pts - base
	return uint32((delta * 90000) / ptsPerSecond) // 90kHz * 秒數
}

// === H.264 SPS 解析寬高（極簡）===
// bitReader 位元流讀取器結構體
type bitReader struct {
	b []byte
	i int // bit index
}

// parseH264SPSDimensions 解析 H.264 SPS 中的視訊尺寸資訊
func parseH264SPSDimensions(nal []byte) (w, h uint16, ok bool) {
	if len(nal) < 4 || (nal[0]&0x1F) != 7 {
		return
	}
	// 去除 emulation prevention bytes
	rbsp := make([]byte, 0, len(nal)-1)
	for i := 1; i < len(nal); i++ {
		if i+2 < len(nal) && nal[i] == 0 && nal[i+1] == 0 && nal[i+2] == 3 {
			rbsp = append(rbsp, 0, 0)
			i += 2
			continue
		}
		rbsp = append(rbsp, nal[i])
	}
	br := bitReader{b: rbsp}

	// profile_idc, constraint_flags, level_idc
	if !br.skip(8 + 8 + 8) {
		return
	}
	// seq_parameter_set_id
	if _, ok2 := br.ue(); !ok2 {
		return
	}

	var chromaFormatIDC uint = 1
	profileIDC := rbsp[0]
	if profileIDC == 100 || profileIDC == 110 || profileIDC == 122 ||
		profileIDC == 244 || profileIDC == 44 || profileIDC == 83 ||
		profileIDC == 86 || profileIDC == 118 || profileIDC == 128 ||
		profileIDC == 138 || profileIDC == 139 || profileIDC == 134 {
		if v, ok2 := br.ue(); !ok2 {
			return
		} else {
			chromaFormatIDC = v
		}
		if chromaFormatIDC == 3 {
			if _, ok2 := br.u(1); !ok2 {
				return
			}
		}
		if _, ok2 := br.ue(); !ok2 {
			return
		}
		if _, ok2 := br.ue(); !ok2 {
			return
		}
		if !br.skip(1) {
			return
		}
		// seq_scaling_matrix_present_flag
		if f, ok2 := br.u(1); !ok2 {
			return
		} else if f == 1 {
			n := 8
			if chromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if g, ok3 := br.u(1); !ok3 {
					return
				} else if g == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					lastScale := 8
					nextScale := 8
					for j := 0; j < size; j++ {
						if nextScale != 0 {
							delta, ok4 := br.se()
							if !ok4 {
								return
							}
							nextScale = (lastScale + int(delta) + 256) % 256
						}
						if nextScale != 0 {
							lastScale = nextScale
						}
					}
				}
			}
		}
	}

	// log2_max_frame_num_minus4
	if _, ok2 := br.ue(); !ok2 {
		return
	}
	// pic_order_cnt_type
	pct, ok2 := br.ue()
	if !ok2 {
		return
	}
	if pct == 0 {
		if _, ok2 = br.ue(); !ok2 {
			return
		}
	} else if pct == 1 {
		if !br.skip(1) {
			return
		}
		if _, ok2 = br.se(); !ok2 {
			return
		}
		if _, ok2 = br.se(); !ok2 {
			return
		}
		var n uint
		if n, ok2 = br.ue(); !ok2 {
			return
		}
		for i := uint(0); i < n; i++ {
			if _, ok2 = br.se(); !ok2 {
				return
			}
		}
	}

	// num_ref_frames, gaps_in_frame_num_value_allowed_flag
	if _, ok2 = br.ue(); !ok2 {
		return
	}
	if !br.skip(1) {
		return
	}

	// 寬高
	pwMinus1, ok2 := br.ue()
	if !ok2 {
		return
	}
	phMinus1, ok2 := br.ue()
	if !ok2 {
		return
	}
	frameMbsOnlyFlag, ok2 := br.u(1)
	if !ok2 {
		return
	}
	if frameMbsOnlyFlag == 0 {
		if !br.skip(1) {
			return
		}
	}
	if !br.skip(1) {
		return
	}

	// cropping
	cropLeft, cropRight, cropTop, cropBottom := uint(0), uint(0), uint(0), uint(0)
	fcrop, ok2 := br.u(1)
	if !ok2 {
		return
	}
	if fcrop == 1 {
		if cropLeft, ok2 = br.ue(); !ok2 {
			return
		}
		if cropRight, ok2 = br.ue(); !ok2 {
			return
		}
		if cropTop, ok2 = br.ue(); !ok2 {
			return
		}
		if cropBottom, ok2 = br.ue(); !ok2 {
			return
		}
	}

	mbWidth := (pwMinus1 + 1)
	mbHeight := (phMinus1 + 1) * (2 - frameMbsOnlyFlag)

	var subW, subH uint = 1, 1
	switch chromaFormatIDC {
	case 0:
		subW, subH = 1, 1
	case 1:
		subW, subH = 2, 2
	case 2:
		subW, subH = 2, 1
	case 3:
		subW, subH = 1, 1
	}
	cropUnitX := subW
	cropUnitY := subH * (2 - frameMbsOnlyFlag)

	width := int(mbWidth*16) - int((cropLeft+cropRight)*cropUnitX)
	height := int(mbHeight*16) - int((cropTop+cropBottom)*cropUnitY)

	if width <= 0 || height <= 0 || width > 65535 || height > 65535 {
		return
	}
	return uint16(width), uint16(height), true
}

// --- bitReader methods ---
// u 從位元流中讀取指定位數的無符號整數
func (br *bitReader) u(n int) (uint, bool) {
	if n <= 0 {
		return 0, true
	}
	var v uint
	for k := 0; k < n; k++ {
		byteIndex := br.i / 8
		if byteIndex >= len(br.b) {
			return 0, false
		}
		bitIndex := 7 - (br.i % 8)
		bit := (br.b[byteIndex] >> uint(bitIndex)) & 1
		v = (v << 1) | uint(bit)
		br.i++
	}
	return v, true
}

// skip 跳過位元流中指定位數的資料
func (br *bitReader) skip(n int) bool { _, ok := br.u(n); return ok }

// ue 讀取 Exp-Golomb 無符號編碼值
func (br *bitReader) ue() (uint, bool) {
	var leadingZeros int
	for {
		b, ok := br.u(1)
		if !ok {
			return 0, false
		}
		if b == 0 {
			leadingZeros++
		} else {
			break
		}
	}
	if leadingZeros == 0 {
		return 0, true
	}
	val, ok := br.u(leadingZeros)
	if !ok {
		return 0, false
	}
	return (1 << leadingZeros) - 1 + val, true
}

// se 讀取 Exp-Golomb 有符號編碼值
func (br *bitReader) se() (int, bool) {
	uev, ok := br.ue()
	if !ok {
		return 0, false
	}
	k := int(uev)
	if k%2 == 0 {
		return -k / 2, true
	}
	return (k + 1) / 2, true
}
