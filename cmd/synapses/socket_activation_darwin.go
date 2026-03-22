//go:build darwin

package main

import (
	"net"

	"github.com/tprasadtp/go-launchd"
)

// socketActivationName is the launchd Sockets dictionary key used in the plist.
const socketActivationName = "SynapsesHTTP"

// trySocketActivation attempts to receive a pre-opened listening socket from
// launchd socket activation. Returns nil, nil if not running under launchd
// or if socket activation is not configured.
func trySocketActivation() (net.Listener, error) {
	listeners, err := launchd.Listeners(socketActivationName)
	if err != nil {
		// ESRCH = not managed by launchd, ENOENT = socket name not found.
		// Both are expected when running without socket activation.
		return nil, nil //nolint:nilerr
	}
	if len(listeners) == 0 {
		return nil, nil
	}
	// Use the first listener (we only configure one socket).
	// Close any extras (shouldn't happen with our config).
	for i := 1; i < len(listeners); i++ {
		listeners[i].Close()
	}
	return listeners[0], nil
}
