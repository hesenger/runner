package main

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary act as the child process: with
// RUNNER_TEST_CHILD set it runs that child mode instead of the tests.
func TestMain(m *testing.M) {
	switch os.Getenv("RUNNER_TEST_CHILD") {
	case "":
		os.Exit(m.Run())
	case "exit-on-term":
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		childReady()
		<-c
		os.Exit(0)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		childReady()
		time.Sleep(time.Minute)
		os.Exit(0)
	case "exit-3":
		os.Exit(3)
	}
}

// childReady creates RUNNER_TEST_READY once the child's signal handling is set
// up, so the test does not send SIGTERM before that.
func childReady() {
	os.WriteFile(os.Getenv("RUNNER_TEST_READY"), nil, 0644)
}

// startChild runs the test binary in the given child mode and waits until it is ready.
func startChild(t *testing.T, mode string) *Process {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("RUNNER_TEST_CHILD", mode)
	t.Setenv("RUNNER_TEST_READY", ready)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := StartProcess("test", exe)
	if err != nil {
		t.Fatal(err)
	}
	// never leave a child behind, even when the test fails
	t.Cleanup(func() { p.Stop(time.Millisecond) })

	if mode == "exit-3" {
		return p
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %q not ready within 5s", mode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stopWithin calls Stop and fails the test if it does not return within limit.
func stopWithin(t *testing.T, p *Process, timeout, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	returned := make(chan struct{})
	go func() {
		p.Stop(timeout)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(limit):
		t.Fatalf("Stop(%s) did not return within %s", timeout, limit)
	}
	return time.Since(start)
}

// assertReaped checks Wait has completed, so the child is not left a zombie.
func assertReaped(t *testing.T, p *Process) *os.ProcessState {
	t.Helper()
	select {
	case <-p.Done():
	default:
		t.Fatal("process was not reaped")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("process has no ProcessState, Wait was not called")
	}
	return p.cmd.ProcessState
}

func TestStopExitsOnSIGTERM(t *testing.T) {
	p := startChild(t, "exit-on-term")

	elapsed := stopWithin(t, p, 10*time.Second, 5*time.Second)
	if elapsed > 2*time.Second {
		t.Errorf("stopped after %s, want well under the 10s stop timeout", elapsed)
	}
	state := assertReaped(t, p)
	if state.ExitCode() != 0 {
		t.Errorf("child exited with %s, want exit status 0 from its SIGTERM handler", state)
	}
}

func TestStopKillsChildIgnoringSIGTERM(t *testing.T) {
	p := startChild(t, "ignore-term")

	timeout := 500 * time.Millisecond
	elapsed := stopWithin(t, p, timeout, 5*time.Second)
	if elapsed < timeout {
		t.Errorf("stopped after %s, want at least the %s stop timeout", elapsed, timeout)
	}
	state := assertReaped(t, p)
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("child ended with %s, want killed by SIGKILL", state)
	}
}

func TestProcessReapedWhenItExitsOnItsOwn(t *testing.T) {
	p := startChild(t, "exit-3")

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process that exited on its own was not reaped within 5s")
	}
	if state := assertReaped(t, p); state.ExitCode() != 3 {
		t.Errorf("child exited with %s, want exit status 3", state)
	}
}
