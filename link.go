package main

import (
	"encoding/binary"
	"log"
	"math/rand"
	"sync"
	"time"
)

const (
	protoMagic = 0x44544831 // "DTH1"
	hdrSize    = 60
	icmpHdrLen = 8
	rxRingCap  = 8192
)

const (
	flagAckOnly = 1 << 0
	flagParity  = 1 << 1
	flagPing    = 1 << 2
	flagFromCli = 1 << 4
)

// Frame is the payload carried inside one ICMP echo packet.
type Frame struct {
	Flags   uint8
	Sess    uint32
	Seq     uint32
	Ack     uint32
	Wnd     uint16
	Holes   [4]uint64
	Block   uint32
	PIdx    uint8
	LinkID  uint8
	Payload []byte
}

func encodeFrame(f *Frame) []byte {
	var h [hdrSize]byte
	binary.BigEndian.PutUint32(h[0:], protoMagic)
	h[4] = 1
	h[5] = f.Flags
	binary.BigEndian.PutUint32(h[6:], f.Sess)
	binary.BigEndian.PutUint32(h[10:], f.Seq)
	binary.BigEndian.PutUint32(h[14:], f.Ack)
	binary.BigEndian.PutUint16(h[18:], f.Wnd)
	binary.BigEndian.PutUint64(h[20:], f.Holes[0])
	binary.BigEndian.PutUint64(h[28:], f.Holes[1])
	binary.BigEndian.PutUint64(h[36:], f.Holes[2])
	binary.BigEndian.PutUint64(h[44:], f.Holes[3])
	binary.BigEndian.PutUint32(h[52:], f.Block)
	h[56] = f.PIdx
	binary.BigEndian.PutUint16(h[57:], uint16(len(f.Payload)))
	h[59] = f.LinkID
	out := make([]byte, 0, hdrSize+len(f.Payload))
	out = append(out, h[:]...)
	out = append(out, f.Payload...)
	return out
}

func decodeFrame(b []byte) (*Frame, bool) {
	if len(b) < hdrSize || binary.BigEndian.Uint32(b[0:]) != protoMagic {
		return nil, false
	}
	n := int(binary.BigEndian.Uint16(b[57:]))
	if hdrSize+n > len(b) {
		return nil, false
	}
	f := &Frame{
		Flags: b[5],
		Sess:  binary.BigEndian.Uint32(b[6:]),
		Seq:   binary.BigEndian.Uint32(b[10:]),
		Ack:   binary.BigEndian.Uint32(b[14:]),
		Wnd:   binary.BigEndian.Uint16(b[18:]),
		Holes: [4]uint64{
			binary.BigEndian.Uint64(b[20:]),
			binary.BigEndian.Uint64(b[28:]),
			binary.BigEndian.Uint64(b[36:]),
			binary.BigEndian.Uint64(b[44:]),
		},
		Block:  binary.BigEndian.Uint32(b[52:]),
		PIdx:   b[56],
		LinkID: b[59],
	}
	if n > 0 {
		f.Payload = b[hdrSize : hdrSize+n]
	}
	return f, true
}

func icmpChecksum(b []byte) uint16 {
	var s uint32
	n := len(b)
	for i := 0; i+1 < n; i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if n%2 == 1 {
		s += uint32(b[n-1]) << 8
	}
	for s>>16 != 0 {
		s = (s & 0xffff) + (s >> 16)
	}
	return ^uint16(s)
}

// Link is a reliable, ordered, FEC-protected byte stream carried over ICMP echo.
type Link struct {
	fd      sockFD
	isCli   bool
	badCk   bool
	linkID  uint8
	sess    uint32
	icmpID  uint16
	icmpSq  uint32
	maxData int
	wndCap  int
	fecK    int
	pps     float64
	verbose bool

	mu   sync.Mutex
	cond *sync.Cond

	dst    [4]byte
	peerUp bool
	rId    uint16
	rSeq   uint16
	rSeen  bool

	txq      *ChunkQueue
	sndNext  uint32
	sndUna   uint32
	ring     map[uint32][]byte
	peerWnd  uint16
	retx     []uint32
	sentAt   map[uint32]time.Time
	dup      int
	pingPps  int
	need     map[uint32]bool
	sacked   map[uint32]bool
	inflight int
	lastOk   time.Time
	lastTx   time.Time
	rto      time.Duration

	lastProg  time.Time
	needAck   bool
	lastAckTx time.Time
	rcvNext   uint32
	maxSeen   uint32
	ooo       map[uint32][]byte
	cache     map[uint32][]byte
	Deliver   *ChunkQueue

	fecBlk  uint32
	fecCnt  int
	fecData [][]byte
	fecLens []uint16
	fecHave []bool
	fecOn   bool

	tokens     float64
	lastRefill time.Time

	dbg              int
	rxAll            uint64
	txBytes, rxBytes uint64
	txPkts, rxPkts   uint64
	retxPkts         uint64
	drops            uint64
}

func NewLink(isCli bool, linkID uint8, maxData, wnd, fecK, pps int, suppress, verbose bool) (*Link, error) {
	fd, err := openRawICMP()
	if err != nil {
		return nil, err
	}
	l := &Link{
		fd:         fd,
		isCli:      isCli,
		badCk:      suppress,
		linkID:     linkID,
		sess:       uint32(time.Now().UnixNano()),
		icmpID:     uint16(time.Now().UnixNano() & 0xffff),
		maxData:    maxData,
		wndCap:     wnd,
		fecK:       fecK,
		fecOn:      fecK > 0,
		pps:        float64(pps),
		verbose:    verbose,
		ring:       make(map[uint32][]byte),
		ooo:        make(map[uint32][]byte),
		cache:      make(map[uint32][]byte),
		txq:        NewChunkQueue(),
		Deliver:    NewChunkQueue(),
		rto:        300 * time.Millisecond,
		sentAt:     make(map[uint32]time.Time),
		need:       make(map[uint32]bool),
		sacked:     make(map[uint32]bool),
		lastOk:     time.Now(),
		lastTx:     time.Now(),
		lastProg:   time.Now(),
		lastRefill: time.Now(),
	}
	if verbose {
		l.dbg = 40
	}
	l.cond = sync.NewCond(&l.mu)
	return l, nil
}

func (l *Link) SetPeer(ip [4]byte) {
	l.mu.Lock()
	l.dst = ip
	l.peerUp = true
	l.mu.Unlock()
}

func (l *Link) wantType() byte {
	if l.isCli {
		return 0 // echo reply
	}
	return 8 // echo request
}

func (l *Link) Start() {
	go l.recvLoop()
	go l.pumpLoop()
}

func (l *Link) Write(p []byte) (int, error) {
	for l.txq.Len() > 4*1024*1024 {
		time.Sleep(time.Millisecond)
	}
	return l.txq.Write(p)
}

func (l *Link) recvLoop() {
	buf := make([]byte, 4096)
	for {
		ic, src, hasSrc, err := rawRecv(l.fd, buf)
		if err != nil {
			if isTemporary(err) {
				continue
			}
			log.Printf("icmp recv: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if len(ic) < icmpHdrLen+hdrSize {
			continue
		}
		if ic[0] != l.wantType() {
			continue
		}
		fr, ok := decodeFrame(ic[icmpHdrLen:])
		if !ok {
			continue
		}
		if fr.LinkID != l.linkID {
			// frame belongs to another tunnel instance on the same host
			continue
		}
		if !l.isCli {
			// mirror the id/seq the client used, so ISP/NAT/cgnat state maps back
			rid := binary.BigEndian.Uint16(ic[4:6])
			rseq := binary.BigEndian.Uint16(ic[6:8])
			l.mu.Lock()
			l.rId, l.rSeq, l.rSeen = rid, rseq, true
			l.mu.Unlock()
		}
		if !l.isCli && fr.Sess != l.sess {
			l.mu.Lock()
			l.resetLocked()
			l.sess = fr.Sess
			l.mu.Unlock()
			if l.verbose {
				log.Printf("new session %d from client", fr.Sess)
			}
		}
		if l.isCli && fr.Flags&flagFromCli != 0 {
			// kernel echo of our own request
			continue
		}
		if !l.isCli && hasSrc {
			l.mu.Lock()
			if !l.peerUp || l.dst != src {
				l.dst = src
				l.peerUp = true
				if l.verbose {
					log.Printf("peer = %v", src)
				}
			}
			l.mu.Unlock()
		}
		fr.Payload = append([]byte(nil), fr.Payload...)
		if l.dbg > 0 {
			l.dbg--
			log.Printf("rx frame: flags=%d sess=%d seq=%d ack=%d wnd=%d len=%d", fr.Flags, fr.Sess, fr.Seq, fr.Ack, fr.Wnd, len(fr.Payload))
		}
		l.onFrame(fr)
	}
}

func (l *Link) sendFrameLocked(f *Frame) {
	if !l.peerUp {
		return
	}
	f.LinkID = l.linkID
	payload := encodeFrame(f)
	pkt := make([]byte, icmpHdrLen+len(payload))
	if l.isCli {
		pkt[0] = 8
	} else {
		pkt[0] = 0
	}
	l.icmpSq++
	if !l.isCli && l.rSeen {
		l.mu.Unlock()
		binary.BigEndian.PutUint16(pkt[4:], l.rId)
		binary.BigEndian.PutUint16(pkt[6:], l.rSeq)
		l.mu.Lock()
	} else {
		binary.BigEndian.PutUint16(pkt[4:], l.icmpID)
		binary.BigEndian.PutUint16(pkt[6:], uint16(l.icmpSq))
	}
	copy(pkt[icmpHdrLen:], payload)
	cs := icmpChecksum(pkt)
	if l.badCk && l.isCli {
		cs ^= 0xa5a5 // invalidate so the remote kernel never answers
	}
	binary.BigEndian.PutUint16(pkt[2:], cs)
	n := l.dup
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		if err := rawSend(l.fd, pkt, l.dst); err != nil {
			l.drops++
			return
		}
		l.txPkts++
		l.txBytes += uint64(len(pkt))
	}
	l.lastTx = time.Now()
}

const minRetxGap = 30 * time.Millisecond

func (l *Link) inRetxLocked(seq uint32) bool {
	for _, s := range l.retx {
		if s == seq {
			return true
		}
	}
	return false
}

func (l *Link) resetLocked() {
	l.sndNext, l.sndUna = 0, 0
	l.rcvNext, l.maxSeen = 0, 0
	l.ring = make(map[uint32][]byte)
	l.ooo = make(map[uint32][]byte)
	l.cache = make(map[uint32][]byte)
	l.retx = nil
	l.sentAt = make(map[uint32]time.Time)
	l.need = make(map[uint32]bool)
	l.sacked = make(map[uint32]bool)
	l.inflight = 0
	l.peerWnd = 0
	l.rto = 300 * time.Millisecond
	l.fecCnt = 0
	l.needAck = true
}

func (l *Link) holesLocked() [4]uint64 {
	var h [4]uint64
	for i := 0; i < 256; i++ {
		seq := l.rcvNext + uint32(i)
		if seq > l.maxSeen {
			break
		}
		if !l.haveLocked(seq) {
			h[i>>6] |= 1 << uint(i&63)
		}
	}
	return h
}

func (l *Link) wndFreeLocked() uint16 {
	free := l.wndCap - len(l.ooo)
	if free < 0 {
		free = 0
	}
	if free > 65535 {
		free = 65535
	}
	return uint16(free)
}

func (l *Link) sendDataLocked(seq uint32, p []byte) {
	fl := uint8(0)
	if l.isCli {
		fl |= flagFromCli
	}
	l.sendFrameLocked(&Frame{
		Flags:   fl,
		Sess:    l.sess,
		Seq:     seq,
		Ack:     l.rcvNext,
		Wnd:     l.wndFreeLocked(),
		Holes:   l.holesLocked(),
		Payload: p,
	})
}

func (l *Link) sendAckLocked() {
	fl := uint8(flagAckOnly)
	if l.isCli {
		fl |= flagFromCli
	}
	l.sendFrameLocked(&Frame{
		Flags: fl,
		Sess:  l.sess,
		Seq:   l.sndNext,
		Ack:   l.rcvNext,
		Wnd:   l.wndFreeLocked(),
		Holes: l.holesLocked(),
	})
}

func (l *Link) outstanding() int {
	if l.inflight < 0 {
		return 0
	}
	return l.inflight
}

func (l *Link) effWnd() int {
	return l.wndCap
}

func (l *Link) pumpLoop() {
	t := time.NewTicker(500 * time.Microsecond)
	defer t.Stop()
	for range t.C {
		l.pump()
	}
}

func (l *Link) pump() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if !l.peerUp {
		return
	}
	if el := now.Sub(l.lastRefill); el > 0 {
		l.tokens += el.Seconds() * l.pps
		capT := l.pps * 0.02
		if capT < 32 {
			capT = 32
		}
		if l.tokens > capT {
			l.tokens = capT
		}
		l.lastRefill = now
	}
	// timeout retransmission
	if l.outstanding() > 0 && now.Sub(l.lastOk) > l.rto {
		n := 0
		for s := l.sndUna; s < l.sndNext && n < 4; s++ {
			p, ok := l.ring[s]
			if !ok {
				continue
			}
			if at, seen := l.sentAt[s]; seen && now.Sub(at) < minRetxGap {
				continue
			}
			l.sendDataLocked(s, p)
			l.sentAt[s] = now
			l.retxPkts++
			n++
		}
		l.lastOk = now
		l.rto *= 2
		if l.rto > 2*time.Second {
			l.rto = 2 * time.Second
		}
	}
	// NACK-driven retransmission
	sent, seen := 0, 0
	for seq := range l.need {
		seen++
		if seen > 256 {
			break
		}
		if seq < l.sndUna {
			delete(l.need, seq) // already acked
			continue
		}
		p, ok := l.ring[seq]
		if !ok {
			delete(l.need, seq)
			continue
		}
		if at, seen := l.sentAt[seq]; seen && now.Sub(at) < minRetxGap {
			continue
		}
		l.sendDataLocked(seq, p)
		l.sentAt[seq] = now
		l.retxPkts++
		l.tokens--
		sent++
		if sent >= 24 || l.tokens < 1 {
			break
		}
	}
	// new data
	wnd := l.effWnd()
	for l.outstanding() < wnd && l.tokens >= 1 {
		p, ok := l.txq.TryTake(l.maxData)
		if !ok {
			break
		}
		s := l.sndNext
		l.sndNext++
		l.ring[s] = p
		l.inflight++
		l.sendDataLocked(s, p)
		l.sentAt[s] = now
		l.tokens--
		if l.fecOn {
			l.fecAccumulateLocked(s, p)
		}
	}
	// rate-limited seq-token pings (keeps fresh ICMP id/seq available for peer replies)
	if l.isCli && l.pingPps > 0 && l.tokens >= 1 {
		gap := time.Second / time.Duration(l.pingPps)
		if now.Sub(l.lastAckTx) >= gap {
			l.sendAckLocked()
			l.lastAckTx = now
			l.needAck = false
			l.tokens--
			if l.tokens < 1 {
				return
			}
		}
	}
	// keepalive / pure ack
	if (l.needAck && now.Sub(l.lastAckTx) > 2*time.Millisecond) || now.Sub(l.lastTx) > 300*time.Millisecond {
		l.sendAckLocked()
		l.lastAckTx = now
		l.needAck = false
	}
}

func (l *Link) fecAccumulateLocked(seq uint32, p []byte) {
	k := l.fecK
	idx := int(seq) % k
	blk := seq / uint32(k)
	if l.fecCnt == 0 || blk != l.fecBlk || idx == 0 {
		l.fecBlk = blk
		l.fecData = make([][]byte, k)
		l.fecLens = make([]uint16, k)
		l.fecHave = make([]bool, k)
		l.fecCnt = 0
	}
	if l.fecHave[idx] {
		return
	}
	l.fecData[idx] = p
	l.fecLens[idx] = uint16(len(p))
	l.fecHave[idx] = true
	l.fecCnt++
	if l.fecCnt < k {
		return
	}
	buf := make([]byte, l.maxData)
	for i := 0; i < k; i++ {
		src := l.fecData[i]
		for j := 0; j < len(src); j++ {
			buf[j] ^= src[j]
		}
	}
	pl := make([]byte, 2*k+len(buf))
	for i := 0; i < k; i++ {
		binary.BigEndian.PutUint16(pl[2*i:], l.fecLens[i])
	}
	copy(pl[2*k:], buf)
	fl := uint8(flagParity)
	if l.isCli {
		fl |= flagFromCli
	}
	l.sendFrameLocked(&Frame{
		Flags:   fl,
		Sess:    l.sess,
		Ack:     l.rcvNext,
		Wnd:     l.wndFreeLocked(),
		Holes:   l.holesLocked(),
		Block:   blk,
		Payload: pl,
	})
	l.fecCnt = 0
}

func (l *Link) haveLocked(seq uint32) bool {
	if seq < l.rcvNext {
		return true
	}
	_, ok := l.ooo[seq]
	return ok
}

func (l *Link) deliverLocked(seq uint32, p []byte) {
	l.Deliver.Write(p)
	l.cache[seq] = p
	l.rxPkts++
	l.rxBytes += uint64(len(p))
}

func (l *Link) onDataLocked(fr *Frame) {
	seq := fr.Seq
	if seq < l.rcvNext {
		return
	}
	if seq == l.rcvNext {
		l.deliverLocked(seq, fr.Payload)
		l.rcvNext++
		for {
			p, ok := l.ooo[l.rcvNext]
			if !ok {
				break
			}
			delete(l.ooo, l.rcvNext)
			l.deliverLocked(l.rcvNext, p)
			l.rcvNext++
		}
		l.lastProg = time.Now()
		// evict old cache entries
		if len(l.cache) > 4096 {
			for s := range l.cache {
				if s+2048 < l.rcvNext {
					delete(l.cache, s)
				}
			}
		}
	} else if len(l.ooo) < rxRingCap {
		if _, ok := l.ooo[seq]; !ok {
			l.ooo[seq] = fr.Payload
		}
	}
	if seq > l.maxSeen {
		l.maxSeen = seq
	}
}

func (l *Link) onParityLocked(fr *Frame) {
	k := l.fecK
	if k <= 0 || len(fr.Payload) < 2*k+1 {
		return
	}
	lens := make([]uint16, k)
	for i := 0; i < k; i++ {
		lens[i] = binary.BigEndian.Uint16(fr.Payload[2*i:])
	}
	par := fr.Payload[2*k:]
	missing := -1
	cnt := 0
	for i := 0; i < k; i++ {
		seq := fr.Block*uint32(k) + uint32(i)
		if !l.haveLocked(seq) {
			cnt++
			missing = i
		}
	}
	if cnt != 1 {
		return
	}
	buf := make([]byte, len(par))
	copy(buf, par)
	for i := 0; i < k; i++ {
		if i == missing {
			continue
		}
		seq := fr.Block*uint32(k) + uint32(i)
		var p []byte
		if seq < l.rcvNext {
			p = l.cache[seq]
		} else {
			p = l.ooo[seq]
		}
		if p == nil {
			return
		}
		for j := 0; j < len(p); j++ {
			buf[j] ^= p[j]
		}
	}
	n := int(lens[missing])
	if n <= 0 || n > len(buf) {
		return
	}
	seq := fr.Block*uint32(k) + uint32(missing)
	l.onDataLocked(&Frame{Seq: seq, Payload: buf[:n]})
}

func (l *Link) onFrame(fr *Frame) {
	l.rxAll++
	l.mu.Lock()
	defer l.mu.Unlock()
	if fr.Sess != l.sess && l.isCli {
		return // stale frame from a previous session does not count as progress
	}
	if fr.Ack > l.sndUna && fr.Ack <= l.sndNext {
		for s := l.sndUna; s < fr.Ack; s++ {
			if _, ok := l.ring[s]; ok {
				delete(l.ring, s)
				if l.sacked[s] {
					delete(l.sacked, s)
				} else {
					l.inflight--
				}
			}
		}
		l.sndUna = fr.Ack
		l.lastOk = time.Now()
		l.lastProg = time.Now()
		l.rto = 300 * time.Millisecond
	}
	if fr.Wnd > 0 {
		l.peerWnd = fr.Wnd
	}
	// Holes bitmap covers [fr.Ack, fr.Ack+256): set bit = missing, clear bit = received.
	for i := 0; i < 256; i++ {
		seq := fr.Ack + uint32(i)
		if seq >= l.sndNext || seq < l.sndUna {
			continue
		}
		if _, ok := l.ring[seq]; !ok {
			continue
		}
		if fr.Holes[i>>6]&(1<<uint(i&63)) != 0 {
			l.need[seq] = true
		} else if !l.sacked[seq] {
			l.sacked[seq] = true
			l.inflight--
			delete(l.need, seq)
		}
	}
	if fr.Flags&flagParity != 0 {
		l.onParityLocked(fr)
	} else if fr.Flags&(flagAckOnly|flagPing) == 0 {
		if len(fr.Payload) > 0 {
			l.onDataLocked(fr)
		}
	}
	l.needAck = true
}

func (l *Link) FramesIn() uint64 { return l.rxAll }

func (l *Link) Stats() (txB, rxB, txP, rxP, retx uint64, sndNext, sndUna, rcvNext uint32, ooo int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.txBytes, l.rxBytes, l.txPkts, l.rxPkts, l.retxPkts, l.sndNext, l.sndUna, l.rcvNext, len(l.ooo)
}

// StalledFor reports how long since the link last made real progress: an ack
// that advanced sndUna, or data that advanced rcvNext. Merely receiving frames
// does not count — a peer that resets its state will happily keep echoing
// frames at us while our ack stays pinned, and treating that as "alive" is what
// let a dead session sit there forever.
func (l *Link) StalledFor() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.lastProg)
}

// HasPending reports whether we are waiting on the peer (data in flight or queued).
func (l *Link) HasPending() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.outstanding() > 0 || l.txq.Len() > 0
}

// NewSession abandons the current session (peer resets its state on the new id).
func (l *Link) NewSession() uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sess = rand.Uint32()
	l.resetLocked()
	l.lastProg = time.Now()
	return l.sess
}
