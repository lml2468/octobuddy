//go:build !linux && !darwin

package main

import "net"

// peerUID is unsupported on this platform (e.g. Windows AF_UNIX, which exposes
// no peer credentials). Returning known=false tells the listener to skip the
// uid check and fall back to the socket's filesystem permissions.
func peerUID(conn net.Conn) (uid int, known bool, err error) {
	return 0, false, nil
}
