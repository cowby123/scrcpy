package utils

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"runtime/debug"
)

// GenerateSessionID 生成唯一的 session ID（32字符十六進制）
func GenerateSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Printf("[UUID] 生成失敗，使用時間戳: %v", err)
		return ""
	}
	return hex.EncodeToString(b)
}

// GoSafe 安全啟動 goroutine，捕獲 panic
func GoSafe(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC][%s] %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// TrimString 截斷字串到指定長度並添加省略號
func TrimString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
