package main

import (
	"io"
	"sync"
)

// ChunkQueue is a byte FIFO with blocking Take/TryTake and io.Reader support.
type ChunkQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	chunks [][]byte
	closed bool
}

func NewChunkQueue() *ChunkQueue {
	q := &ChunkQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *ChunkQueue) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c := make([]byte, len(p))
	copy(c, p)
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	q.chunks = append(q.chunks, c)
	q.mu.Unlock()
	q.cond.Signal()
	return len(p), nil
}

func (q *ChunkQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// Take blocks until data is available and returns up to max bytes.
func (q *ChunkQueue) Take(max int) []byte {
	q.mu.Lock()
	for len(q.chunks) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.chunks) == 0 {
		q.mu.Unlock()
		return nil
	}
	c := q.chunks[0]
	if len(c) <= max {
		q.chunks = q.chunks[1:]
		q.mu.Unlock()
		return c
	}
	out := c[:max]
	q.chunks[0] = c[max:]
	q.mu.Unlock()
	return out
}

// TryTake returns immediately; ok=false when the queue is empty.
func (q *ChunkQueue) TryTake(max int) ([]byte, bool) {
	q.mu.Lock()
	if len(q.chunks) == 0 {
		q.mu.Unlock()
		return nil, false
	}
	c := q.chunks[0]
	if len(c) <= max {
		q.chunks = q.chunks[1:]
		q.mu.Unlock()
		return c, true
	}
	out := c[:max]
	q.chunks[0] = c[max:]
	q.mu.Unlock()
	return out, true
}

func (q *ChunkQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, c := range q.chunks {
		n += len(c)
	}
	return n
}

// Read implements io.Reader.
func (q *ChunkQueue) Read(p []byte) (int, error) {
	b := q.Take(len(p))
	if b == nil {
		return 0, io.EOF
	}
	copy(p, b)
	return len(b), nil
}
