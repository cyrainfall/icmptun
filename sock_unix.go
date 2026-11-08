//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// sockFD is the raw socket descriptor type. On Unix it is a plain int; on
// Windows it is a *net.IPConn, because syscall.Sendto/Recvfrom are EWINDOWS
// stubs there. All raw-ICMP traffic therefore goes through this file's
// helpers, which also normalise the received data to an ICMP-only slice.
type sockFD = int

const ipProtoICMP = 1

func openRawICMP() (sockFD, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, ipProtoICMP)
	if err != nil {
		return 0, err
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 8*1024*1024)
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4*1024*1024)
	return fd, nil
}

// rawRecv returns only the ICMP message: Unix raw sockets hand back the IPv4
// header too, so it is skipped here.
func rawRecv(fd sockFD, buf []byte) (icmp []byte, src [4]byte, hasSrc bool, err error) {
	n, from, e := syscall.Recvfrom(fd, buf, 0)
	if sa, ok := from.(*syscall.SockaddrInet4); ok {
		src, hasSrc = sa.Addr, true
	}
	if e != nil {
		return nil, src, hasSrc, e
	}
	if n < 20 {
		return nil, src, hasSrc, nil
	}
	ihl := int(buf[0]&0x0f) * 4
	if ihl < 20 || ihl > n {
		return nil, src, hasSrc, nil
	}
	return buf[ihl:n], src, hasSrc, nil
}

// rawSend expects only the ICMP message; the kernel adds the IPv4 header.
func rawSend(fd sockFD, pkt []byte, dst [4]byte) error {
	return syscall.Sendto(fd, pkt, 0, &syscall.SockaddrInet4{Addr: dst})
}

func isTemporary(err error) bool {
	return errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN)
}
