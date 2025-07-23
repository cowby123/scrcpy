package main

import (
	"fmt"
	"log"
	"os"
)

type Options struct {
	ShowHelp    bool
	ShowVersion bool
	PauseOnExit string // "true", "false", "if_error"
	LogLevel    string
	UseOTG      bool
	V4L2Device  string
}

const Version = "0.1.0"

func parseArgs(args []string) (*Options, error) {
	opts := &Options{
		PauseOnExit: "false",
		LogLevel:    "info",
	}

	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			opts.ShowHelp = true
		case "--version", "-v":
			opts.ShowVersion = true
		case "--pause":
			opts.PauseOnExit = "true"
		case "--otg":
			opts.UseOTG = true
		case "--debug":
			opts.LogLevel = "debug"
			// 你可以擴充更多參數
		}
	}

	return opts, nil
}

func mainScrcpy(opts *Options) int {
	fmt.Printf("scrcpy-go %s <https://github.com/yourname/scrcpy-go>\n", Version)

	if opts.ShowHelp {
		fmt.Println("Usage: scrcpy-go [options]")
		// 印出更多說明
		return 0
	}

	if opts.ShowVersion {
		fmt.Println("Version:", Version)
		return 0
	}

	// 模擬 net_init()
	if err := initNetwork(); err != nil {
		log.Println("Failed to init network:", err)
		return 1
	}

	// 模擬 log 設定
	configureLog(opts.LogLevel)

	// 模擬 USB / TCP 選擇邏輯
	var exitCode int
	if opts.UseOTG {
		exitCode = runOTG(opts)
	} else {
		exitCode = runScrcpy(opts)
	}

	// 模擬 pause on exit
	if opts.PauseOnExit == "true" || (opts.PauseOnExit == "if_error" && exitCode != 0) {
		fmt.Println("Press Enter to continue...")
		fmt.Scanln()
	}

	return exitCode
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		log.Println("Failed to parse args:", err)
		os.Exit(1)
	}

	code := mainScrcpy(opts)
	os.Exit(code)
}

func initNetwork() error {
	// 預留初始化 socket 等邏輯
	return nil
}

func configureLog(level string) {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Log level:", level)
}

func runScrcpy(opts *Options) int {
	log.Println("Running scrcpy (TCP mode)...")
	// 預留啟動邏輯
	return 0
}

func runOTG(opts *Options) int {
	log.Println("Running scrcpy (OTG mode)...")
	// 預留啟動邏輯
	return 0
}
