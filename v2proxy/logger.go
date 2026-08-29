package main

import (
	"log"
	"os"
	"strings"
)

const (
	levelInfo = iota
	levelDebug
)

var currentLogLevel = levelInfo

func init() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug", "trace", "verbose", "all":
		currentLogLevel = levelDebug
	default:
		currentLogLevel = levelInfo
	}
}

func isDebug() bool {
	return currentLogLevel >= levelDebug
}

func debugLog(format string, v ...any) {
	if currentLogLevel >= levelDebug {
		log.Printf(format, v...)
	}
}
