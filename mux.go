package main

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"os"
	"log"
)

const (
	mtOpen   = 1
	mtData   = 2
	mtClose  = 3
	mtCredit = 4

	creditWindow = 1 << 40
	maxMuxFrame  = 1 << 20
)

// Mux multiplexes several TCP streams onto one Link.
type Mux struct {
	link    *Link
	dialFn  func(string) (net.Conn, error)
	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	verbose bool
}

func NewMux(l *Link, dialFn func(string) (net.Conn, error), verbose bool) *Mux {
	return &Mux{
		link:    l,
		dialFn:  dialFn,
		streams: make(map[uint32]*Stream),
		verbose: verbose,
	}
}

func (m *Mux) Start() {
	go m.run()
}

func (m *Mux) writeFrame(typ uint8, id uint32, data []byte) {
	buf := make([]byte, 9+len(data))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:], id)
	binary.BigEndian.PutUint32(buf[5:], uint32(len(data)))
	copy(buf[9:], data)
	m.link.Write(buf)
}

func (m *Mux) run() {
	var h [9]byte
	r := m.link.Deliver
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return
		}
		typ := h[0]
		id := binary.BigEndian.Uint32(h[1:])
		n := binary.BigEndian.Uint32(h[5:])
		if n > maxMuxFrame {
			log.Printf("mux: invalid frame type=%d len=%d (session desync)", typ, n)
			os.Exit(4)
		}
		var data []byte
		if n > 0 {
			data = make([]byte, n)
			if _, err := io.ReadFull(r, data); err != nil {
				return
			}
		}
		switch typ {
		case mtOpen:
			m.handleOpen(id, string(data))
		case mtData:
			m.mu.Lock()
			s := m.streams[id]
			m.mu.Unlock()
			if s != nil {
				s.push(data)
			}
		case mtClose:
			m.mu.Lock()
			s := m.streams[id]
			m.mu.Unlock()
			if s != nil {
				s.shutdown()
			}
		case mtCredit:
			if len(data) >= 4 {
				m.mu.Lock()
				s := m.streams[id]
				m.mu.Unlock()
				if s != nil {
					s.addCredit(int(binary.BigEndian.Uint32(data)))
				}
			}
		}
	}
}

func (m *Mux) handleOpen(id uint32, addr string) {
	if m.dialFn == nil {
		return
	}
	conn, err := m.dialFn(addr)
	if err != nil {
		if m.verbose {
			log.Printf("dial %s: %v", addr, err)
		}
		m.writeFrame(mtClose, id, nil)
		return
	}
	s := m.newStream(id, conn)
	if m.verbose {
		log.Printf("open #%d -> %s", id, addr)
	}
	_ = s
}

func (m *Mux) dbg(format string, a ...any) {
	if m.verbose {
		log.Printf(format, a...)
	}
}

func (m *Mux) newStream(id uint32, conn net.Conn) *Stream {
	s := &Stream{
		id:     id,
		m:      m,
		conn:   conn,
		ch:     make(chan []byte, 1024),
		done:   make(chan struct{}),
		credit: creditWindow,
	}
	s.cond = sync.NewCond(&s.mu)
	m.mu.Lock()
	m.streams[id] = s
	m.mu.Unlock()
	go s.reader()
	go s.writer()
	return s
}

// Open registers a client-side stream bound to an already established local conn.
func (m *Mux) Open(addr string, conn net.Conn) *Stream {
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	m.mu.Unlock()
	m.writeFrame(mtOpen, id, []byte(addr))
	return m.newStream(id, conn)
}

type Stream struct {
	id     uint32
	m      *Mux
	conn   net.Conn
	ch     chan []byte
	done   chan struct{}
	mu     sync.Mutex
	cond   *sync.Cond
	credit int
	closed bool
	once   sync.Once
}

func (s *Stream) push(b []byte) {
	if len(b) == 0 {
		return
	}
	select {
	case s.ch <- b:
	case <-s.done:
	}
}

func (s *Stream) addCredit(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.credit += n
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *Stream) waitCredit(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed && s.credit < n {
		s.cond.Wait()
	}
	if s.closed {
		return false
	}
	s.credit -= n
	return true
}

func (s *Stream) reader() {
	buf := make([]byte, 8192)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			s.m.dbg("s%d READ %d", s.id, n)
			if !s.waitCredit(n) {
				s.m.dbg("s%d credit-wait fail", s.id)
				return
			}
			s.m.writeFrame(mtData, s.id, buf[:n])
			s.m.dbg("s%d SENT %d", s.id, n)
		}
		if err != nil {
			s.closeFromLocal()
			return
		}
	}
}

func (s *Stream) writer() {
	for {
		select {
		case <-s.done:
			return
		case b := <-s.ch:
			s.m.dbg("s%d WRITE %d", s.id, len(b))
			if _, err := s.conn.Write(b); err != nil {
				s.closeFromLocal()
				return
			}
			var c [4]byte
			binary.BigEndian.PutUint32(c[:], uint32(len(b)))
			s.m.writeFrame(mtCredit, s.id, c[:])
		}
	}
}

func (s *Stream) closeFromLocal() {
	s.once.Do(func() {
		s.m.writeFrame(mtClose, s.id, nil)
		s.m.mu.Lock()
		delete(s.m.streams, s.id)
		s.m.mu.Unlock()
		s.conn.Close()
		s.shutdown()
	})
}

func (s *Stream) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.cond.Broadcast()
	close(s.done)
	s.m.mu.Lock()
	delete(s.m.streams, s.id)
	s.m.mu.Unlock()
	s.conn.Close()
}

// Active returns the number of open streams.
func (m *Mux) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// CloseAll tears down every open stream (used when the link is reset).
func (m *Mux) CloseAll() int {
	m.mu.Lock()
	list := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		s.shutdown()
	}
	return len(list)
}
