package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
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
	case "write-env":
		os.WriteFile(os.Getenv("RUNNER_TEST_READY"), []byte(strings.Join(os.Environ(), "\n")), 0644)
		os.Exit(0)
	case "write-cwd":
		cwd, _ := os.Getwd()
		os.WriteFile(os.Getenv("RUNNER_TEST_READY"), []byte(cwd), 0644)
		os.Exit(0)
	case "serve":
		serveChild()
	}
}

// serveChild acts as a deployed app on the port of its last argument
// ("--port N"): /health answers 200, /env?key=NAME prints NAME's value,
// /config prints ./config.txt and /crash exits 1. While RUNNER_TEST_CRASH_FILE exists it exits 1 on start.
func serveChild() {
	if f := os.Getenv("RUNNER_TEST_CRASH_FILE"); f != "" {
		if _, err := os.Stat(f); err == nil {
			os.Exit(1)
		}
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {})
	http.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, os.Getenv(r.URL.Query().Get("key")))
	})
	// /config prints config.txt of the working directory, as read on start
	config, configErr := os.ReadFile("config.txt")
	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if configErr != nil {
			http.NotFound(w, r)
			return
		}
		w.Write(config)
	})
	http.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) { os.Exit(1) })
	http.ListenAndServe("127.0.0.1:"+os.Args[len(os.Args)-1], nil)
	os.Exit(1)
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
	env := map[string]string{"RUNNER_TEST_CHILD": mode, "RUNNER_TEST_READY": ready}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := StartProcess("test", nil, env, "", exe)
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
	if p.Stopping() {
		t.Error("Stopping() = true for a process that exited on its own, want false so runner restarts it")
	}
}

func TestStoppedProcessIsNotUnexpected(t *testing.T) {
	p := startChild(t, "exit-on-term")

	stopWithin(t, p, 10*time.Second, 5*time.Second)
	assertReaped(t, p)
	if !p.Stopping() {
		t.Error("Stopping() = false after Stop, want true so runner does not restart it")
	}
}

func TestStartProcessPassesOnlyTheAppEnv(t *testing.T) {
	// set in runner's environment only, so the child must not see it
	t.Setenv("RUNNER_TEST_SECRET", "runner-only")
	out := filepath.Join(t.TempDir(), "env")
	env := map[string]string{"RUNNER_TEST_CHILD": "write-env", "RUNNER_TEST_READY": out, "APP_GREETING": "a=b c"}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := StartProcess("test", nil, env, "", exe)
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(string(data), "\n")
	slices.Sort(got)
	want := []string{"APP_GREETING=a=b c", "RUNNER_TEST_CHILD=write-env", "RUNNER_TEST_READY=" + out}
	if !slices.Equal(got, want) {
		t.Errorf("child environment %q, want exactly %q", got, want)
	}
}

func TestStartProcessRunsInDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"", t.TempDir()} {
		out := filepath.Join(t.TempDir(), "cwd")
		env := map[string]string{"RUNNER_TEST_CHILD": "write-cwd", "RUNNER_TEST_READY": out}
		p, err := StartProcess("test", nil, env, dir, exe)
		if err != nil {
			t.Fatal(err)
		}
		<-p.Done()
		if p.cmd.Dir != dir {
			t.Errorf("cmd.Dir = %q, want %q", p.cmd.Dir, dir)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		want := dir
		if dir == "" {
			want, _ = os.Getwd()
		}
		// macOS temp dirs are reached through a symlink
		want, _ = filepath.EvalSymlinks(want)
		if gotReal, _ := filepath.EvalSymlinks(string(got)); gotReal != want {
			t.Errorf("child ran in %q, want %q", got, want)
		}
	}
}

func TestStartProcessInDirWithRelativeBinary(t *testing.T) {
	// runner starts bin/<name>-<id>/<binName>, relative to its own directory
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	os.MkdirAll("bin", 0755)
	if err := os.WriteFile("bin/app", data, 0755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "cwd")
	env := map[string]string{"RUNNER_TEST_CHILD": "write-cwd", "RUNNER_TEST_READY": out}
	p, err := StartProcess("test", nil, env, t.TempDir(), "bin/app")
	if err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	if _, err := os.Stat(out); err != nil {
		t.Errorf("relative binary did not run in dir: %v", err)
	}
}
