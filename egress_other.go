//go:build !linux

package main

// bindSocketToDevice is a no-op on non-Linux platforms (SO_BINDTODEVICE is Linux-specific).
// Egress interface binding on non-Linux falls back to resolving the interface's local IP address.
func bindSocketToDevice(fd uintptr, iface string) error {
	return nil
}
