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
