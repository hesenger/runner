package main

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// logLines is how many lines each log buffer keeps for the admin UI.
const logLines = 1000

// maxLineLength caps a stored line, so a single huge line cannot hold on to
// much memory.
const maxLineLength = 4096

// subscriberLag is how many lines a subscriber may fall behind before it is
// dropped.
const subscriberLag = 256

type logLine struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// logBuffer keeps the last lines of a log and passes new lines on to
// subscribers. Adding a line never blocks: a subscriber that falls behind is
// dropped and its channel closed, so a slow client cannot hold up an app's
// output. All methods are safe for concurrent use, and add does nothing on a
// nil buffer.
type logBuffer struct {
	mu   sync.Mutex
	size int
	// lines is a ring: once it holds size lines, the oldest one is at next
	lines []logLine
	next  int
	subs  map[chan logLine]struct{}
}

func newLogBuffer(size int) *logBuffer {
	return &logBuffer{size: size, subs: map[chan logLine]struct{}{}}
}

// add appends a line, dropping the oldest one once the buffer is full.
func (b *logBuffer) add(text string) {
	if b == nil {
		return
	}
	if len(text) > maxLineLength {
		text = text[:maxLineLength]
	}
	line := logLine{Time: time.Now(), Text: text}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) < b.size {
		b.lines = append(b.lines, line)
	} else {
		b.lines[b.next] = line
		b.next = (b.next + 1) % b.size
	}
	for ch := range b.subs {
		select {
		case ch <- line:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// Write adds each line of p, so the buffer can be one of runner's log
// outputs. Runner writes whole lines; a last line without a newline is
// added as is.
func (b *logBuffer) Write(p []byte) (int, error) {
	for line := range strings.Lines(string(p)) {
		b.add(strings.TrimRight(line, "\r\n"))
	}
	return len(p), nil
}

// snapshot returns the buffered lines, oldest first.
func (b *logBuffer) snapshot() []logLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

func (b *logBuffer) snapshotLocked() []logLine {
	return append(slices.Clone(b.lines[b.next:]), b.lines[:b.next]...)
}

// subscribe returns the buffered lines and a channel that receives every line
// added after them. The channel is closed when the subscriber falls more than
// subscriberLag lines behind, or by cancel.
func (b *logBuffer) subscribe() ([]logLine, <-chan logLine, func()) {
	ch := make(chan logLine, subscriberLag)

	b.mu.Lock()
	defer b.mu.Unlock()
	replay := b.snapshotLocked()
	b.subs[ch] = struct{}{}

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
	return replay, ch, cancel
}
