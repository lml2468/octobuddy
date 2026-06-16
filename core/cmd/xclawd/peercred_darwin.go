//go:build darwin

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the OS uid of the connection's peer via LOCAL_PEERCRED. known
// is always true on Darwin (the kernel supplies credentials for AF_UNIX sockets).
func peerUID(conn net.Conn) (uid int, known bool, err error) {
	cerr := rawConnControl(conn, func(fd uintptr) error {
		xu, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			return e
		}
		uid = int(xu.Uid)
		return nil
	})
	if cerr != nil {
		return 0, false, cerr
	}
	return uid, true, nil
}
