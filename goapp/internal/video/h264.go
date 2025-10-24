package video

import (
	"bytes"
	"encoding/binary"
)

// NALUType 返回 NALU 的類型
func NALUType(nalu []byte) byte {
	if len(nalu) == 0 {
		return 0
	}
	return nalu[0] & 0x1F
}

// SplitAnnexBNALUs 解析 Annex-B 格式的 H.264 資料，返回 NALU 列表
func SplitAnnexBNALUs(data []byte) [][]byte {
	var nalus [][]byte
	start := -1

	for i := 0; i+3 < len(data); {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 { // 0x000001
				if start >= 0 {
					nalus = append(nalus, data[start:i])
				}
				start = i + 3
				i += 3
			} else if data[i+2] == 0 && i+3 < len(data) && data[i+3] == 1 { // 0x00000001
				if start >= 0 {
					nalus = append(nalus, data[start:i])
				}
				start = i + 4
				i += 4
			} else {
				i++
			}
		} else {
			i++
		}
	}

	if start >= 0 && start < len(data) {
		nalus = append(nalus, data[start:])
	}

	return nalus
}

// ParseH264SPSDimensions 從 SPS NALU 中解析視訊解析度
func ParseH264SPSDimensions(sps []byte) (w, h uint16, ok bool) {
	if len(sps) < 8 || NALUType(sps) != 7 {
		return 0, 0, false
	}

	// 簡化版：假設 SPS 後面跟著 profile/level，然後是解析度資訊
	// 這裡使用簡單的啟發式方法，實際解析需要完整的 SPS 解碼器

	// 跳過 NALU header 和 profile/level (約 4-6 bytes)
	if len(sps) < 20 {
		return 0, 0, false
	}

	// 尋找可能的寬高資訊（通常在 SPS 的後半部分）
	// 這是簡化實現，實際需要完整的 Exp-Golomb 解碼
	offset := 4
	if offset+8 > len(sps) {
		return 0, 0, false
	}

	// 嘗試讀取可能的解析度值
	// 注意：這是簡化版本，可能不適用所有 SPS 格式
	for offset+4 < len(sps) {
		val := binary.BigEndian.Uint32(sps[offset:])
		// 常見的手機解析度範圍：480-2160
		possibleW := uint16(val >> 16)
		possibleH := uint16(val & 0xFFFF)

		if possibleW >= 320 && possibleW <= 4096 && possibleH >= 240 && possibleH <= 4096 {
			// 檢查是否為常見的寬高比
			ratio := float64(possibleW) / float64(possibleH)
			if ratio > 0.5 && ratio < 3.0 {
				return possibleW, possibleH, true
			}
		}
		offset++
	}

	return 0, 0, false
}

// HasIDR 檢查 NALU 列表中是否包含 IDR 幀
func HasIDR(nalus [][]byte) bool {
	for _, nalu := range nalus {
		if NALUType(nalu) == 5 { // IDR
			return true
		}
	}
	return false
}

// FilterByType 過濾指定類型的 NALU
func FilterByType(nalus [][]byte, naluType byte) [][]byte {
	var result [][]byte
	for _, nalu := range nalus {
		if NALUType(nalu) == naluType {
			result = append(result, nalu)
		}
	}
	return result
}

// CountByType 統計各類型 NALU 的數量
func CountByType(nalus [][]byte) (sps, pps, idr, others int) {
	for _, nalu := range nalus {
		switch NALUType(nalu) {
		case 7: // SPS
			sps++
		case 8: // PPS
			pps++
		case 5: // IDR
			idr++
		default:
			others++
		}
	}
	return
}

// EqualNALU 比較兩個 NALU 是否相同
func EqualNALU(a, b []byte) bool {
	return bytes.Equal(a, b)
}
