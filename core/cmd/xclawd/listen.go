package main

import (
	"fmt"
	"net"
	"os"
)

// mustListenUnix creates the control-bus Unix socket with owner-only access.
//
// The control bus exposes privileged operations (session.send, secret.inject,
// cron.*, history + the broadcast event stream), so it must not be drivable by
// any local process. Two layers guard it:
//
//   - chmod 0600 — defense in depth. The kernel enforces socket-connect perms on
//     Linux, so a different user is denied connect(); macOS does NOT enforce them,
//     hence the second layer.
//   - peer-credential check (peerCredListener) — authoritative. Every accepted
//     connection's peer OS-uid is read from the kernel and must equal the daemon's
//     effective uid; a cross-uid process is dropped at accept. This does not rely
//     on filesystem perms, so it holds even with the socket in a world-writable
//     /tmp and on platforms that ignore socket perms.
//
// On platforms without peer-cred support (Windows AF_UNIX) the check is skipped
// and the socket relies on filesystem ACLs alone.
func mustListenUnix(path string) net.Listener {
	_ = os.Remove(path) // clear a stale socket
	ln, err := net.Listen("unix", path)
	if err != nil {
		fatal("listen %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		fatal("chmod %s: %v", path, err)
	}
	return &peerCredListener{Listener: ln, allowUID: os.Geteuid()}
}

// peerCredListener rejects any accepted connection whose peer OS-uid is not the
// allowed uid. It fails closed: a peer-cred read error on a platform that should
// support it drops the connection.
type peerCredListener struct {
	net.Listener
	allowUID int
}

func (l *peerCredListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, known, perr := peerUID(conn)
		switch {
		case perr != nil:
			// Platform claims peer-cred support but the read failed — fail closed.
			fmt.Fprintf(os.Stderr, "control: rejecting connection (peer-cred error: %v)\n", perr)
			_ = conn.Close()
			continue
		case known && uid != l.allowUID:
			fmt.Fprintf(os.Stderr, "control: rejecting connection from uid %d (allowed %d)\n", uid, l.allowUID)
			_ = conn.Close()
			continue
		}
		// known == false: peer-cred unsupported on this platform; rely on the
		// socket's filesystem perms. Allow.
		return conn, nil
	}
}

// rawConnControl runs f with the connection's underlying file descriptor. Used
// by the platform-specific peerUID implementations to call getsockopt.
func rawConnControl(conn net.Conn, f func(fd uintptr) error) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("peer-cred: not a unix connection (%T)", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cerr error
	if cerr2 := raw.Control(func(fd uintptr) { cerr = f(fd) }); cerr2 != nil {
		return cerr2
	}
	return cerr
}
