// Package logbuf is an in-memory ring buffer of recent log lines with live
// fan-out. It is wired as a second slog destination so the admin UI can tail
// server logs without a file path or external tooling.
package logbuf

import (
	"bytes"
	"sync"
)

// Buffer keeps the last N log lines and fans new lines out to subscribers.
type Buffer struct {
	mu    sync.Mutex
	max   int
	lines []string
	subs  map[int]chan string
	next  int
}

// New builds a buffer retaining up to max recent lines.
func New(max int) *Buffer {
	return &Buffer{max: max, subs: make(map[int]chan string)}
}

// Write implements io.Writer: each call (one slog record) is stored as a line
// and fanned out. Non-blocking sends drop for lagging subscribers.
func (b *Buffer) Write(p []byte) (int, error) {
	line := string(bytes.TrimRight(p, "\n"))
	b.mu.Lock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- line:
		default:
		}
	}
	b.mu.Unlock()
	return len(p), nil
}

// Recent returns a snapshot of the retained lines.
func (b *Buffer) Recent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

// Subscribe returns a channel of new lines plus a cancel func.
func (b *Buffer) Subscribe() (<-chan string, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan string, 256)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}
