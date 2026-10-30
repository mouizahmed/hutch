//go:build !linux

package keys

import (
	"errors"
	"net"
)

func listenIn(ns, addr string) (net.Listener, error) {
	return nil, errors.New("boxes run on Linux machines only")
}
