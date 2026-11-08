package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// The self-checks below exercise the parts of the protocol that are easy to get
// subtly wrong and impossible to see from the outside: the frame layout, the
// FEC parity/recovery path, the SACK bitmap polarity, the mux framing and the
// ICMP checksum trick. They run without a raw socket, so `icmptun -mode selftest`
// works as an ordinary user and `go test` covers the same code in CI.
//
// Each check is a name plus a function returning nil on success.

type selfCheck struct {
	name string
	run  func() error
}

// offlineLink builds a Link whose socket is a placeholder and records every
// packet the link would have put on the wire. The globals are restored by the
// returned cleanup function. Checks must therefore run sequentially.
func offlineLink(isCli bool, fecK int, suppress bool) (*Link, *[][]byte, func(), error) {
	prevOpen, prevSend := openRawSocket, sendPacket
	packets := &[][]byte{}
	openRawSocket = func() (sockFD, error) {
		var fd sockFD
		return fd, nil
	}
	sendPacket = func(_ sockFD, pkt []byte, _ [4]byte) error {
		cp := make([]byte, len(pkt))
		copy(cp, pkt)
		*packets = append(*packets, cp)
		return nil
	}
	l, err := NewLink(isCli, 0, 1350, 64, fecK, 1000, suppress, false)
	restore := func() { openRawSocket, sendPacket = prevOpen, prevSend }
	if err != nil {
		restore()
		return nil, nil, func() {}, err
	}
	return l, packets, restore, nil
}

func checkFrameRoundTrip() error {
	want := &Frame{
		Flags:   flagParity | flagFromCli,
		Sess:    0x11223344,
		Seq:     42,
		Ack:     7,
		Wnd:     4096,
		Holes:   [4]uint64{1, 0, 0x8000000000000000, 1 << 63},
		Block:   9,
		PIdx:    3,
		LinkID:  5,
		Payload: []byte("payload-bytes"),
	}
	got, ok := decodeFrame(encodeFrame(want))
	if !ok {
		return fmt.Errorf("decodeFrame rejected a frame it had just encoded")
	}
	if got.Flags != want.Flags || got.Sess != want.Sess || got.Seq != want.Seq ||
		got.Ack != want.Ack || got.Wnd != want.Wnd || got.Block != want.Block ||
		got.PIdx != want.PIdx || got.LinkID != want.LinkID {
		return fmt.Errorf("header fields changed in transit: %+v", got)
	}
	if got.Holes != want.Holes {
		return fmt.Errorf("SACK bitmap changed in transit: %x != %x", got.Holes, want.Holes)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		return fmt.Errorf("payload changed in transit: %q", got.Payload)
	}
	return nil
}

func checkHeaderLayout() error {
	b := encodeFrame(&Frame{LinkID: 0xab, Payload: []byte{1, 2, 3}})
	if len(b) != hdrSize+3 {
		return fmt.Errorf("frame is %d bytes, want %d", len(b), hdrSize+3)
	}
	if got := binary.BigEndian.Uint32(b[0:]); got != protoMagic {
		return fmt.Errorf("magic is %#x, want %#x", got, protoMagic)
	}
	if b[4] != 1 {
		return fmt.Errorf("version byte is %d, want 1", b[4])
	}
	if b[59] != 0xab {
		return fmt.Errorf("link id is at a different offset: got %#x at 59", b[59])
	}
	if n := binary.BigEndian.Uint16(b[57:]); n != 3 {
		return fmt.Errorf("payload length is %d, want 3", n)
	}
	return nil
}

func checkDecoderRejects() error {
	good := encodeFrame(&Frame{Payload: []byte("abcdef")})
	if _, ok := decodeFrame(nil); ok {
		return fmt.Errorf("accepted an empty buffer")
	}
	if _, ok := decodeFrame(good[:hdrSize-1]); ok {
		return fmt.Errorf("accepted a truncated header")
	}
	badMagic := append([]byte(nil), good...)
	badMagic[0] ^= 0xff
	if _, ok := decodeFrame(badMagic); ok {
		return fmt.Errorf("accepted a frame with a corrupted magic")
	}
	if _, ok := decodeFrame(good[:len(good)-1]); ok {
		return fmt.Errorf("accepted a frame whose length field exceeds the buffer")
	}
	return nil
}

func checkFEC() error {
	payloads := [][]byte{
		bytes.Repeat([]byte("a"), 100),
		bytes.Repeat([]byte("b"), 120),
		bytes.Repeat([]byte("c"), 90),
		bytes.Repeat([]byte("d"), 60),
	}
	const k = 4

	// --- sender: four data packets must produce one parity packet ---
	tx, packets, restore, err := offlineLink(true, k, false)
	if err != nil {
		return err
	}
	tx.SetPeer([4]byte{203, 0, 113, 10})
	tx.mu.Lock()
	for i, p := range payloads {
		tx.fecAccumulateLocked(uint32(i), p)
	}
	tx.mu.Unlock()
	restore()

	if len(*packets) != 1 {
		return fmt.Errorf("a full FEC block produced %d packets, want 1", len(*packets))
	}
	fr, ok := decodeFrame((*packets)[0][icmpHdrLen:])
	if !ok {
		return fmt.Errorf("parity packet does not contain a decodable frame")
	}
	if fr.Flags&flagParity == 0 {
		return fmt.Errorf("parity packet is not flagged as parity")
	}
	if fr.Block != 0 {
		return fmt.Errorf("parity packet claims block %d, want 0", fr.Block)
	}
	if want := 2*k + tx.maxData; len(fr.Payload) != want {
		return fmt.Errorf("parity payload is %d bytes, want %d", len(fr.Payload), want)
	}
	want := make([]byte, tx.maxData)
	for i, p := range payloads {
		for j := range p {
			want[j] ^= p[j]
		}
		if got := binary.BigEndian.Uint16(fr.Payload[2*i:]); int(got) != len(p) {
			return fmt.Errorf("length table entry %d is %d, want %d", i, got, len(p))
		}
	}
	if !bytes.Equal(fr.Payload[2*k:], want) {
		return fmt.Errorf("parity payload is not the XOR of the block")
	}

	// --- receiver: lose packet 0, the parity must reconstruct it ---
	rx, _, restore2, err := offlineLink(false, k, false)
	if err != nil {
		return err
	}
	defer restore2()
	rx.mu.Lock()
	for i := 1; i < k; i++ {
		rx.onDataLocked(&Frame{Seq: uint32(i), Payload: payloads[i]})
	}
	rx.onParityLocked(&Frame{Block: 0, Payload: fr.Payload})
	rx.mu.Unlock()

	for i := 0; i < k; i++ {
		got, ok := rx.Deliver.TryTake(1 << 16)
		if !ok {
			return fmt.Errorf("recovery delivered %d of %d packets", i, k)
		}
		if !bytes.Equal(got, payloads[i]) {
			return fmt.Errorf("recovered packet %d differs from the original", i)
		}
	}
	return nil
}

func checkSACKBitmap() error {
	l, _, restore, err := offlineLink(false, 0, false)
	if err != nil {
		return err
	}
	defer restore()

	l.mu.Lock()
	l.rcvNext = 100
	l.maxSeen = 103
	l.ooo[101] = []byte{0}
	h := l.holesLocked()
	l.mu.Unlock()

	// 100 is the next expected packet and 102/103 never arrived; 101 did.
	if want := uint64(1) | 1<<2 | 1<<3; h[0] != want {
		return fmt.Errorf("bitmap word 0 is %#b, want %#b (bit set must mean missing)", h[0], want)
	}
	if h[1] != 0 || h[2] != 0 || h[3] != 0 {
		return fmt.Errorf("bitmap leaked bits outside the advertised range")
	}
	return nil
}

func checkMuxFraming() error {
	l, _, restore, err := offlineLink(true, 0, false)
	if err != nil {
		return err
	}
	defer restore()

	m := NewMux(l, nil, false)
	m.writeFrame(mtData, 0x01020304, []byte("hello"))
	buf, ok := l.txq.TryTake(1 << 16)
	if !ok {
		return fmt.Errorf("mux wrote nothing to the link")
	}
	if len(buf) != 9+5 {
		return fmt.Errorf("mux frame is %d bytes, want 14", len(buf))
	}
	if buf[0] != mtData {
		return fmt.Errorf("mux type is %d, want %d", buf[0], mtData)
	}
	if got := binary.BigEndian.Uint32(buf[1:]); got != 0x01020304 {
		return fmt.Errorf("stream id is %#x", got)
	}
	if got := binary.BigEndian.Uint32(buf[5:]); got != 5 {
		return fmt.Errorf("mux length is %d, want 5", got)
	}
	if string(buf[9:]) != "hello" {
		return fmt.Errorf("mux payload is %q", buf[9:])
	}
	return nil
}

func checkChecksum() error {
	send := func(suppress bool) ([]byte, error) {
		l, packets, restore, err := offlineLink(true, 0, suppress)
		if err != nil {
			return nil, err
		}
		defer restore()
		l.SetPeer([4]byte{203, 0, 113, 10})
		l.mu.Lock()
		l.sendDataLocked(0, []byte("checksum"))
		l.mu.Unlock()
		if len(*packets) != 1 {
			return nil, fmt.Errorf("expected one packet, got %d", len(*packets))
		}
		return (*packets)[0], nil
	}

	good, err := send(false)
	if err != nil {
		return err
	}
	zeroed := append([]byte(nil), good...)
	zeroed[2], zeroed[3] = 0, 0
	if want := icmpChecksum(zeroed); binary.BigEndian.Uint16(good[2:]) != want {
		return fmt.Errorf("checksum is %#x, want %#x", binary.BigEndian.Uint16(good[2:]), want)
	}

	broken, err := send(true)
	if err != nil {
		return err
	}
	zeroed2 := append([]byte(nil), broken...)
	zeroed2[2], zeroed2[3] = 0, 0
	if got, want := binary.BigEndian.Uint16(broken[2:]), icmpChecksum(zeroed2)^0xa5a5; got != want {
		return fmt.Errorf("-suppress produced checksum %#x, want %#x", got, want)
	}
	return nil
}

func checkICMPMirror() error {
	l, packets, restore, err := offlineLink(false, 0, false)
	if err != nil {
		return err
	}
	defer restore()

	l.SetPeer([4]byte{203, 0, 113, 10})
	l.mu.Lock()
	l.rSeen = true
	l.rId, l.rSeq = 0x1234, 0x5678
	l.sendDataLocked(0, []byte("mirror"))
	l.mu.Unlock()

	if len(*packets) != 1 {
		return fmt.Errorf("expected one packet, got %d", len(*packets))
	}
	pkt := (*packets)[0]
	if id := binary.BigEndian.Uint16(pkt[4:]); id != 0x1234 {
		return fmt.Errorf("server invented its own ICMP id %#x instead of mirroring 0x1234", id)
	}
	if seq := binary.BigEndian.Uint16(pkt[6:]); seq != 0x5678 {
		return fmt.Errorf("server invented its own ICMP seq %#x instead of mirroring 0x5678", seq)
	}
	return nil
}

func selfChecks() []selfCheck {
	return []selfCheck{
		{"frame round-trip preserves every field", checkFrameRoundTrip},
		{"header layout: 60 B, magic/version/linkid/len", checkHeaderLayout},
		{"decoder rejects malformed frames", checkDecoderRejects},
		{"FEC: parity emit + single-loss recovery", checkFEC},
		{"SACK bitmap: bit set means missing", checkSACKBitmap},
		{"mux framing: [type][stream][len][data]", checkMuxFraming},
		{"ICMP checksum, valid and suppressed", checkChecksum},
		{"server mirrors the client's ICMP id/seq", checkICMPMirror},
	}
}

// runSelfTests runs every check, prints one line per check and returns a process
// exit code: 0 if all passed, 1 otherwise.
func runSelfTests(w io.Writer) int {
	checks := selfChecks()
	start := time.Now()
	failed := 0
	fmt.Fprintf(w, "icmptun selftest: %d checks, no raw socket needed\n\n", len(checks))
	for i, c := range checks {
		err := c.run()
		if err != nil {
			failed++
			fmt.Fprintf(w, "  FAIL  %d/%d  %s\n              %v\n", i+1, len(checks), c.name, err)
			continue
		}
		fmt.Fprintf(w, "  ok    %d/%d  %s\n", i+1, len(checks), c.name)
	}
	fmt.Fprintf(w, "\n%d passed, %d failed in %s\n", len(checks)-failed, failed,
		time.Since(start).Round(time.Microsecond))
	if failed > 0 {
		return 1
	}
	return 0
}
