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
	if !ok || kb <= 0 || kb > total {
		t.Fatalf("processRSS = %d, %v, want the test's own memory, at most its tree's %d", kb, ok, total)
	}
}
