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

func shortName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		if idx := strings.LastIndex(s, "/"); idx >= 0 && idx+1 < len(s) {
			cand := strings.TrimSpace(s[idx+1:])
			if cand != "" {
				s = cand
			}
		}
		if idx := strings.LastIndex(s, "-"); idx >= 0 && len(s)-idx < 40 {
			cand := strings.TrimSpace(s[idx+1:])
			if cand != "" {
				s = cand
			}
		}
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	r := []rune(s)
	if len(r) > 48 {
		s = string(r[:48]) + "…"
	}
	return s
}
