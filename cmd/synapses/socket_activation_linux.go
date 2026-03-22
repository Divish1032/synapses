//go:build linux

package main

import (
	"net"

	"github.com/coreos/go-systemd/v22/activation"
)

// trySocketActivation attempts to receive a pre-opened listening socket from
// systemd socket activation. Returns nil, nil if not running under systemd
// or if socket activation is not configured.
func trySocketActivation() (net.Listener, error) {
	listeners, err := activation.Listeners()
	if err != nil {
		return nil, nil //nolint:nilerr
	}
	if len(listeners) == 0 {
		return nil, nil
	}
	// Use the first listener (we only configure one socket).
	for i := 1; i < len(listeners); i++ {
		if listeners[i] != nil {
			listeners[i].Close()
		}
	}
	if listeners[0] == nil {
		return nil, nil
	}
	return listeners[0], nil
}
