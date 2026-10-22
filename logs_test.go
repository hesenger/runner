package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// texts returns the text of each line.
func texts(lines []logLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

func TestLogBufferKeepsLastLines(t *testing.T) {
	b := newLogBuffer(3)
	if got := b.snapshot(); len(got) != 0 {
		t.Fatalf("new buffer holds %v", texts(got))
	}

	b.add("1")
	b.add("2")
	if got := strings.Join(texts(b.snapshot()), ","); got != "1,2" {
		t.Fatalf("got %q, want 1,2", got)
	}

	// wraps around twice: only the last 3 lines are kept, oldest first
	for i := 3; i <= 7; i++ {
		b.add(fmt.Sprint(i))
	}
	if got := strings.Join(texts(b.snapshot()), ","); got != "5,6,7" {
		t.Fatalf("got %q, want 5,6,7", got)
	}
}

func TestLogBufferWriteSplitsLines(t *testing.T) {
	b := newLogBuffer(10)
	fmt.Fprintf(b, "[MAIN] one\n[MAIN] two\r\n")
	b.Write([]byte("no newline"))
	if got := strings.Join(texts(b.snapshot()), "|"); got != "[MAIN] one|[MAIN] two|no newline" {
		t.Fatalf("got %q", got)
	}
}

func TestLogBufferCapsLineLength(t *testing.T) {
	b := newLogBuffer(1)
	b.add(strings.Repeat("x", maxLineLength+10))
	if got := len(b.snapshot()[0].Text); got != maxLineLength {
		t.Fatalf("stored line has %d bytes, want %d", got, maxLineLength)
	}
}

func TestNilLogBufferIgnoresLines(t *testing.T) {
	var b *logBuffer
	b.add("ignored")
}

func TestLogBufferSubscriber(t *testing.T) {
	b := newLogBuffer(2)
	b.add("old")
	b.add("before")

	replay, lines, cancel := b.subscribe()
	if got := strings.Join(texts(replay), ","); got != "old,before" {
		t.Fatalf("replay %q, want old,before", got)
	}

	b.add("after")
	if l := <-lines; l.Text != "after" {
		t.Fatalf("subscriber got %q, want after", l.Text)
	}

	cancel()
	if _, ok := <-lines; ok {
		t.Fatal("channel still open after cancel")
	}
	// cancelling twice and adding after cancel must not panic
	cancel()
	b.add("later")
}

func TestLogBufferDropsSlowSubscriber(t *testing.T) {
	b := newLogBuffer(10)
	_, slow, cancelSlow := b.subscribe()
	defer cancelSlow()
	_, fast, cancelFast := b.subscribe()
	defer cancelFast()

	// fast reads each line as it is added, so it never falls behind; slow
	// reads nothing and falls behind
	for i := 0; i < subscriberLag+5; i++ {
		b.add(fmt.Sprint(i))
		if l, ok := <-fast; !ok || l.Text != fmt.Sprint(i) {
			t.Fatalf("fast subscriber got %q (open %v), want %d", l.Text, ok, i)
		}
	}

	// slow got the lines that fit, then its channel was closed
	n := 0
	for range slow {
		n++
	}
	if n != subscriberLag {
		t.Fatalf("slow subscriber got %d lines before being dropped, want %d", n, subscriberLag)
	}
}

func TestLogBufferConcurrentUse(t *testing.T) {
	b := newLogBuffer(50)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			for i := 0; i < 200; i++ {
				b.add("line")
			}
		})
		wg.Go(func() {
			_, lines, cancel := b.subscribe()
			defer cancel()
			// the writers may be done already, so do not wait for long
			for i := 0; i < 10; i++ {
				select {
				case _, ok := <-lines:
					if !ok {
						return
					}
				case <-time.After(100 * time.Millisecond):
					return
				}
			}
		})
	}
	wg.Wait()
	if got := len(b.snapshot()); got != 50 {
		t.Fatalf("buffer holds %d lines, want 50", got)
	}
}
