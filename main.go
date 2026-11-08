package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	mode := flag.String("mode", "client", "client|server|selftest")
	peer := flag.String("peer", "", "peer IPv4 address (client only)")
	socks := flag.String("socks", "127.0.0.1:1080", "local SOCKS5 listener (client only)")
	httpAddr := flag.String("http", "", "local HTTP proxy listener (client only, e.g. 127.0.0.1:8118)")
	fwd := flag.String("L", "", "extra port forwards lport:host:rport,... (client only)")
	wnd := flag.Int("wnd", 512, "receive window in packets")
	fecK := flag.Int("fec", 8, "FEC block size, 0 disables")
	pps := flag.Int("pps", 4000, "max ICMP packets per second")
	payload := flag.Int("payload", 1300, "data bytes per packet")
	dupN := flag.Int("dup", 1, "send each packet N times (loss mitigation)")
	pingPps := flag.Int("ping", 0, "client: extra empty frames per second to refresh mirrored icmp seq")
	watchdog := flag.Int("watchdog", 15, "client: reset session after N seconds stalled with work pending (0=off)")
	suppress := flag.Bool("suppress", true, "client uses invalid ICMP checksum to suppress remote kernel echo")
	linkID := flag.Int("linkid", 0, "tunnel lane id 0-255 (lets several tunnels share one server host)")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// -mode selftest runs the protocol checks and exits; it never opens a socket,
	// so it works as an ordinary user and is safe to run in CI.
	if *mode == "selftest" {
		os.Exit(runSelfTests(os.Stdout))
	}

	var dialFn func(string) (net.Conn, error)
	if *mode == "server" {
		dialFn = func(addr string) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, 10*time.Second)
		}
	}

	link, err := NewLink(*mode == "client", uint8(*linkID), *payload, *wnd, *fecK, *pps, *suppress, *verbose)
	if err == nil {
		link.dup = *dupN
		link.pingPps = *pingPps
	}
	if err != nil {
		log.Fatalf("open raw socket: %v (need root)", err)
	}

	if *mode == "client" {
		ip, err := parseIPv4(*peer)
		if err != nil {
			log.Fatalf("bad -peer: %v", err)
		}
		link.SetPeer(ip)
	}
	link.Start()
	mux := NewMux(link, dialFn, *verbose)
	mux.Start()

	if *mode == "client" {
		ln, err := net.Listen("tcp", *socks)
		if err != nil {
			log.Fatalf("socks listen: %v", err)
		}
		log.Printf("socks5 on %s -> %s via icmp (payload=%d wnd=%d fec=%d pps=%d suppress=%v)",
			*socks, *peer, *payload, *wnd, *fecK, *pps, *suppress)
		go serveSOCKS(ln, mux)
		if *httpAddr != "" {
			hl, err := net.Listen("tcp", *httpAddr)
			if err != nil {
				log.Fatalf("http listen: %v", err)
			}
			log.Printf("http proxy on %s", *httpAddr)
			go serveHTTPProxy(hl, mux)
		}
		if *fwd != "" {
			for _, spec := range strings.Split(*fwd, ",") {
				spec = strings.TrimSpace(spec)
				if spec == "" {
					continue
				}
				parts := strings.Split(spec, ":")
				if len(parts) != 3 {
					log.Printf("bad -L %q", spec)
					continue
				}
				lp, err1 := strconv.Atoi(parts[0])
				rp, err2 := strconv.Atoi(parts[2])
				if err1 != nil || err2 != nil {
					log.Printf("bad -L %q", spec)
					continue
				}
				addr := net.JoinHostPort(parts[1], strconv.Itoa(rp))
				fl, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(lp)))
				if err != nil {
					log.Printf("forward listen %d: %v", lp, err)
					continue
				}
				log.Printf("forward 127.0.0.1:%d -> %s", lp, addr)
				go serveForward(fl, mux, addr)
			}
		}
	} else {
		log.Printf("server ready (payload=%d wnd=%d fec=%d suppress=%v)", *payload, *wnd, *fecK, *suppress)
	}

	if *verbose {
		go func() {
			var lastT, lastR, lastP, lastX, lastF uint64
			for range time.Tick(5 * time.Second) {
				txB, rxB, txP, rxP, retx, sndNext, sndUna, rcvNext, ooo := link.Stats()
				_ = rxP
				fin := link.FramesIn()
				fmt.Fprintf(os.Stderr, "stats: tx %.0f KB/s rx %.0f KB/s txpkt %.0f/s rxpkt %.0f/s rxfrm %.0f/s retx %d inflight %d acked %d ooo %d\n",
					float64(txB-lastT)/5/1024, float64(rxB-lastR)/5/1024,
					float64(txP-lastP)/5, float64(rxP-lastX)/5, float64(fin-lastF)/5,
					retx, sndNext-sndUna, sndUna, ooo)
				lastF = fin
				lastT, lastR, lastP, lastX = txB, rxB, txP, rxP
				_ = rcvNext
			}
		}()
	}

	if *mode == "client" && *watchdog > 0 {
		wd := time.Duration(*watchdog) * time.Second
		go func() {
			for range time.Tick(time.Second) {
				if mux.Active() == 0 && !link.HasPending() {
					continue
				}
				if d := link.StalledFor(); d >= wd {
					log.Printf("watchdog: stalled %v (streams=%d) -> exiting for supervisor restart", d.Truncate(time.Second), mux.Active())
					os.Exit(3)
				}
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
}

func parseIPv4(s string) ([4]byte, error) {
	var out [4]byte
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return out, fmt.Errorf("not an IPv4 address: %q", s)
	}
	copy(out[:], ip)
	return out, nil
}

func serveForward(ln net.Listener, mux *Mux, addr string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go mux.Open(addr, c)
	}
}

func serveSOCKS(ln net.Listener, mux *Mux) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleSOCKS(c, mux)
	}
}

func handleSOCKS(c net.Conn, mux *Mux) {
	c.SetDeadline(time.Now().Add(20 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 5 {
		c.Close()
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		c.Close()
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		c.Close()
		return
	}
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 5 {
		c.Close()
		return
	}
	if req[1] != 1 { // CONNECT only
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		c.Close()
		return
	}
	var host string
	switch req[3] {
	case 1:
		var a [4]byte
		if _, err := io.ReadFull(c, a[:]); err != nil {
			c.Close()
			return
		}
		host = net.IP(a[:]).String()
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			c.Close()
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			c.Close()
			return
		}
		host = string(b)
	case 4:
		var a [16]byte
		if _, err := io.ReadFull(c, a[:]); err != nil {
			c.Close()
			return
		}
		host = net.IP(a[:]).String()
	default:
		c.Close()
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		c.Close()
		return
	}
	addr := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb[:]))))
	c.SetDeadline(time.Time{})
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		c.Close()
		return
	}
	mux.Open(addr, c)
}

type bufConn struct {
	net.Conn
	r io.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func serveHTTPProxy(ln net.Listener, mux *Mux) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleHTTP(c, mux)
	}
}

func handleHTTP(c net.Conn, mux *Mux) {
	c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		c.Close()
		return
	}
	if req.Method == http.MethodConnect {
		addr := req.Host
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "443")
		}
		c.SetDeadline(time.Time{})
		if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			c.Close()
			return
		}
		mux.Open(addr, &bufConn{Conn: c, r: br})
		return
	}
	// plain HTTP: absolute-form request -> origin-form over the tunnel
	addr := req.URL.Host
	if addr == "" {
		addr = req.Host
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "80")
	}
	var hb bytes.Buffer
	if err := req.Write(&hb); err != nil {
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	mux.Open(addr, &bufConn{Conn: c, r: io.MultiReader(bytes.NewReader(hb.Bytes()), br)})
}
