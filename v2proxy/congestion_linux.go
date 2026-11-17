//go:build linux

package main

import "syscall"

// tcpCongestionSupported asks the kernel to set the algorithm on a throwaway
// TCP socket. This is deliberately not a /proc/sys/net/ipv4/tcp_available_congestion_control
// read: bbr is normally a loadable module, so it can be absent from that list
// until something requests it. setsockopt itself triggers the module autoload,
// which is exactly what xray's per-socket option will do. If the kernel cannot
// honor the name, xray would fail every dial - so refuse to render it.
func tcpCongestionSupported(name string) bool {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer func() { _ = syscall.Close(fd) }()
	return syscall.SetsockoptString(fd, syscall.IPPROTO_TCP, syscall.TCP_CONGESTION, name) == nil
}
