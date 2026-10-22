package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionFlagPrintsInjectedVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "runner")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=1.2.3", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1.2.3" {
		t.Errorf("--version printed %q, want %q", got, "1.2.3")
	}
}
