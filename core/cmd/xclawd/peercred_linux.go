//go:build linux

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the OS uid of the connection's peer via SO_PEERCRED. known is
// always true on Linux (the kernel supplies credentials for AF_UNIX sockets).
func peerUID(conn net.Conn) (uid int, known bool, err error) {
	cerr := rawConnControl(conn, func(fd uintptr) error {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			return e
		}
		uid = int(cred.Uid)
		return nil
	})
	if cerr != nil {
		return 0, false, cerr
	}
	return uid, true, nil
}
