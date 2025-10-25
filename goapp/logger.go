package main

import (
	"io"
	"log"
	"os"
)

// ==================== 日誌級別控制 ====================
type LogLevel int

const (
	LogLevelDebug  LogLevel = iota // 0 - 顯示所有日誌
	LogLevelInfo                   // 1 - 顯示 info 和 error
	LogLevelError                  // 2 - 只顯示 error
	LogLevelSilent                 // 3 - 不顯示日誌
)

var (
	currentLogLevel = LogLevelSilent // 預設級別：靜默模式
	logOutput       = log.New(os.Stdout, "", log.LstdFlags)
)

// SetLogLevel 設定日誌級別
func SetLogLevel(level LogLevel) {
	currentLogLevel = level
	if level == LogLevelSilent {
		log.SetOutput(io.Discard)
	} else {
		log.SetOutput(os.Stdout)
	}
}

// LogDebug 調試日誌（最詳細）
func LogDebug(format string, v ...interface{}) {
	if currentLogLevel <= LogLevelDebug {
		logOutput.Printf("[DEBUG] "+format, v...)
	}
}

// LogInfo 一般信息日誌
func LogInfo(format string, v ...interface{}) {
	if currentLogLevel <= LogLevelInfo {
		logOutput.Printf("[INFO] "+format, v...)
	}
}

// LogError 錯誤日誌
func LogError(format string, v ...interface{}) {
	if currentLogLevel <= LogLevelError {
		logOutput.Printf("[ERROR] "+format, v...)
	}
}

// LogFatal 致命錯誤（總是顯示並退出）
func LogFatal(format string, v ...interface{}) {
	logOutput.Printf("[FATAL] "+format, v...)
	os.Exit(1)
}
