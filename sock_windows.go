//go:build windows

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// sockFD is a *net.IPConn: on Windows syscall.Sendto/Recvfrom are EWINDOWS
// stubs, while net.ListenIP("ip4:icmp") is a real raw socket. Raw ICMP needs
// an elevated (Administrator) process.
type sockFD = *net.IPConn

func openRawICMP() (sockFD, error) {
	c, err := net.ListenIP("ip4:icmp", &net.IPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, fmt.Errorf("listen raw icmp: %w (raw sockets need Administrator)", err)
	}
	_ = c.SetReadBuffer(8 * 1024 * 1024)
	_ = c.SetWriteBuffer(4 * 1024 * 1024)
	return c, nil
}

// rawRecv already yields the ICMP message: ReadFromIP strips the IPv4 header
// on Windows (net.stripIPv4Header).
func rawRecv(c sockFD, buf []byte) (icmp []byte, src [4]byte, hasSrc bool, err error) {
	n, from, e := c.ReadFromIP(buf)
	if from != nil {
		if ip := from.IP.To4(); ip != nil {
			src, hasSrc = [4]byte{ip[0], ip[1], ip[2], ip[3]}, true
		}
	}
	if e != nil {
		return nil, src, hasSrc, e
	}
	if n < icmpHdrLen {
		return nil, src, hasSrc, nil
	}
	return buf[:n], src, hasSrc, nil
}

func rawSend(c sockFD, pkt []byte, dst [4]byte) error {
	_, err := c.WriteToIP(pkt, &net.IPAddr{IP: net.IPv4(dst[0], dst[1], dst[2], dst[3])})
	return err
}

func isTemporary(err error) bool {
	if err == nil {
		return false
	}
	var ne syscall.Errno
	if errors.As(err, &ne) {
		switch uintptr(ne) {
		// WSAEINTR, WSAEWOULDBLOCK, WSAENETDOWN, WSAENETUNREACH,
		// WSAENETRESET, WSAETIMEDOUT: transient, keep the loop alive.
		case 10004, 10035, 10050, 10051, 10052, 10060:
			return true
		}
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}
