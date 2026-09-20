//go:build linux

package main

import "syscall"

// bindSocketToDevice binds a socket descriptor to a specific network interface via SO_BINDTODEVICE.
func bindSocketToDevice(fd uintptr, iface string) error {
	if iface == "" {
		return nil
	}
	return syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
}
