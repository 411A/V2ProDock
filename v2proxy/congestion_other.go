//go:build !linux

package main

// tcpCongestionSupported is Linux-only: xray exposes the knob as a raw
// setsockopt, and the non-Linux build hosts (developer Windows/macOS) are not
// where production dials happen. Reporting false keeps the render honest.
func tcpCongestionSupported(string) bool { return false }
