//go:build !darwin && !linux

package main

import "net"

// trySocketActivation is a no-op on unsupported platforms.
func trySocketActivation() (net.Listener, error) {
	return nil, nil
}
