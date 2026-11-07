package main

import (
	"os"
	"testing"
)

func TestProcessTreeRSSOfSelf(t *testing.T) {
	kb, ok := processTreeRSS(os.Getpid())
	if !ok || kb <= 0 {
		t.Fatalf("processTreeRSS = %d, %v, want the test's own memory", kb, ok)
	}
}

func TestProcessRSSOfSelf(t *testing.T) {
	kb, ok := processRSS(os.Getpid())
	total, _ := processTreeRSS(os.Getpid())
	// the two reads are moments apart and the test's memory moves in
	// between (the GC returns pages, other tests allocate), so allow 10%
	if !ok || kb <= 0 || kb > total+total/10 {
		t.Fatalf("processRSS = %d, %v, want the test's own memory, about at most its tree's %d", kb, ok, total)
	}
}
